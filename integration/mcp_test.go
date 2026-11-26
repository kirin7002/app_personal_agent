package integration

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"testing"

	"github.com/kirin7002/app_personal_agent/contract"
)

type callAudit struct {
	mu    sync.Mutex
	calls []mcpCall
}

type mcpCall struct {
	Tool           string
	MatchedBinding bool
	TaskID         string
	Position       any
}

// wrap captures correlation evidence and leaves authentication, binding and
// mutation checks to the real MCP handler.
func (a *callAudit) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, err := io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
			if err != nil || len(body) > 1<<20 {
				http.Error(w, "cannot inspect test request", http.StatusBadRequest)
				return
			}
			_ = r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(body))
			var req struct {
				Method string `json:"method"`
				Params struct {
					Name     string `json:"name"`
					Metadata struct {
						TurnHandle      string `json:"vehicle.turn_handle"`
						HermesSessionID string `json:"vehicle.hermes_session_id"`
					} `json:"_meta"`
					Arguments contract.TaskMutation `json:"arguments"`
				} `json:"params"`
			}
			if json.Unmarshal(body, &req) == nil && req.Method == "tools/call" {
				handle, session := r.Header.Get(contract.TurnHandleHeader), r.Header.Get(contract.HermesSessionHeader)
				observed := mcpCall{
					Tool: req.Params.Name, TaskID: req.Params.Arguments.TaskID, Position: req.Params.Arguments.Params["position"],
					MatchedBinding: handle != "" && session != "" && handle == req.Params.Metadata.TurnHandle &&
						session == req.Params.Metadata.HermesSessionID,
				}
				a.mu.Lock()
				a.calls = append(a.calls, observed)
				a.mu.Unlock()
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (a *callAudit) assertScenario(t *testing.T, taskID string) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	var writes []mcpCall
	for _, call := range a.calls {
		if !call.MatchedBinding {
			t.Fatalf("MCP %s lacked matching transport binding", call.Tool)
		}
		if call.Tool == contract.CreateVehicleTask || call.Tool == contract.DispatchVehicleCommand {
			writes = append(writes, call)
		}
	}
	if len(writes) != 2 || writes[0].Tool != contract.CreateVehicleTask || writes[0].TaskID != "" ||
		writes[1].Tool != contract.DispatchVehicleCommand || writes[1].TaskID != taskID || writes[1].Position != "rear_left" {
		t.Fatalf("unexpected MCP write sequence: %+v", writes)
	}
	t.Logf("trusted MCP tools: %s → %s(task_id=%s, position=rear_left)", writes[0].Tool, writes[1].Tool, taskID)
}
