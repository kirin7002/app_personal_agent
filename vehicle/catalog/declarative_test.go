package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kirin7002/app_personal_agent/contract"
)

func TestBusinessMCPCatalogCoversAll31ToolsAnd26Enabled(t *testing.T) {
	svc, err := NewBusinessMCPService(nil)
	if err != nil {
		t.Fatalf("NewBusinessMCPService: %v", err)
	}
	all := svc.AllSpecs()
	if len(all) != 31 {
		t.Fatalf("expected all 31 business MCP server tools, got %d", len(all))
	}

	enabled := svc.ListCapabilities("VCL_BIZ")
	if len(enabled) != 26 {
		t.Fatalf("expected 26 enabled business MCP tools (31 minus 5 default_disable_tools), got %d", len(enabled))
	}

	// Verify every enabled tool produces valid JSON Schema with zero sensitive identity fields.
	for _, capInfo := range enabled {
		var schema map[string]any
		if err := json.Unmarshal(capInfo.ParamsSchema, &schema); err != nil {
			t.Fatalf("tool %s.%s emitted invalid JSON schema: %v", capInfo.Device, capInfo.Tool, err)
		}
		props, _ := schema["properties"].(map[string]any)
		for propName := range props {
			if IsSensitiveFieldName(propName) {
				t.Fatalf("tool %s.%s leaked sensitive field %q into LLM schema", capInfo.Device, capInfo.Tool, propName)
			}
		}
	}

	// Verify the 5 default-disabled tools return CAPABILITY_UNAVAILABLE unless re-enabled.
	for _, disabledTool := range []string{"ac_appoint_ctrl", "child_seat_vent_ctrl", "deeplink_ctrl", "car_signal_status", "car_prepare_ctrl"} {
		_, _, err := svc.LookupByDownstreamTool("VCL_BIZ", disabledTool)
		var be *contract.BusinessError
		if !errors.As(err, &be) || be.Code != contract.CapabilityUnavailable {
			t.Fatalf("expected %s disabled by default, got %v", disabledTool, err)
		}
	}

	// VehicleProfile.EnabledTools can re-enable a default-disabled tool, and DisabledTools can disable another.
	svc.SetVehicleProfile(VehicleProfile{
		VehicleID:     "VCL_CUSTOM",
		MinTempC:      17,
		MaxTempC:      30,
		EnabledTools:  map[string]bool{"deeplink_ctrl": true},
		DisabledTools: map[string]bool{"horn_honk_ctrl": true},
	})
	if _, _, err := svc.LookupByDownstreamTool("VCL_CUSTOM", "deeplink_ctrl"); err != nil {
		t.Fatalf("expected deeplink_ctrl enabled via profile: %v", err)
	}
	if _, _, err := svc.LookupByDownstreamTool("VCL_CUSTOM", "horn_honk_ctrl"); err == nil {
		t.Fatal("expected horn_honk_ctrl disabled via DisabledTools")
	}
}

func TestBusinessMCPComplexValidationRules(t *testing.T) {
	svc, err := NewBusinessMCPService(nil)
	if err != nil {
		t.Fatal(err)
	}
	svc.SetVehicleProfile(VehicleProfile{VehicleID: "V1", MinTempC: 18, MaxTempC: 28})

	// 1. ac_ctrl: mutually exclusive temperature vs temperature_tune.
	acOp := contract.Operation{Device: "hvac", Tool: "ac_ctrl"}
	_, _, err = svc.ValidateMutation("V1", acOp, map[string]any{
		"power":            "on",
		"temperature":      "24",
		"temperature_tune": "+2",
	}, true)
	var be *contract.BusinessError
	if !errors.As(err, &be) || be.Code != contract.InvalidArgument || !strings.Contains(be.Message, "mutually exclusive") {
		t.Fatalf("expected mutually exclusive error for ac_ctrl, got %v", err)
	}
	// String-typed temperature respects dynamic vehicle bounds [18, 28].
	_, _, err = svc.ValidateMutation("V1", acOp, map[string]any{
		"power":       "on",
		"temperature": "16",
	}, true)
	if !errors.As(err, &be) || be.Code != contract.InvalidArgument {
		t.Fatalf("expected temperature out of range error for V1, got %v", err)
	}
	if _, _, err := svc.ValidateMutation("V1", acOp, map[string]any{
		"power":       "on",
		"temperature": "24",
	}, true); err != nil {
		t.Fatalf("expected valid ac_ctrl call, got %v", err)
	}

	// 2. seat_heat_ctrl: integer range [1,3] and RequireAnyOf(power, level).
	seatOp := contract.Operation{Device: "seat", Tool: "seat_heat_ctrl"}
	_, _, err = svc.ValidateMutation("V1", seatOp, map[string]any{"positionList": "front_left"}, true)
	if !errors.As(err, &be) || be.Code != contract.InvalidArgument {
		t.Fatalf("expected RequireAnyOf failure when neither power nor level supplied, got %v", err)
	}
	// Non-integer level rejected.
	_, _, err = svc.ValidateMutation("V1", seatOp, map[string]any{"positionList": "front_left", "level": 2.5}, true)
	if !errors.As(err, &be) || be.Code != contract.InvalidArgument {
		t.Fatalf("expected integer rejection for level=2.5, got %v", err)
	}
	// Valid integer level succeeds.
	out, _, err := svc.ValidateMutation("V1", seatOp, map[string]any{"positionList": "front_left", "level": 2.0}, true)
	if err != nil || out.Normalized["level"] != int64(2) {
		t.Fatalf("expected normalized int64(2), got %+v, %v", out, err)
	}

	// 3. timed_charge_ctrl: array_integer + mutually exclusive chargesoc vs chargesoc_tune.
	chargeOp := contract.Operation{Device: "charging", Tool: "timed_charge_ctrl"}
	_, _, err = svc.ValidateMutation("V1", chargeOp, map[string]any{
		"chargesoc":      "90",
		"chargesoc_tune": "+10",
	}, true)
	if !errors.As(err, &be) || be.Code != contract.InvalidArgument {
		t.Fatalf("expected mutually exclusive chargesoc rejection, got %v", err)
	}
	out, _, err = svc.ValidateMutation("V1", chargeOp, map[string]any{
		"power":      "on",
		"starttime":  "23:00",
		"cyclesList": []any{1.0, 2.0, 3.0, 2.0},
	}, true)
	if err != nil {
		t.Fatalf("expected valid timed_charge_ctrl, got %v", err)
	}
	cycles := out.Normalized["cyclesList"].([]int64)
	if len(cycles) != 3 {
		t.Fatalf("expected deduped 3 cycles, got %+v", cycles)
	}

	// 4. vehicle_prep_ctrl: nested object validation & RequireAnyOf.
	prepOp := contract.Operation{Device: "prepare", Tool: "vehicle_prep_ctrl"}
	_, _, err = svc.ValidateMutation("V1", prepOp, map[string]any{
		"start_time": "08:00",
		"days":       "2026-11-29",
		"ac":         map[string]any{"power": "on", "temperature": "35"}, // exceeds V1 max 28
	}, true)
	if !errors.As(err, &be) || be.Code != contract.InvalidArgument {
		t.Fatalf("expected nested ac.temperature bound rejection, got %v", err)
	}
	if _, _, err := svc.ValidateMutation("V1", prepOp, map[string]any{
		"start_time": "08:00",
		"days":       "2026-11-29",
		"ac":         map[string]any{"power": "on", "temperature": "24"},
		"seat_setting": map[string]any{"left_front": "3"},
	}, true); err != nil {
		t.Fatalf("expected valid nested vehicle_prep_ctrl, got %v", err)
	}

	// 5. Sensitive field supplied by LLM is strictly rejected, but injected server-side by BuildDownstreamCall.
	apptOp := contract.Operation{Device: "appointment", Tool: "car_appointment_record"}
	_, _, err = svc.ValidateMutation("V1", apptOp, map[string]any{
		"cmdidList": []any{161.0},
		"deviceKey": "SPOOFED-KEY",
	}, true)
	if !errors.As(err, &be) || be.Code != contract.InvalidArgument {
		t.Fatalf("expected sensitive deviceKey rejection, got %v", err)
	}
	scope := contract.Scope{UserID: "U1", SessionID: "S1", VehicleID: "V1"}
	call, err := svc.BuildDownstreamCall(scope, apptOp, map[string]any{"cmdidList": []any{161.0, 171.0}}, "TRUSTED-DK-99")
	if err != nil {
		t.Fatalf("BuildDownstreamCall failed: %v", err)
	}
	if call.Tool != "car_appointment_record" || call.Arguments["deviceKey"] != "TRUSTED-DK-99" {
		t.Fatalf("expected server-injected deviceKey, got %+v", call)
	}
}

func TestDeclarativeDynamicCatalogLoadAndStatusPostFilter(t *testing.T) {
	tel := NewMemoryTelemetry()
	svc, err := NewBusinessMCPService(tel)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC)
	tel.Set("V1", "hvac.power", "on", now.Add(-3*time.Second))
	tel.Set("V1", "hvac.temperature_c", 23.0, now.Add(-3*time.Second))
	tel.Set("V1", "hvac.cabin_temperature_c", 25.5, now.Add(-3*time.Second))

	scope := contract.Scope{UserID: "U1", SessionID: "S1", VehicleID: "V1"}
	res, err := svc.GetVehicleStatus(context.Background(), scope, contract.VehicleStatusInput{
		Operation: contract.Operation{Device: "hvac", Tool: "car_airconditioner_status"},
		Params:    map[string]any{"names": []string{"hvac.cabin_temperature_c"}},
	}, now)
	if err != nil || len(res.Signals) != 1 || res.Signals[0].Name != "hvac.cabin_temperature_c" || res.Signals[0].Availability != AvailabilityFresh {
		t.Fatalf("expected filtered fresh cabin_temperature_c signal, got %+v, %v", res, err)
	}

	// Adding a brand-new tool purely via declarative JSON requires zero Go code changes.
	customJSON := []byte(`{
		"version": "2026.2-custom",
		"operations": [
			{
				"device": "fragrance",
				"tool": "set_fragrance",
				"mcp_tool": "dispatch_vehicle_command",
				"title": "香氛控制",
				"category": "空调舒适",
				"description": "控制车内香氛开关与浓度",
				"fields": [
					{"name": "enabled", "type": "boolean", "required": true},
					{"name": "density", "type": "integer", "min": 1, "max": 3}
				]
			}
		]
	}`)
	if err := svc.LoadCatalogJSON(customJSON); err != nil {
		t.Fatalf("LoadCatalogJSON custom: %v", err)
	}
	if _, _, err := svc.ValidateMutation("V1", contract.Operation{Device: "fragrance", Tool: "set_fragrance"}, map[string]any{"enabled": true, "density": 2}, true); err != nil {
		t.Fatalf("expected custom declarative tool validation pass: %v", err)
	}
}

func TestResolveTimeExprAndStripSensitiveSchema(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 11, 28, 19, 10, 0, 0, loc)

	// Tomorrow morning 08:30
	r1, err := ResolveTimeExpr(now, ResolveTimeInput{RelativeDays: 1, TimePeriod: "早上", TimeStr: "8:30"})
	if err != nil || r1.StartTime != "08:30" || r1.Days != "2026-11-29" || r1.IsPeriodic {
		t.Fatalf("unexpected tomorrow resolution: %+v, %v", r1, err)
	}

	// Periodic weekdays 7:30 PM -> 19:30, days="1,2,3,4,5"
	r2, err := ResolveTimeExpr(now, ResolveTimeInput{
		TimePeriod:      "晚上",
		TimeStr:         "7:30",
		HasPeriodicDays: true,
		PeriodicDays:    []string{"工作日"},
	})
	if err != nil || r2.StartTime != "19:30" || r2.Days != "1,2,3,4,5" || !r2.IsPeriodic {
		t.Fatalf("unexpected periodic resolution: %+v, %v", r2, err)
	}

	// StripSensitiveFieldsFromSchema removes runtime & deviceKey from raw upstream schema.
	raw := json.RawMessage(`{"type":"object","properties":{"runtime":{"type":"object"},"deviceKey":{"type":"string"},"cmdidList":{"type":"array"}},"required":["runtime","cmdidList"]}`)
	cleaned, err := StripSensitiveFieldsFromSchema(raw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cleaned), "runtime") || strings.Contains(string(cleaned), "deviceKey") {
		t.Fatalf("sensitive fields not stripped: %s", string(cleaned))
	}
}
