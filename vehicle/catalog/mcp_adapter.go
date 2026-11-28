package catalog

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kirin7002/app_personal_agent/contract"
)

var sensitiveFieldNames = map[string]bool{
	"runtime":           true,
	"devicekey":         true,
	"device_key":        true,
	"vin":               true,
	"turn_handle":       true,
	"hermes_session_id": true,
	"user_id":           true,
	"session_id":        true,
	"vehicle_id":        true,
	"scope":             true,
	"request_id":        true,
	"_meta":             true,
}

// IsSensitiveFieldName reports whether a field name carries runtime or vehicle identity
// context that must be stripped from LLM-facing schemas and injected only by Go.
func IsSensitiveFieldName(name string) bool {
	return sensitiveFieldNames[strings.ToLower(strings.TrimSpace(name))]
}

// StripSensitiveFieldsFromSchema removes sensitive identity/runtime properties from
// an arbitrary JSON Schema object (used when importing upstream MCP schemas).
func StripSensitiveFieldsFromSchema(raw json.RawMessage) (json.RawMessage, error) {
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil, err
	}
	stripSensitiveSchemaMap(schema)
	return json.Marshal(schema)
}

func stripSensitiveSchemaMap(obj map[string]any) {
	if props, ok := obj["properties"].(map[string]any); ok {
		for k, v := range props {
			if IsSensitiveFieldName(k) {
				delete(props, k)
				continue
			}
			if child, ok := v.(map[string]any); ok {
				stripSensitiveSchemaMap(child)
			}
		}
	}
	if req, ok := obj["required"].([]any); ok {
		filtered := make([]any, 0, len(req))
		for _, item := range req {
			if s, ok := item.(string); ok && !IsSensitiveFieldName(s) {
				filtered = append(filtered, s)
			}
		}
		obj["required"] = filtered
	}
}

// DownstreamMCPCall is the concrete business MCP invocation assembled by Go after
// validating canonical LLM arguments and injecting trusted vehicle identity server-side.
type DownstreamMCPCall struct {
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments"`
}

// BuildDownstreamCall validates LLM-supplied params against the vehicle catalog and
// builds the concrete downstream MCP tool payload, injecting trusted deviceKey only
// when the downstream spec declares it and never accepting identity fields from LLM input.
func (s *Service) BuildDownstreamCall(
	scope contract.Scope,
	op contract.Operation,
	params map[string]any,
	trustedDeviceKey string,
) (DownstreamMCPCall, error) {
	if !scope.Valid() {
		return DownstreamMCPCall{}, &contract.BusinessError{
			Code:    contract.TurnContextInvalid,
			Message: "invalid scope",
		}
	}
	outcome, spec, err := s.ValidateMutation(scope.VehicleID, op, params, true)
	if err != nil {
		return DownstreamMCPCall{}, err
	}

	toolName := spec.DownstreamTool
	if toolName == "" {
		toolName = spec.Operation.Tool
	}
	args := make(map[string]any, len(outcome.Normalized)+1)
	for k, v := range outcome.Normalized {
		args[k] = v
	}

	// Inject trusted deviceKey strictly server-side if declared by downstream spec.
	for _, f := range spec.Fields {
		if strings.EqualFold(f.Name, "deviceKey") && strings.TrimSpace(trustedDeviceKey) != "" {
			args[f.Name] = strings.TrimSpace(trustedDeviceKey)
		}
	}
	return DownstreamMCPCall{
		Tool:      toolName,
		Arguments: args,
	}, nil
}

// ResolveTimeInput mirrors the natural-language relative/periodic time parameters
// accepted by the local resolve_time_tool before calling timed/prep vehicle tools.
type ResolveTimeInput struct {
	TimeStr         string   `json:"time_str,omitempty"`         // e.g. "08:00" or "8:30"
	TimePeriod      string   `json:"time_period,omitempty"`      // "早上"|"上午"|"中午"|"下午"|"晚上"
	RelativeDays    int      `json:"relative_days,omitempty"`    // e.g. 1 for 明天
	RelativeHours   int      `json:"relative_hours,omitempty"`   // e.g. 2 for 2小时后
	RelativeMinutes int      `json:"relative_minutes,omitempty"` // e.g. 30 for 半小时后
	HasPeriodicDays bool     `json:"has_periodic_days,omitempty"`
	PeriodicDays    []string `json:"periodic_days,omitempty"` // e.g. ["周一","周三"] or ["工作日"]
}

// ResolveTimeResult provides normalized wall-clock schedule fields for downstream
// appointment tools (timed_charge_ctrl, timed_bat_heat_ctrl, vehicle_prep_ctrl).
type ResolveTimeResult struct {
	StartTime  string `json:"start_time"` // HH:MM
	Days       string `json:"days"`       // YYYY-MM-DD or comma-separated weekday digits 1..7
	TargetTime string `json:"target_time"`
	IsPeriodic bool   `json:"is_periodic"`
}

var hhmmRe = regexp.MustCompile(`^(\d{1,2})(?::(\d{1,2}))?$`)

var weekdayCodeMap = map[string]string{
	"周一": "1", "星期一": "1", "1": "1",
	"周二": "2", "星期二": "2", "2": "2",
	"周三": "3", "星期三": "3", "3": "3",
	"周四": "4", "星期四": "4", "4": "4",
	"周五": "5", "星期五": "5", "5": "5",
	"周六": "6", "星期六": "6", "6": "6",
	"周日": "7", "周天": "7", "星期日": "7", "星期天": "7", "7": "7",
}

// ResolveTimeExpr deterministically resolves relative or periodic schedule expressions
// against Go's trusted clock so LLMs never perform date arithmetic.
func ResolveTimeExpr(now time.Time, in ResolveTimeInput) (ResolveTimeResult, error) {
	target := now
	if in.RelativeDays != 0 {
		target = target.AddDate(0, 0, in.RelativeDays)
	}
	if in.RelativeHours != 0 {
		target = target.Add(time.Duration(in.RelativeHours) * time.Hour)
	}
	if in.RelativeMinutes != 0 {
		target = target.Add(time.Duration(in.RelativeMinutes) * time.Minute)
	}

	if ts := strings.TrimSpace(in.TimeStr); ts != "" {
		m := hhmmRe.FindStringSubmatch(ts)
		if m == nil {
			return ResolveTimeResult{}, &contract.BusinessError{
				Code:    contract.InvalidArgument,
				Message: fmt.Sprintf("invalid time_str %q (expected HH:MM)", ts),
			}
		}
		hour, _ := strconv.Atoi(m[1])
		minute := 0
		if m[2] != "" {
			minute, _ = strconv.Atoi(m[2])
		}
		period := strings.TrimSpace(in.TimePeriod)
		if (period == "下午" || period == "晚上" || period == "傍晚") && hour >= 1 && hour < 12 {
			hour += 12
		}
		if hour < 0 || hour > 23 || minute < 0 || minute > 59 {
			return ResolveTimeResult{}, &contract.BusinessError{
				Code:    contract.InvalidArgument,
				Message: fmt.Sprintf("time_str %q out of 24-hour range", ts),
			}
		}
		target = time.Date(target.Year(), target.Month(), target.Day(), hour, minute, 0, 0, target.Location())
	}

	startTime := target.Format("15:04")
	if in.HasPeriodicDays || len(in.PeriodicDays) > 0 {
		var codes []string
		seen := make(map[string]bool)
		addCode := func(c string) {
			if !seen[c] {
				seen[c] = true
				codes = append(codes, c)
			}
		}
		for _, raw := range in.PeriodicDays {
			d := strings.TrimSpace(raw)
			switch d {
			case "每天", "每日":
				for _, c := range []string{"1", "2", "3", "4", "5", "6", "7"} {
					addCode(c)
				}
			case "工作日":
				for _, c := range []string{"1", "2", "3", "4", "5"} {
					addCode(c)
				}
			case "周末":
				addCode("6")
				addCode("7")
			default:
				c, ok := weekdayCodeMap[d]
				if !ok {
					return ResolveTimeResult{}, &contract.BusinessError{
						Code:    contract.InvalidArgument,
						Message: fmt.Sprintf("unsupported periodic day %q", d),
					}
				}
				addCode(c)
			}
		}
		if len(codes) == 0 {
			return ResolveTimeResult{}, &contract.BusinessError{
				Code:    contract.InvalidArgument,
				Message: "periodic_days must not be empty when has_periodic_days is true",
			}
		}
		return ResolveTimeResult{
			StartTime:  startTime,
			Days:       strings.Join(codes, ","),
			TargetTime: target.Format(time.RFC3339),
			IsPeriodic: true,
		}, nil
	}

	return ResolveTimeResult{
		StartTime:  startTime,
		Days:       target.Format("2006-01-02"),
		TargetTime: target.Format(time.RFC3339),
		IsPeriodic: false,
	}, nil
}
