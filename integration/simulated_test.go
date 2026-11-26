package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kirin7002/app_personal_agent/contract"
	"github.com/kirin7002/app_personal_agent/vehicle/runs"
)

type fixtureMedia struct{}

func (fixtureMedia) Connect(context.Context) (contract.S2SSession, error) {
	return &fixtureSession{}, nil
}

type fixtureSession struct {
	first byte
}

func (s *fixtureSession) AppendAudio(_ context.Context, pcm []byte) error {
	s.first = pcm[0]
	return nil
}

func (s *fixtureSession) Commit(context.Context) (contract.FinalTranscript, error) {
	switch s.first {
	case 1:
		return contract.FinalTranscript{ProviderSessionID: "simulated-Qwen", InputItemID: "item-1", Text: "把车窗关一下"}, nil
	case 2:
		return contract.FinalTranscript{ProviderSessionID: "simulated-Qwen", InputItemID: "item-2", Text: "左后窗"}, nil
	default:
		return contract.FinalTranscript{}, fmt.Errorf("unexpected fixture marker byte %d", s.first)
	}
}

func (*fixtureSession) CreateResponse(context.Context, string) (contract.S2SResponse, error) {
	return nil, fmt.Errorf("vehicle test attempted native response")
}

func (*fixtureSession) CancelResponse(context.Context) error { return nil }
func (*fixtureSession) Close() error                         { return nil }

type fixtureTTS struct{}

func (fixtureTTS) Synthesize(_ context.Context, _ string, emit func([]byte) error) error {
	return emit([]byte{8, 0, 7, 0})
}

// fixtureHermes replaces only the model decision with a deterministic two-turn
// fixture. The Runs client, HTTP MCP handler and result pipeline are unchanged.
func fixtureHermes(t *testing.T, endpoint, serviceToken string) *runs.Client {
	t.Helper()
	var mu sync.Mutex
	results := make(map[string]runs.Run)
	keys := make(map[string]string)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/runs":
			accepted := func(id string) {
				w.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(w).Encode(runs.Run{ID: id, Status: "started"})
			}
			key := r.Header.Get("Idempotency-Key")
			if id := keys[key]; id != "" {
				accepted(id)
				return
			}
			var input runs.StartRequest
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				http.Error(w, "invalid fixture request", http.StatusBadRequest)
				return
			}
			var task contract.TaskMutation
			var tool string
			task.Operation = contract.Operation{Device: "window", Tool: "close"}
			switch input.Input {
			case "把车窗关一下":
				tool, task.Params = contract.CreateVehicleTask, map[string]any{}
			case "左后窗":
				tool, task.Params = contract.DispatchVehicleCommand, map[string]any{"position": "rear_left"}
				_, contextJSON, ok := strings.Cut(input.Instructions, "\n{")
				var data struct {
					Pending []contract.PendingCandidate `json:"pending_tasks"`
				}
				if !ok || json.Unmarshal([]byte("{"+contextJSON), &data) != nil || len(data.Pending) != 1 {
					t.Error("repeat turn missed the pending task context")
					http.Error(w, "missing candidate", http.StatusBadRequest)
					return
				}
				task.TaskID = data.Pending[0].TaskID
			default:
				http.Error(w, "unexpected final transcript", http.StatusBadRequest)
				return
			}

			if err := fakeMCPCall(r.Context(), endpoint, serviceToken, r.Header, tool, task); err != nil {
				t.Error(err)
				http.Error(w, "MCP call failed", http.StatusBadGateway)
				return
			}
			id := fmt.Sprintf("sim_run_%d", len(results)+1)
			results[id] = runs.Run{ID: id, Status: "completed", Output: "已处理"}
			keys[key] = id
			accepted(id)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/runs/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/runs/")
			if result, ok := results[id]; ok {
				_ = json.NewEncoder(w).Encode(result)
				return
			}
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	client, err := runs.New(server.URL, "sim-hermes-api-key", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	client.RetryAdmission = true
	return client
}

func fakeMCPCall(ctx context.Context, endpoint, token string, headers http.Header, tool string, args contract.TaskMutation) error {
	handle, session := headers.Get(contract.TurnHandleHeader), headers.Get(contract.HermesSessionHeader)
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": tool, "arguments": args,
			"_meta": map[string]string{"vehicle.turn_handle": handle, "vehicle.hermes_session_id": session},
		},
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	req.Header.Set(contract.TurnHandleHeader, handle)
	req.Header.Set(contract.HermesSessionHeader, session)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var read struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Result *struct {
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s returned HTTP %d", tool, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&read); err != nil {
		return err
	}
	if read.Error != nil || read.Result == nil || read.Result.IsError {
		return fmt.Errorf("%s returned an MCP error", tool)
	}
	return nil
}
