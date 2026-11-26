package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/kirin7002/app_personal_agent/contract"
)

func TestSchemaSingleSourceOfTruthAndVehicleBounds(t *testing.T) {
	svc := NewService(nil)
	svc.SetVehicleProfile(VehicleProfile{
		VehicleID:     "VCL_STRICT",
		Model:         "EV-SUV-2026",
		ManualVersion: DefaultCatalogVersion,
		MinTempC:      18,
		MaxTempC:      28,
		DisabledOps:   map[contract.Operation]bool{{Device: "window", Tool: "open"}: true},
	})

	// Emitted JSON Schema reflects vehicle-specific temperature range.
	rawSchema, err := svc.SchemaFor("VCL_STRICT", contract.Operation{Device: "hvac", Tool: "set_temperature"})
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(rawSchema, &parsed); err != nil {
		t.Fatal(err)
	}
	props := parsed["properties"].(map[string]any)
	tempProp := props["temperature_c"].(map[string]any)
	if tempProp["minimum"].(float64) != 18 || tempProp["maximum"].(float64) != 28 {
		t.Fatalf("expected vehicle-specific bounds [18,28], got %+v", tempProp)
	}

	// Same validator rejects 17C on VCL_STRICT but accepts 17C on default VCL1.
	_, _, err = svc.ValidateMutation("VCL_STRICT", contract.Operation{Device: "hvac", Tool: "set_temperature"}, map[string]any{"temperature_c": 17}, true)
	var be *contract.BusinessError
	if !errors.As(err, &be) || be.Code != contract.InvalidArgument {
		t.Fatalf("expected INVALID_ARGUMENT for 17C on VCL_STRICT, got %v", err)
	}
	if _, _, err := svc.ValidateMutation("VCL1", contract.Operation{Device: "hvac", Tool: "set_temperature"}, map[string]any{"temperature_c": 17}, true); err != nil {
		t.Fatalf("expected 17C valid on default vehicle: %v", err)
	}

	// Disabled operation returns CAPABILITY_UNAVAILABLE.
	_, _, err = svc.ValidateMutation("VCL_STRICT", contract.Operation{Device: "window", Tool: "open"}, map[string]any{"position": "rear_left"}, true)
	if !errors.As(err, &be) || be.Code != contract.CapabilityUnavailable {
		t.Fatalf("expected CAPABILITY_UNAVAILABLE for disabled op, got %v", err)
	}

	// Unknown field rejected (additionalProperties=false).
	_, _, err = svc.ValidateMutation("VCL1", contract.Operation{Device: "window", Tool: "close"}, map[string]any{"position": "rear_left", "extra": "x"}, true)
	if !errors.As(err, &be) || be.Code != contract.InvalidArgument {
		t.Fatalf("expected INVALID_ARGUMENT for unknown field, got %v", err)
	}
}

func TestBM25RecallAndZeroRecallFallbackSearch(t *testing.T) {
	ctx := context.Background()
	svc := NewService(nil)
	scope := contract.Scope{UserID: "U1", SessionID: "S1", VehicleID: "VCL1"}

	// First-turn BM25 recall hits window.close for "把车窗关一下".
	caps, err := svc.FindCapabilities(ctx, scope, "把车窗关一下")
	if err != nil {
		t.Fatal(err)
	}
	if len(caps) == 0 || caps[0].Operation != (contract.Operation{Device: "window", Tool: "close"}) {
		t.Fatalf("expected window.close top recall, got %+v", caps)
	}

	// Unrelated utterance produces zero recall on first turn...
	emptyCaps, err := svc.FindCapabilities(ctx, scope, "帮我看看那个东西")
	if err != nil {
		t.Fatal(err)
	}
	if len(emptyCaps) != 0 {
		t.Fatalf("expected empty first-turn recall, got %+v", emptyCaps)
	}

	// ...yet subsequent search_vehicle_capabilities still finds applicable operations cleanly.
	found, err := svc.SearchCapabilities(ctx, scope, contract.SearchCapabilitiesInput{Query: "空调温度"})
	if err != nil {
		t.Fatal(err)
	}
	hasHVAC := false
	for _, c := range found.Capabilities {
		if c.Device == "hvac" {
			hasHVAC = true
		}
	}
	if !hasHVAC {
		t.Fatalf("expected search_vehicle_capabilities to find hvac, got %+v", found)
	}
}

func TestWindowStatusMixedFreshnessAndAllClosedGuard(t *testing.T) {
	ctx := context.Background()
	tel := NewMemoryTelemetry()
	svc := NewService(tel)
	scope := contract.Scope{UserID: "U1", SessionID: "S1", VehicleID: "VCL1"}
	now := time.Date(2026, 11, 26, 10, 5, 0, 0, time.UTC)

	// 3 fresh closed + 1 stale closed -> AllWindowsClosed MUST be false!
	tel.Set("VCL1", "window.front_left", "closed", now.Add(-5*time.Second))
	tel.Set("VCL1", "window.front_right", "closed", now.Add(-5*time.Second))
	tel.Set("VCL1", "window.rear_left", "closed", now.Add(-5*time.Second))
	tel.Set("VCL1", "window.rear_right", "closed", now.Add(-2*time.Minute))

	res, err := svc.GetVehicleStatus(ctx, scope, contract.VehicleStatusInput{
		Operation: contract.Operation{Device: "window", Tool: "get_status"},
		Params:    map[string]any{"positions": WindowPositions},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Signals) != 4 {
		t.Fatalf("expected 4 signals, got %d", len(res.Signals))
	}
	if res.Signals[3].Availability != AvailabilityStale {
		t.Fatalf("expected rear_right stale, got %+v", res.Signals[3])
	}
	if AllWindowsClosed(res) {
		t.Fatal("AllWindowsClosed must return false when any window is stale")
	}

	// Unavailable window also prevents AllWindowsClosed.
	tel.SetUnavailable("VCL1", "window.rear_right")
	resUnavail, err := svc.GetVehicleStatus(ctx, scope, contract.VehicleStatusInput{
		Operation: contract.Operation{Device: "window", Tool: "get_status"},
		Params:    map[string]any{"positions": WindowPositions},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if resUnavail.Signals[3].Availability != AvailabilityUnavailable || AllWindowsClosed(resUnavail) {
		t.Fatalf("expected unavailable rear_right and AllWindowsClosed=false, got %+v", resUnavail)
	}

	// All 4 fresh and closed -> true.
	tel.Set("VCL1", "window.rear_right", "closed", now.Add(-2*time.Second))
	resFresh, err := svc.GetVehicleStatus(ctx, scope, contract.VehicleStatusInput{
		Operation: contract.Operation{Device: "window", Tool: "get_status"},
		Params:    map[string]any{"positions": WindowPositions},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if !AllWindowsClosed(resFresh) {
		t.Fatalf("expected AllWindowsClosed=true when all 4 are fresh closed, got %+v", resFresh)
	}
}

func TestConfirmCommandEffectTemporalAndHVACRules(t *testing.T) {
	ctx := context.Background()
	tel := NewMemoryTelemetry()
	svc := NewService(tel)
	dispatchedAt := time.Date(2026, 11, 26, 10, 0, 10, 0, time.UTC)
	now := dispatchedAt.Add(5 * time.Second)

	// Pre-dispatch sample MUST NOT confirm even if value already matches!
	tel.Set("VCL1", "window.rear_left", "closed", dispatchedAt.Add(-1*time.Second))
	conf, err := svc.ConfirmCommandEffect(ctx, "VCL1", contract.Operation{Device: "window", Tool: "close"}, map[string]any{"position": "rear_left"}, dispatchedAt, now)
	if err != nil {
		t.Fatal(err)
	}
	if conf.Confirmed {
		t.Fatalf("pre-dispatch sample must not confirm: %+v", conf)
	}

	// Post-dispatch fresh sample confirms.
	tel.Set("VCL1", "window.rear_left", "closed", dispatchedAt.Add(2*time.Second))
	conf, err = svc.ConfirmCommandEffect(ctx, "VCL1", contract.Operation{Device: "window", Tool: "close"}, map[string]any{"position": "rear_left"}, dispatchedAt, now)
	if err != nil || !conf.Confirmed {
		t.Fatalf("expected post-dispatch fresh sample to confirm, got %+v, %v", conf, err)
	}

	// HVAC set_temperature must verify BOTH power=on AND target temperature.
	tel.Set("VCL1", "hvac.power", "off", dispatchedAt.Add(2*time.Second))
	tel.Set("VCL1", "hvac.temperature_c", 25.0, dispatchedAt.Add(2*time.Second))
	confHVAC, err := svc.ConfirmCommandEffect(ctx, "VCL1", contract.Operation{Device: "hvac", Tool: "set_temperature"}, map[string]any{"temperature_c": 25}, dispatchedAt, now)
	if err != nil || confHVAC.Confirmed {
		t.Fatalf("HVAC with power=off must not confirm: %+v, %v", confHVAC, err)
	}
	tel.Set("VCL1", "hvac.power", "on", dispatchedAt.Add(3*time.Second))
	confHVAC, err = svc.ConfirmCommandEffect(ctx, "VCL1", contract.Operation{Device: "hvac", Tool: "set_temperature"}, map[string]any{"temperature_c": 25}, dispatchedAt, now)
	if err != nil || !confHVAC.Confirmed {
		t.Fatalf("HVAC with power=on and temp=25 must confirm: %+v, %v", confHVAC, err)
	}
}

func TestSearchManualModelAndVersionIsolation(t *testing.T) {
	ctx := context.Background()
	svc := NewService(nil)
	svc.SetVehicleProfile(VehicleProfile{
		VehicleID:     "VCL_SEDAN",
		Model:         "EV-SEDAN-2025",
		ManualVersion: "2025.3",
	})

	suvScope := contract.Scope{UserID: "U1", SessionID: "S1", VehicleID: "VCL1"}
	sedanScope := contract.Scope{UserID: "U1", SessionID: "S1", VehicleID: "VCL_SEDAN"}

	suvRes, err := svc.SearchManual(ctx, suvScope, contract.SearchManualInput{Query: "自动关窗在哪里设置"})
	if err != nil || len(suvRes.Excerpts) == 0 {
		t.Fatalf("expected SUV manual excerpt, got %+v, %v", suvRes, err)
	}
	if suvRes.Excerpts[0].Version != DefaultCatalogVersion || suvRes.Excerpts[0].Source == "" {
		t.Fatalf("expected version & traceable source: %+v", suvRes.Excerpts[0])
	}

	sedanRes, err := svc.SearchManual(ctx, sedanScope, contract.SearchManualInput{Query: "自动关窗在哪里设置"})
	if err != nil || len(sedanRes.Excerpts) == 0 {
		t.Fatalf("expected sedan manual excerpt, got %+v, %v", sedanRes, err)
	}
	if sedanRes.Excerpts[0].Version != "2025.3" || sedanRes.Excerpts[0].Source == suvRes.Excerpts[0].Source {
		t.Fatalf("expected model-version isolated manual excerpt, got %+v", sedanRes.Excerpts[0])
	}
}
