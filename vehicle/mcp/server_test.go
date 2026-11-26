package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kirin7002/app_personal_agent/contract"
	"github.com/kirin7002/app_personal_agent/vehicle/catalog"
	"github.com/kirin7002/app_personal_agent/vehicle/tasks"
	"github.com/kirin7002/app_personal_agent/vehicle/turns"
)

type testEnv struct {
	now       time.Time
	tel       *catalog.MemoryTelemetry
	cat       *catalog.Service
	turnStore *turns.MemoryStore
	taskStore *tasks.MemoryStore
	server    *Server
}

func newTestEnv() *testEnv {
	now := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	tel := catalog.NewMemoryTelemetry()
	tel.Set("VCL1", "window.rear_left", "open", now.Add(-2*time.Second))
	cat := catalog.NewService(tel)
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

func (e *testEnv) registerTurn(t *testing.T, scope contract.Scope, reqID, handle, hermesSession, text string, candidates ...contract.CandidateVersion) {
	t.Helper()
	err := e.turnStore.Register(context.Background(), turns.Record{
		Context: contract.TurnContext{
			VehicleTurn: contract.VehicleTurn{
				Scope:           scope,
				VoiceTurnID:     "VT-" + reqID,
				RequestID:       reqID,
				FinalTranscript: contract.FinalTranscript{ProviderSessionID: "Q1", InputItemID: "I-" + reqID, Text: text},
			},
			HermesSessionID: hermesSession,
			TurnHandle:      handle,
			Candidates:      candidates,
			ExpiresAt:       e.now.Add(5 * time.Minute),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (e *testEnv) callTool(t *testing.T, token, handle, sessionID, toolName string, args map[string]any, meta map[string]any) (int, bool, json.RawMessage) {
	t.Helper()
	params := map[string]any{"name": toolName, "arguments": args}
	if meta != nil {
		params["_meta"] = meta
	}
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": params,
	})
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if handle != "" {
		req.Header.Set(contract.TurnHandleHeader, handle)
	}
	if sessionID != "" {
		req.Header.Set(contract.HermesSessionHeader, sessionID)
	}
	rr := httptest.NewRecorder()
	e.server.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		return rr.Code, true, rr.Body.Bytes()
	}
	var rpcResp struct {
		Result struct {
			IsError           bool            `json:"isError"`
			StructuredContent json.RawMessage `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &rpcResp); err != nil {
		t.Fatalf("decode rpc: %v body=%s", err, rr.Body.String())
	}
	return rr.Code, rpcResp.Result.IsError, rpcResp.Result.StructuredContent
}

func decodeBizErr(t *testing.T, raw json.RawMessage) contract.BusinessError {
	t.Helper()
	var be contract.BusinessError
	if err := json.Unmarshal(raw, &be); err != nil {
		t.Fatalf("decode business error: %v raw=%s", err, string(raw))
	}
	return be
}

func TestToolsListAndAuthAndTrustedMetadataGuards(t *testing.T) {
	env := newTestEnv()
	scope := contract.Scope{UserID: "U1", SessionID: "S1", VehicleID: "VCL1"}
	env.registerTurn(t, scope, "R1", "TH1", "HS1", "把左后窗关上")

	// Missing service bearer -> 401.
	code, _, _ := env.callTool(t, "", "TH1", "HS1", contract.DispatchVehicleCommand, map[string]any{
		"device": "window", "tool": "close", "params": map[string]any{"position": "rear_left"},
	}, nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", code)
	}

	// Missing trusted headers -> TURN_CONTEXT_INVALID.
	_, isErr, raw := env.callTool(t, "svc-secret", "", "", contract.DispatchVehicleCommand, map[string]any{
		"device": "window", "tool": "close", "params": map[string]any{"position": "rear_left"},
	}, nil)
	if !isErr || decodeBizErr(t, raw).Code != contract.TurnContextInvalid {
		t.Fatalf("expected TURN_CONTEXT_INVALID, got isErr=%v raw=%s", isErr, string(raw))
	}

	// Mismatched header vs _meta -> TURN_CONTEXT_INVALID.
	_, isErr, raw = env.callTool(t, "svc-secret", "TH1", "HS1", contract.DispatchVehicleCommand, map[string]any{
		"device": "window", "tool": "close", "params": map[string]any{"position": "rear_left"},
	}, map[string]any{MetaTurnHandleKey: "TH-FORGED", MetaHermesSessionKey: "HS1"})
	if !isErr || decodeBizErr(t, raw).Code != contract.TurnContextInvalid {
		t.Fatalf("expected TURN_CONTEXT_INVALID for _meta mismatch, got %s", string(raw))
	}

	// Smuggled identity field in tool arguments -> INVALID_ARGUMENT.
	_, isErr, raw = env.callTool(t, "svc-secret", "TH1", "HS1", contract.DispatchVehicleCommand, map[string]any{
		"device": "window", "tool": "close", "params": map[string]any{"position": "rear_left"}, "user_id": "U2",
	}, nil)
	if !isErr || decodeBizErr(t, raw).Code != contract.InvalidArgument {
		t.Fatalf("expected INVALID_ARGUMENT for smuggled user_id, got %s", string(raw))
	}

	// tools/list returns all 6 contract tools with readOnlyHint annotations.
	listReq := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)))
	listReq.Header.Set("Authorization", "Bearer svc-secret")
	rr := httptest.NewRecorder()
	env.server.ServeHTTP(rr, listReq)
	var listResp struct {
		Result struct {
			Tools []struct {
				Name        string          `json:"name"`
				Annotations map[string]bool `json:"annotations"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &listResp); err != nil || len(listResp.Result.Tools) != 6 {
		t.Fatalf("expected 6 tools from tools/list, err=%v count=%d", err, len(listResp.Result.Tools))
	}
}

func TestScenarioAFollowUpAndRetryIdempotency(t *testing.T) {
	env := newTestEnv()
	scope := contract.Scope{UserID: "U1", SessionID: "S1", VehicleID: "VCL1"}

	// Turn 1: "把车窗关一下" -> create_vehicle_task.
	env.registerTurn(t, scope, "R1", "TH1", "HS1", "把车窗关一下")
	_, isErr, raw := env.callTool(t, "svc-secret", "TH1", "HS1", contract.CreateVehicleTask, map[string]any{
		"device": "window", "tool": "close", "params": map[string]any{},
	}, map[string]any{MetaTurnHandleKey: "TH1", MetaHermesSessionKey: "HS1"})
	if isErr {
		t.Fatalf("turn 1 create failed: %s", string(raw))
	}
	var t1 contract.TaskResult
	_ = json.Unmarshal(raw, &t1)
	if t1.Status != contract.PendingReply || t1.TaskID == "" {
		t.Fatalf("unexpected create result: %+v", t1)
	}

	// Turn 2: "左后窗" -> short reply omitting task_id MUST BE REJECTED (not downgraded to new command!).
	env.registerTurn(t, scope, "R2", "TH2", "HS1", "左后窗", contract.CandidateVersion{TaskID: t1.TaskID, Version: 1})
	_, isErr, raw = env.callTool(t, "svc-secret", "TH2", "HS1", contract.DispatchVehicleCommand, map[string]any{
		"device": "window", "tool": "close", "params": map[string]any{"position": "rear_left"},
	}, nil)
	if !isErr || decodeBizErr(t, raw).Code != contract.ClarificationRequired {
		t.Fatalf("short reply omitting task_id must be rejected with CLARIFICATION_REQUIRED, got isErr=%v raw=%s", isErr, string(raw))
	}

	// Same turn 2 with matching task_id succeeds!
	_, isErr, raw = env.callTool(t, "svc-secret", "TH2", "HS1", contract.DispatchVehicleCommand, map[string]any{
		"task_id": t1.TaskID, "device": "window", "tool": "close", "params": map[string]any{"position": "rear_left"},
	}, nil)
	if isErr {
		t.Fatalf("expected resume dispatch success, got %s", string(raw))
	}
	var d1 contract.TaskResult
	_ = json.Unmarshal(raw, &d1)
	if d1.TaskID != t1.TaskID || d1.CommandID == "" || d1.Status != contract.PendingDispatch {
		t.Fatalf("unexpected dispatch result: %+v", d1)
	}

	// Exact retry within the SAME turn recovers the exact persisted TaskResult (same command_id)!
	_, isErr, rawRetry := env.callTool(t, "svc-secret", "TH2", "HS1", contract.DispatchVehicleCommand, map[string]any{
		"task_id": t1.TaskID, "device": "window", "tool": "close", "params": map[string]any{"position": "rear_left"},
	}, nil)
	if isErr {
		t.Fatalf("exact retry should succeed from idempotency record: %s", string(rawRetry))
	}
	var dRetry contract.TaskResult
	_ = json.Unmarshal(rawRetry, &dRetry)
	if dRetry.CommandID != d1.CommandID || dRetry.TaskID != d1.TaskID {
		t.Fatalf("retry returned different IDs: first=%+v retry=%+v", d1, dRetry)
	}
}

func TestWriteGuardsQueryNegationAmbiguityCrossScopeAndCancel(t *testing.T) {
	env := newTestEnv()
	scope1 := contract.Scope{UserID: "U1", SessionID: "S1", VehicleID: "VCL1"}
	scope2 := contract.Scope{UserID: "U2", SessionID: "S2", VehicleID: "VCL2"}

	// 1. Read-only query ("左后窗开着吗？") cannot trigger write.
	env.registerTurn(t, scope1, "RQ", "THQ", "HS1", "左后窗开着吗？")
	_, isErr, raw := env.callTool(t, "svc-secret", "THQ", "HS1", contract.DispatchVehicleCommand, map[string]any{
		"device": "window", "tool": "close", "params": map[string]any{"position": "rear_left"},
	}, nil)
	if !isErr || decodeBizErr(t, raw).Code != contract.ClarificationRequired {
		t.Fatalf("query must not write, got %s", string(raw))
	}
	// But get_vehicle_status succeeds!
	_, isErr, rawStatus := env.callTool(t, "svc-secret", "THQ", "HS1", contract.GetVehicleStatus, map[string]any{
		"device": "window", "tool": "get_status", "params": map[string]any{"positions": []string{"rear_left"}},
	}, nil)
	if isErr {
		t.Fatalf("get_vehicle_status failed: %s", string(rawStatus))
	}

	// 2. Negation ("别关左后窗") cannot write.
	env.registerTurn(t, scope1, "RN", "THN", "HS1", "别关左后窗")
	_, isErr, raw = env.callTool(t, "svc-secret", "THN", "HS1", contract.DispatchVehicleCommand, map[string]any{
		"device": "window", "tool": "close", "params": map[string]any{"position": "rear_left"},
	}, nil)
	if !isErr || decodeBizErr(t, raw).Code != contract.ClarificationRequired {
		t.Fatalf("negation must not write, got %s", string(raw))
	}

	// 3. Ambiguity: both window.open and window.close pending -> "左后窗" rejected; "关左后窗" succeeds!
	env.registerTurn(t, scope1, "RC1", "THC1", "HS1", "把车窗关一下")
	_, _, rClose := env.callTool(t, "svc-secret", "THC1", "HS1", contract.CreateVehicleTask, map[string]any{
		"device": "window", "tool": "close", "params": map[string]any{},
	}, nil)
	var tcClose contract.TaskResult
	_ = json.Unmarshal(rClose, &tcClose)

	env.registerTurn(t, scope1, "RO1", "THO1", "HS1", "把车窗打开")
	_, _, rOpen := env.callTool(t, "svc-secret", "THO1", "HS1", contract.CreateVehicleTask, map[string]any{
		"device": "window", "tool": "open", "params": map[string]any{},
	}, nil)
	var tcOpen contract.TaskResult
	_ = json.Unmarshal(rOpen, &tcOpen)

	cands := []contract.CandidateVersion{
		{TaskID: tcOpen.TaskID, Version: 1},
		{TaskID: tcClose.TaskID, Version: 1},
	}
	env.registerTurn(t, scope1, "RAMB", "THAMB", "HS1", "左后窗", cands...)
	_, isErr, raw = env.callTool(t, "svc-secret", "THAMB", "HS1", contract.DispatchVehicleCommand, map[string]any{
		"task_id": tcClose.TaskID, "device": "window", "tool": "close", "params": map[string]any{"position": "rear_left"},
	}, nil)
	if !isErr || decodeBizErr(t, raw).Code != contract.ClarificationRequired {
		t.Fatalf("ambiguous reply across open+close candidates must fail with CLARIFICATION_REQUIRED, got %s", string(raw))
	}

	env.registerTurn(t, scope1, "RDIS", "THDIS", "HS1", "关左后窗", cands...)
	_, isErr, raw = env.callTool(t, "svc-secret", "THDIS", "HS1", contract.DispatchVehicleCommand, map[string]any{
		"task_id": tcClose.TaskID, "device": "window", "tool": "close", "params": map[string]any{"position": "rear_left"},
	}, nil)
	if isErr {
		t.Fatalf("explicit disambiguated reply should succeed, got %s", string(raw))
	}

	// 4. Cross-user / non-candidate task rejected with TASK_NOT_ELIGIBLE.
	env.registerTurn(t, scope2, "RX", "THX", "HS2", "开左后窗")
	_, isErr, raw = env.callTool(t, "svc-secret", "THX", "HS2", contract.DispatchVehicleCommand, map[string]any{
		"task_id": tcOpen.TaskID, "device": "window", "tool": "open", "params": map[string]any{"position": "rear_left"},
	}, nil)
	if !isErr || decodeBizErr(t, raw).Code != contract.TaskNotEligible {
		t.Fatalf("cross-user / non-candidate task must fail with TASK_NOT_ELIGIBLE, got %s", string(raw))
	}

	// 5. Cancelled turn rejected with TURN_CONTEXT_INVALID.
	env.registerTurn(t, scope1, "RCAN", "THCAN", "HS1", "把左前窗关上")
	_ = env.turnStore.Cancel(context.Background(), scope1, "VT-RCAN")
	_, isErr, raw = env.callTool(t, "svc-secret", "THCAN", "HS1", contract.DispatchVehicleCommand, map[string]any{
		"device": "window", "tool": "close", "params": map[string]any{"position": "front_left"},
	}, nil)
	if !isErr || decodeBizErr(t, raw).Code != contract.TurnContextInvalid {
		t.Fatalf("cancelled turn must fail with TURN_CONTEXT_INVALID, got %s", string(raw))
	}

	// 6. Read-only manual, capabilities, and list_vehicle_tasks all work cleanly.
	env.registerTurn(t, scope1, "RREAD", "THREAD", "HS1", "自动关窗在哪里设置")
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{contract.SearchVehicleManual, map[string]any{"query": "自动关窗在哪里设置"}},
		{contract.SearchVehicleCapabilities, map[string]any{"query": "空调"}},
		{contract.ListVehicleTasks, map[string]any{}},
	} {
		_, isErr, raw = env.callTool(t, "svc-secret", "THREAD", "HS1", tc.name, tc.args, nil)
		if isErr {
			t.Fatalf("read tool %s failed: %s", tc.name, string(raw))
		}
	}
}
