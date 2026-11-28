package mcp

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/kirin7002/app_personal_agent/contract"
	"github.com/kirin7002/app_personal_agent/vehicle/catalog"
	"github.com/kirin7002/app_personal_agent/vehicle/tasks"
	"github.com/kirin7002/app_personal_agent/vehicle/turns"
)

func newBusinessGuardEnv(t *testing.T) *testEnv {
	t.Helper()
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	tel := catalog.NewMemoryTelemetry()
	cat, err := catalog.NewBusinessMCPService(tel)
	if err != nil {
		t.Fatalf("NewBusinessMCPService: %v", err)
	}
	turnStore := turns.NewMemoryStore()
	taskStore := tasks.NewMemoryStore(tasks.NewCatalogValidator(cat))
	env := &testEnv{
		now:       now,
		tel:       tel,
		cat:       cat,
		turnStore: turnStore,
		taskStore: taskStore,
	}
	env.server = NewServer(Config{
		ServiceToken: "svc-secret",
		Turns:        turnStore,
		Tasks:        taskStore,
		Catalog:      cat,
		Now:          func() time.Time { return env.now },
	})
	return env
}

func TestCatalogDrivenGuardSeatHeatAndAmbiguity(t *testing.T) {
	env := newBusinessGuardEnv(t)
	scope := contract.Scope{UserID: "U1", SessionID: "S1", VehicleID: "VCL_BIZ"}

	// 1. Multi-seat + Chinese numeral level "二档" succeeds purely via catalog metadata!
	env.registerTurn(t, scope, "RS1", "THS1", "HS1", "把主驾和副驾座椅加热调到二档")
	_, isErr, raw := env.callTool(t, "svc-secret", "THS1", "HS1", contract.DispatchVehicleCommand, map[string]any{
		"device": "seat", "tool": "seat_heat_ctrl",
		"params": map[string]any{"positionList": "front_left,front_right", "level": 2},
	}, nil)
	if isErr {
		t.Fatalf("expected valid seat_heat_ctrl dispatch, got %s", string(raw))
	}

	// 2. Hallucinated level (utterance did NOT mention 3档) MUST be rejected!
	env.registerTurn(t, scope, "RS2", "THS2", "HS1", "打开主驾座椅加热")
	_, isErr, raw = env.callTool(t, "svc-secret", "THS2", "HS1", contract.DispatchVehicleCommand, map[string]any{
		"device": "seat", "tool": "seat_heat_ctrl",
		"params": map[string]any{"positionList": "front_left", "power": "on", "level": 3},
	}, nil)
	if !isErr || decodeBizErr(t, raw).Code != contract.ClarificationRequired {
		t.Fatalf("hallucinated seat level must be rejected with CLARIFICATION_REQUIRED, got isErr=%v raw=%s", isErr, string(raw))
	}

	// 3. Two pending seat tasks (seat_heat_ctrl + seat_vent_ctrl) missing positionList ->
	//    bare "主驾" rejected as ambiguous; "主驾座椅加热" uniquely resumes seat_heat_ctrl!
	env.registerTurn(t, scope, "RS3", "THS3", "HS1", "打开座椅加热")
	_, isErr, rHeat := env.callTool(t, "svc-secret", "THS3", "HS1", contract.CreateVehicleTask, map[string]any{
		"device": "seat", "tool": "seat_heat_ctrl", "params": map[string]any{"power": "on"},
	}, nil)
	if isErr {
		t.Fatalf("create pending seat_heat_ctrl failed: %s", string(rHeat))
	}
	var tHeat contract.TaskResult
	_ = json.Unmarshal(rHeat, &tHeat)

	env.registerTurn(t, scope, "RS4", "THS4", "HS1", "打开座椅通风")
	_, isErr, rVent := env.callTool(t, "svc-secret", "THS4", "HS1", contract.CreateVehicleTask, map[string]any{
		"device": "seat", "tool": "seat_vent_ctrl", "params": map[string]any{"power": "on"},
	}, nil)
	if isErr {
		t.Fatalf("create pending seat_vent_ctrl failed: %s", string(rVent))
	}
	var tVent contract.TaskResult
	_ = json.Unmarshal(rVent, &tVent)

	cands := []contract.CandidateVersion{
		{TaskID: tHeat.TaskID, Version: 1},
		{TaskID: tVent.TaskID, Version: 1},
	}

	// Bare "主驾" matches BOTH seat_heat_ctrl and seat_vent_ctrl -> rejected as ambiguous!
	env.registerTurn(t, scope, "RS5", "THS5", "HS1", "主驾", cands...)
	_, isErr, raw = env.callTool(t, "svc-secret", "THS5", "HS1", contract.DispatchVehicleCommand, map[string]any{
		"task_id": tHeat.TaskID, "device": "seat", "tool": "seat_heat_ctrl",
		"params": map[string]any{"positionList": "front_left"},
	}, nil)
	if !isErr || decodeBizErr(t, raw).Code != contract.ClarificationRequired {
		t.Fatalf("bare 主驾 across heat+vent candidates must be rejected as ambiguous, got isErr=%v raw=%s", isErr, string(raw))
	}

	// Explicit "主驾座椅加热" triggers seat_vent_ctrl's anti_example ("座椅加热"), uniquely matching seat_heat_ctrl!
	env.registerTurn(t, scope, "RS6", "THS6", "HS1", "主驾座椅加热", cands...)
	_, isErr, raw = env.callTool(t, "svc-secret", "THS6", "HS1", contract.DispatchVehicleCommand, map[string]any{
		"task_id": tHeat.TaskID, "device": "seat", "tool": "seat_heat_ctrl",
		"params": map[string]any{"positionList": "front_left"},
	}, nil)
	if isErr {
		t.Fatalf("disambiguated 主驾座椅加热 should succeed, got %s", string(raw))
	}
}

func TestCatalogDrivenGuardLockTrunkChargingAndRelativeTune(t *testing.T) {
	env := newBusinessGuardEnv(t)
	scope := contract.Scope{UserID: "U1", SessionID: "S1", VehicleID: "VCL_BIZ"}

	// 1. car_lock_ctrl: "帮我锁车" rejects unlock, allows lock; "别解锁" rejected.
	env.registerTurn(t, scope, "RL1", "THL1", "HS1", "帮我锁车")
	_, isErr, raw := env.callTool(t, "svc-secret", "THL1", "HS1", contract.DispatchVehicleCommand, map[string]any{
		"device": "lock", "tool": "car_lock_ctrl", "params": map[string]any{"operate": "unlock"},
	}, nil)
	if !isErr || decodeBizErr(t, raw).Code != contract.ClarificationRequired {
		t.Fatalf("lock utterance must reject unlock, got %s", string(raw))
	}
	_, isErr, raw = env.callTool(t, "svc-secret", "THL1", "HS1", contract.DispatchVehicleCommand, map[string]any{
		"device": "lock", "tool": "car_lock_ctrl", "params": map[string]any{"operate": "lock"},
	}, nil)
	if isErr {
		t.Fatalf("lock utterance should allow lock, got %s", string(raw))
	}

	env.registerTurn(t, scope, "RL2", "THL2", "HS1", "别解锁车辆")
	_, isErr, raw = env.callTool(t, "svc-secret", "THL2", "HS1", contract.DispatchVehicleCommand, map[string]any{
		"device": "lock", "tool": "car_lock_ctrl", "params": map[string]any{"operate": "unlock"},
	}, nil)
	if !isErr || decodeBizErr(t, raw).Code != contract.ClarificationRequired {
		t.Fatalf("negated unlock must be rejected, got %s", string(raw))
	}

	// 2. trunk_ctrl: "关闭尾门" allows operate="false", rejects "true".
	env.registerTurn(t, scope, "RT1", "THT1", "HS1", "把尾门关上")
	_, isErr, raw = env.callTool(t, "svc-secret", "THT1", "HS1", contract.DispatchVehicleCommand, map[string]any{
		"device": "trunk", "tool": "trunk_ctrl", "params": map[string]any{"operate": "true"},
	}, nil)
	if !isErr || decodeBizErr(t, raw).Code != contract.ClarificationRequired {
		t.Fatalf("close trunk utterance must reject open, got %s", string(raw))
	}
	_, isErr, raw = env.callTool(t, "svc-secret", "THT1", "HS1", contract.DispatchVehicleCommand, map[string]any{
		"device": "trunk", "tool": "trunk_ctrl", "params": map[string]any{"operate": "false"},
	}, nil)
	if isErr {
		t.Fatalf("close trunk utterance should allow false, got %s", string(raw))
	}

	// 3. charge_start_stop_ctrl: "现在正在充电吗" blocked as query; "停止充电" allows finish.
	env.registerTurn(t, scope, "RCQ", "THCQ", "HS1", "现在正在充电吗")
	_, isErr, raw = env.callTool(t, "svc-secret", "THCQ", "HS1", contract.DispatchVehicleCommand, map[string]any{
		"device": "charging", "tool": "charge_start_stop_ctrl", "params": map[string]any{"operate": "start"},
	}, nil)
	if !isErr || decodeBizErr(t, raw).Code != contract.ClarificationRequired {
		t.Fatalf("charging query must not write, got %s", string(raw))
	}
	env.registerTurn(t, scope, "RCS", "THCS", "HS1", "停止充电")
	_, isErr, raw = env.callTool(t, "svc-secret", "THCS", "HS1", contract.DispatchVehicleCommand, map[string]any{
		"device": "charging", "tool": "charge_start_stop_ctrl", "params": map[string]any{"operate": "finish"},
	}, nil)
	if isErr {
		t.Fatalf("stop charging should succeed, got %s", string(raw))
	}

	// 4. ac_ctrl relative tuning: "把空调温度升高3度" allows "+3", rejects "-3" and "+5".
	env.registerTurn(t, scope, "RA1", "THA1", "HS1", "把空调温度升高3度")
	_, isErr, raw = env.callTool(t, "svc-secret", "THA1", "HS1", contract.DispatchVehicleCommand, map[string]any{
		"device": "hvac", "tool": "ac_ctrl", "params": map[string]any{"power": "on", "temperature_tune": "-3"},
	}, nil)
	if !isErr || decodeBizErr(t, raw).Code != contract.ClarificationRequired {
		t.Fatalf("opposite relative tune sign must be rejected, got %s", string(raw))
	}
	_, isErr, raw = env.callTool(t, "svc-secret", "THA1", "HS1", contract.DispatchVehicleCommand, map[string]any{
		"device": "hvac", "tool": "ac_ctrl", "params": map[string]any{"power": "on", "temperature_tune": "+5"},
	}, nil)
	if !isErr || decodeBizErr(t, raw).Code != contract.ClarificationRequired {
		t.Fatalf("mismatched relative tune magnitude must be rejected, got %s", string(raw))
	}
	_, isErr, raw = env.callTool(t, "svc-secret", "THA1", "HS1", contract.DispatchVehicleCommand, map[string]any{
		"device": "hvac", "tool": "ac_ctrl", "params": map[string]any{"power": "on", "temperature_tune": "+3"},
	}, nil)
	if isErr {
		t.Fatalf("matching relative tune +3 should succeed, got %s", string(raw))
	}
}

func TestClauseAwarePartialNegation(t *testing.T) {
	env := newTestEnv()
	scope := contract.Scope{UserID: "U1", SessionID: "S1", VehicleID: "VCL1"}

	// "先别开空调，把左后窗关上" -> blocks hvac, allows window.close(rear_left)!
	env.registerTurn(t, scope, "RP1", "THP1", "HS1", "先别开空调，把左后窗关上")
	_, isErr, raw := env.callTool(t, "svc-secret", "THP1", "HS1", contract.DispatchVehicleCommand, map[string]any{
		"device": "hvac", "tool": "set_climate", "params": map[string]any{"power": "on", "temperature_c": 24},
	}, nil)
	if !isErr || decodeBizErr(t, raw).Code != contract.ClarificationRequired {
		t.Fatalf("negated hvac clause must block hvac write, got %s", string(raw))
	}

	_, isErr, raw = env.callTool(t, "svc-secret", "THP1", "HS1", contract.DispatchVehicleCommand, map[string]any{
		"device": "window", "tool": "close", "params": map[string]any{"position": "rear_left"},
	}, nil)
	if isErr {
		t.Fatalf("positive window.close clause in partial negation sentence should succeed, got %s", string(raw))
	}
}
