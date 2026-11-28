// Package catalog implements the versioned vehicle capability map, BM25 recall,
// single-source parameter schema validation, telemetry status reading (with
// command confirmation helpers), and versioned vehicle manual search (H06).
package catalog

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"

	"github.com/kirin7002/app_personal_agent/contract"
)

const DefaultCatalogVersion = "2026.1"

var WindowPositions = []string{"front_left", "front_right", "rear_left", "rear_right"}

// VehicleProfile describes model-specific capability constraints and manual version.
type VehicleProfile struct {
	VehicleID     string
	Model         string
	ManualVersion string
	MinTempC      float64
	MaxTempC      float64
	// DisabledOps optionally hides specific operations for a vehicle trim.
	DisabledOps map[contract.Operation]bool
}

func DefaultProfile(vehicleID string) VehicleProfile {
	return VehicleProfile{
		VehicleID:     vehicleID,
		Model:         "EV-SUV-2026",
		ManualVersion: DefaultCatalogVersion,
		MinTempC:      16,
		MaxTempC:      32,
	}
}

// FieldSpec is the single source of truth for both JSON Schema emission and Go validation.
type FieldSpec struct {
	Name      string
	Type      string // "string", "number", "array_string"
	Required  bool
	Enum      []string
	Min       *float64
	Max       *float64
	MinItems  int
	Question  string
}

// OperationSpec defines one device.tool capability in the versioned functional map.
type OperationSpec struct {
	Operation   contract.Operation
	MCPTool     string
	Description string
	Aliases     []string
	Utterances  []string
	Fields      []FieldSpec
	// DynamicBounds adjusts numeric bounds per vehicle profile when non-nil.
	DynamicBounds func(p VehicleProfile, f FieldSpec) FieldSpec
	// QuestionFn builds a natural Chinese follow-up question for the given missing fields.
	QuestionFn func(missing []string) string
}

// ParamsSchemaFor renders the exact JSON Schema for this operation and vehicle profile.
func (spec OperationSpec) ParamsSchemaFor(profile VehicleProfile) json.RawMessage {
	props := make(map[string]any, len(spec.Fields))
	required := make([]string, 0, len(spec.Fields))
	for _, rawField := range spec.Fields {
		f := rawField
		if spec.DynamicBounds != nil {
			f = spec.DynamicBounds(profile, f)
		}
		prop := map[string]any{}
		switch f.Type {
		case "string":
			prop["type"] = "string"
			if len(f.Enum) > 0 {
				prop["enum"] = f.Enum
			}
		case "number":
			prop["type"] = "number"
			if f.Min != nil {
				prop["minimum"] = *f.Min
			}
			if f.Max != nil {
				prop["maximum"] = *f.Max
			}
		case "array_string":
			prop["type"] = "array"
			items := map[string]any{"type": "string"}
			if len(f.Enum) > 0 {
				items["enum"] = f.Enum
			}
			prop["items"] = items
			if f.MinItems > 0 {
				prop["minItems"] = f.MinItems
			}
		}
		props[f.Name] = prop
		if f.Required {
			required = append(required, f.Name)
		}
	}
	schemaObj := map[string]any{
		"type":                 "object",
		"properties":           props,
		"required":             required,
		"additionalProperties": false,
	}
	raw, _ := json.Marshal(schemaObj)
	return raw
}

// ValidationOutcome holds normalized parameters, missing required fields, and follow-up question.
type ValidationOutcome struct {
	Normalized    map[string]any
	MissingFields []string
	Question      string
}

// ValidateParams validates params against the same OperationSpec used to build ParamsSchema.
// When requireComplete is true, any missing required field returns INVALID_ARGUMENT.
func (spec OperationSpec) ValidateParams(profile VehicleProfile, params map[string]any, requireComplete bool) (ValidationOutcome, error) {
	if params == nil {
		params = map[string]any{}
	}
	fieldByName := make(map[string]FieldSpec, len(spec.Fields))
	for _, rawField := range spec.Fields {
		f := rawField
		if spec.DynamicBounds != nil {
			f = spec.DynamicBounds(profile, f)
		}
		fieldByName[f.Name] = f
	}

	normalized := make(map[string]any, len(params))
	for k, v := range params {
		f, ok := fieldByName[k]
		if !ok {
			return ValidationOutcome{}, &contract.BusinessError{
				Code:    contract.InvalidArgument,
				Message: fmt.Sprintf("unknown parameter %q for %s.%s", k, spec.Operation.Device, spec.Operation.Tool),
			}
		}
		if v == nil {
			continue
		}
		val, err := validateFieldValue(f, v)
		if err != nil {
			return ValidationOutcome{}, err
		}
		normalized[k] = val
	}

	var missing []string
	for _, f := range spec.Fields {
		if !f.Required {
			continue
		}
		if _, ok := normalized[f.Name]; !ok {
			missing = append(missing, f.Name)
		}
	}

	if requireComplete && len(missing) > 0 {
		return ValidationOutcome{}, &contract.BusinessError{
			Code:    contract.InvalidArgument,
			Message: fmt.Sprintf("missing required parameters %s for %s.%s", strings.Join(missing, ", "), spec.Operation.Device, spec.Operation.Tool),
		}
	}

	question := ""
	if len(missing) > 0 {
		if spec.QuestionFn != nil {
			question = spec.QuestionFn(missing)
		} else {
			question = fieldByName[missing[0]].Question
		}
	}
	return ValidationOutcome{
		Normalized:    normalized,
		MissingFields: missing,
		Question:      question,
	}, nil
}

func validateFieldValue(f FieldSpec, raw any) (any, error) {
	switch f.Type {
	case "string":
		s, ok := raw.(string)
		if !ok || strings.TrimSpace(s) == "" {
			return nil, &contract.BusinessError{
				Code:    contract.InvalidArgument,
				Message: fmt.Sprintf("parameter %q must be a non-empty string", f.Name),
			}
		}
		if len(f.Enum) > 0 && !containsString(f.Enum, s) {
			return nil, &contract.BusinessError{
				Code:    contract.InvalidArgument,
				Message: fmt.Sprintf("parameter %q value %q not in allowed set [%s]", f.Name, s, strings.Join(f.Enum, ", ")),
			}
		}
		return s, nil

	case "number":
		num, ok := toFloat64(raw)
		if !ok || math.IsNaN(num) || math.IsInf(num, 0) {
			return nil, &contract.BusinessError{
				Code:    contract.InvalidArgument,
				Message: fmt.Sprintf("parameter %q must be a finite number", f.Name),
			}
		}
		if f.Min != nil && num < *f.Min {
			return nil, &contract.BusinessError{
				Code:    contract.InvalidArgument,
				Message: fmt.Sprintf("parameter %q=%.1f below minimum %.1f", f.Name, num, *f.Min),
			}
		}
		if f.Max != nil && num > *f.Max {
			return nil, &contract.BusinessError{
				Code:    contract.InvalidArgument,
				Message: fmt.Sprintf("parameter %q=%.1f exceeds maximum %.1f", f.Name, num, *f.Max),
			}
		}
		return num, nil

	case "array_string":
		list, ok := toStringSlice(raw)
		if !ok {
			return nil, &contract.BusinessError{
				Code:    contract.InvalidArgument,
				Message: fmt.Sprintf("parameter %q must be an array of strings", f.Name),
			}
		}
		if len(list) < f.MinItems {
			return nil, &contract.BusinessError{
				Code:    contract.InvalidArgument,
				Message: fmt.Sprintf("parameter %q requires at least %d item(s)", f.Name, f.MinItems),
			}
		}
		seen := make(map[string]bool, len(list))
		dedup := make([]string, 0, len(list))
		for _, item := range list {
			if len(f.Enum) > 0 && !containsString(f.Enum, item) {
				return nil, &contract.BusinessError{
					Code:    contract.InvalidArgument,
					Message: fmt.Sprintf("parameter %q item %q not in allowed set [%s]", f.Name, item, strings.Join(f.Enum, ", ")),
				}
			}
			if !seen[item] {
				seen[item] = true
				dedup = append(dedup, item)
			}
		}
		return dedup, nil
	default:
		return raw, nil
	}
}

func containsString(list []string, target string) bool {
	for _, s := range list {
		if s == target {
			return true
		}
	}
	return false
}

func toFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

func toStringSlice(v any) ([]string, bool) {
	switch s := v.(type) {
	case []string:
		return s, true
	case []any:
		out := make([]string, 0, len(s))
		for _, item := range s {
			str, ok := item.(string)
			if !ok {
				return nil, false
			}
			out = append(out, str)
		}
		return out, true
	default:
		return nil, false
	}
}

// Service provides capability lookup, BM25 recall, parameter validation,
// telemetry status queries, command confirmation, and manual search.
type Service struct {
	Version string

	mu        sync.RWMutex
	specs     map[contract.Operation]OperationSpec
	order     []contract.Operation
	profiles  map[string]VehicleProfile
	telemetry TelemetryProvider
	manuals   []ManualEntry
}

// NewService constructs a default versioned catalog populated with the design's
// core window and HVAC operations and sample versioned vehicle manuals.
func NewService(telemetry TelemetryProvider) *Service {
	if telemetry == nil {
		telemetry = NewMemoryTelemetry()
	}
	s := &Service{
		Version:   DefaultCatalogVersion,
		specs:     make(map[contract.Operation]OperationSpec),
		profiles:  make(map[string]VehicleProfile),
		telemetry: telemetry,
		manuals:   defaultManualEntries(),
	}
	for _, spec := range defaultOperationSpecs() {
		s.RegisterOperation(spec)
	}
	return s
}

func (s *Service) RegisterOperation(spec OperationSpec) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.specs[spec.Operation]; !exists {
		s.order = append(s.order, spec.Operation)
	}
	s.specs[spec.Operation] = spec
}

func (s *Service) SetVehicleProfile(profile VehicleProfile) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if profile.MinTempC == 0 && profile.MaxTempC == 0 {
		profile.MinTempC, profile.MaxTempC = 16, 32
	}
	if profile.ManualVersion == "" {
		profile.ManualVersion = s.Version
	}
	if profile.Model == "" {
		profile.Model = "EV-SUV-2026"
	}
	s.profiles[profile.VehicleID] = profile
}

func (s *Service) Profile(vehicleID string) VehicleProfile {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if p, ok := s.profiles[vehicleID]; ok {
		return p
	}
	return DefaultProfile(vehicleID)
}

// LookupOperation resolves an operation and checks vehicle-level availability.
func (s *Service) LookupOperation(vehicleID string, op contract.Operation) (OperationSpec, VehicleProfile, error) {
	s.mu.RLock()
	spec, ok := s.specs[op]
	profile, hasProfile := s.profiles[vehicleID]
	s.mu.RUnlock()
	if !hasProfile {
		profile = DefaultProfile(vehicleID)
	}
	if !ok || profile.DisabledOps[op] {
		return OperationSpec{}, profile, &contract.BusinessError{
			Code:    contract.CapabilityUnavailable,
			Message: fmt.Sprintf("operation %s.%s is unavailable for vehicle %s", op.Device, op.Tool, vehicleID),
		}
	}
	return spec, profile, nil
}

// SchemaFor returns the exact JSON Schema for an operation on a specific vehicle.
func (s *Service) SchemaFor(vehicleID string, op contract.Operation) (json.RawMessage, error) {
	spec, profile, err := s.LookupOperation(vehicleID, op)
	if err != nil {
		return nil, err
	}
	return spec.ParamsSchemaFor(profile), nil
}

// ValidateMutation validates parameters against the vehicle's operation schema.
func (s *Service) ValidateMutation(vehicleID string, op contract.Operation, params map[string]any, requireComplete bool) (ValidationOutcome, OperationSpec, error) {
	spec, profile, err := s.LookupOperation(vehicleID, op)
	if err != nil {
		return ValidationOutcome{}, OperationSpec{}, err
	}
	outcome, err := spec.ValidateParams(profile, params, requireComplete)
	if err != nil {
		return ValidationOutcome{}, spec, err
	}
	return outcome, spec, nil
}

// CapabilityInfo is a read-only exported view of an enabled capability and its
// vehicle-specific parameter schema.
type CapabilityInfo struct {
	Device       string          `json:"device"`
	Tool         string          `json:"tool"`
	MCPTool      string          `json:"mcp_tool"`
	Description  string          `json:"description"`
	Aliases      []string        `json:"aliases"`
	Utterances   []string        `json:"utterances"`
	ParamsSchema json.RawMessage `json:"params_schema"`
}

// ListCapabilities returns all enabled capabilities and schemas for vehicleID.
func (s *Service) ListCapabilities(vehicleID string) []CapabilityInfo {
	s.mu.RLock()
	profile, ok := s.profiles[vehicleID]
	if !ok {
		profile = DefaultProfile(vehicleID)
	}
	specs := s.enabledSpecsLocked(profile)
	s.mu.RUnlock()

	out := make([]CapabilityInfo, 0, len(specs))
	for _, sp := range specs {
		aliases := make([]string, len(sp.Aliases))
		copy(aliases, sp.Aliases)
		utterances := make([]string, len(sp.Utterances))
		copy(utterances, sp.Utterances)
		out = append(out, CapabilityInfo{
			Device:       sp.Operation.Device,
			Tool:         sp.Operation.Tool,
			MCPTool:      sp.MCPTool,
			Description:  sp.Description,
			Aliases:      aliases,
			Utterances:   utterances,
			ParamsSchema: sp.ParamsSchemaFor(profile),
		})
	}
	return out
}

func (s *Service) enabledSpecsLocked(profile VehicleProfile) []OperationSpec {
	out := make([]OperationSpec, 0, len(s.order))
	for _, op := range s.order {
		if profile.DisabledOps[op] {
			continue
		}
		out = append(out, s.specs[op])
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Operation.Device != out[j].Operation.Device {
			return out[i].Operation.Device < out[j].Operation.Device
		}
		return out[i].Operation.Tool < out[j].Operation.Tool
	})
	return out
}

func defaultOperationSpecs() []OperationSpec {
	tempBounds := func(p VehicleProfile, f FieldSpec) FieldSpec {
		if f.Name == "temperature_c" {
			minV, maxV := p.MinTempC, p.MaxTempC
			f.Min = &minV
			f.Max = &maxV
		}
		return f
	}
	return []OperationSpec{
		{
			Operation:   contract.Operation{Device: "window", Tool: "close"},
			MCPTool:     contract.DispatchVehicleCommand,
			Description: "关闭指定位置的车窗",
			Aliases:     []string{"关窗", "关闭车窗", "升窗", "摇上车窗", "window close", "左前窗", "右前窗", "左后窗", "右后窗"},
			Utterances:  []string{"把车窗关一下", "把左后窗关上", "关闭右前窗", "左后窗关上"},
			Fields: []FieldSpec{
				{Name: "position", Type: "string", Required: true, Enum: WindowPositions, Question: "要关哪扇车窗？"},
			},
			QuestionFn: func([]string) string { return "要关哪扇车窗？" },
		},
		{
			Operation:   contract.Operation{Device: "window", Tool: "open"},
			MCPTool:     contract.DispatchVehicleCommand,
			Description: "打开指定位置的车窗",
			Aliases:     []string{"开窗", "打开车窗", "降窗", "摇下车窗", "window open", "左前窗", "右前窗", "左后窗", "右后窗"},
			Utterances:  []string{"把车窗打开", "把左后窗打开", "打开右前窗", "降下左前窗"},
			Fields: []FieldSpec{
				{Name: "position", Type: "string", Required: true, Enum: WindowPositions, Question: "要打开哪扇车窗？"},
			},
			QuestionFn: func([]string) string { return "要打开哪扇车窗？" },
		},
		{
			Operation:   contract.Operation{Device: "window", Tool: "get_status"},
			MCPTool:     contract.GetVehicleStatus,
			Description: "查询一扇或多扇车窗的开关状态",
			Aliases:     []string{"查车窗", "车窗状态", "车窗关了吗", "车窗开着吗", "四窗状态", "window status"},
			Utterances:  []string{"左后窗关了吗", "左后窗开着吗", "车窗都关了吗", "看看四个车窗关好没"},
			Fields: []FieldSpec{
				{Name: "positions", Type: "array_string", Required: true, Enum: WindowPositions, MinItems: 1, Question: "要查询哪扇车窗的状态？"},
			},
		},
		{
			Operation:     contract.Operation{Device: "hvac", Tool: "set_temperature"},
			MCPTool:       contract.DispatchVehicleCommand,
			Description:   "设置车内空调目标温度",
			Aliases:       []string{"空调温度", "设温", "调温度", "几度", "调高温度", "调低温度", "hvac temperature"},
			Utterances:    []string{"把空调调到25度", "空调设成24度", "温度调到22度"},
			DynamicBounds: tempBounds,
			Fields: []FieldSpec{
				{Name: "temperature_c", Type: "number", Required: true, Question: "空调要调到几度？"},
			},
			QuestionFn: func([]string) string { return "空调要调到几度？" },
		},
		{
			Operation:     contract.Operation{Device: "hvac", Tool: "set_climate"},
			MCPTool:       contract.DispatchVehicleCommand,
			Description:   "同时控制空调开关状态与目标温度",
			Aliases:       []string{"空调", "开空调", "关空调", "冷风", "暖风", "制热", "制冷", "hvac climate"},
			Utterances:    []string{"打开空调并调到25度", "把空调打开", "空调开一下", "打开空调调到24度"},
			DynamicBounds: tempBounds,
			Fields: []FieldSpec{
				{Name: "power", Type: "string", Required: true, Enum: []string{"on", "off"}, Question: "要打开还是关闭空调？"},
				{Name: "temperature_c", Type: "number", Required: true, Question: "空调要调到几度？"},
			},
			QuestionFn: func(missing []string) string {
				hasPower := containsString(missing, "power")
				hasTemp := containsString(missing, "temperature_c")
				if hasPower && hasTemp {
					return "请确认空调要打开还是关闭，以及调到几度？"
				}
				if hasPower {
					return "要打开还是关闭空调？"
				}
				return "空调要调到几度？"
			},
		},
		{
			Operation:   contract.Operation{Device: "hvac", Tool: "get_status"},
			MCPTool:     contract.GetVehicleStatus,
			Description: "查询空调当前开关状态与设定温度",
			Aliases:     []string{"空调状态", "空调开着吗", "现在空调几度", "hvac status"},
			Utterances:  []string{"空调开着吗", "现在空调设定多少度"},
			Fields:      []FieldSpec{},
		},
	}
}

var _ contract.CapabilityFinder = (*Service)(nil)
