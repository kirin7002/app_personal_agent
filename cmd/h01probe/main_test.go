package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kirin7002/app_personal_agent/contract"
)

func postMCP(t *testing.T, handler http.Handler, token, handle, sessionID string, reqBody map[string]any) jsonRPCResponse {
	t.Helper()
	b, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(b))
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
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp jsonRPCResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v body=%s", err, rr.Body.String())
	}
	return resp
}

func TestProbeMCPServerTrustedBindingAndGuards(t *testing.T) {
	srv := newProbeMCPServer("svc-secret")
	now := time.Now().UTC()
	scope := contract.Scope{UserID: "U1", SessionID: "S1", VehicleID: "VCL1"}

	// Callback-before-202: turn handle is registered BEFORE Hermes POST /v1/runs returns.
	srv.RegisterTurn(contract.TurnContext{
		VehicleTurn: contract.VehicleTurn{
			Scope:           scope,
			VoiceTurnID:     "V1",
			RequestID:       "R1",
			FinalTranscript: contract.FinalTranscript{ProviderSessionID: "Q1", InputItemID: "I1", Text: "把车窗关一下"},
		},
		HermesSessionID: "HS1",
		TurnHandle:      "TH-1",
		ExpiresAt:       now.Add(5 * time.Minute),
	})

	// 1. Missing bearer rejected with 401.
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader([]byte(`{}`)))
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without bearer, got %d", rr.Code)
	}

	// 2. Missing trusted headers rejected with TURN_CONTEXT_INVALID.
	resMissing := postMCP(t, srv, "svc-secret", "", "", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name":      contract.CreateVehicleTask,
			"arguments": map[string]any{"device": "window", "tool": "close", "params": map[string]any{}},
		},
	})
	m := resMissing.Result.(map[string]any)
	if m["isError"] != true {
		t.Fatalf("expected isError=true for missing handle: %+v", m)
	}

	// 3. Model-supplied untrusted identity field rejected with INVALID_ARGUMENT.
	resSmuggle := postMCP(t, srv, "svc-secret", "TH-1", "HS1", map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{
			"name":      contract.CreateVehicleTask,
			"arguments": map[string]any{"device": "window", "tool": "close", "params": map[string]any{}, "turn_handle": "FORGED"},
			"_meta":     map[string]any{metaTurnHandleKey: "TH-1", metaHermesSessionKey: "HS1"},
		},
	})
	if resSmuggle.Result.(map[string]any)["isError"] != true {
		t.Fatalf("expected isError=true for smuggled turn_handle")
	}

	// 4. Valid trusted header + matching _meta succeeds and creates T101.
	resCreate := postMCP(t, srv, "svc-secret", "TH-1", "HS1", map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]any{
			"name":      contract.CreateVehicleTask,
			"arguments": map[string]any{"device": "window", "tool": "close", "params": map[string]any{}},
			"_meta":     map[string]any{metaTurnHandleKey: "TH-1", metaHermesSessionKey: "HS1"},
		},
	})
	resMap := resCreate.Result.(map[string]any)
	if resMap["isError"] != false {
		t.Fatalf("expected success creating task, got %+v", resMap)
	}

	// 5. Cancelled turn rejects late calls.
	srv.RegisterTurn(contract.TurnContext{
		VehicleTurn: contract.VehicleTurn{
			Scope:       scope,
			VoiceTurnID: "V2",
			RequestID:   "R2",
		},
		HermesSessionID: "HS1",
		TurnHandle:      "TH-2",
		ExpiresAt:       now.Add(5 * time.Minute),
	})
	srv.CancelTurn("TH-2")
	resCancel := postMCP(t, srv, "svc-secret", "TH-2", "HS1", map[string]any{
		"jsonrpc": "2.0", "id": 4, "method": "tools/call",
		"params": map[string]any{
			"name":      contract.DispatchVehicleCommand,
			"arguments": map[string]any{"device": "window", "tool": "close", "params": map[string]any{"position": "rear_left"}},
			"_meta":     map[string]any{metaTurnHandleKey: "TH-2", metaHermesSessionKey: "HS1"},
		},
	})
	if resCancel.Result.(map[string]any)["isError"] != true {
		t.Fatalf("expected cancelled turn to be rejected")
	}
}
