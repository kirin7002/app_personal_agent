package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kirin7002/app_personal_agent/contract"
)

func TestDemoServerTextTwoTurnEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	mcpToken := "test-mcp-token"
	hermesKey := "test-hermes-key"
	demoToken := "test-demo-token"

	var (
		mu       sync.Mutex
		mcpURL   string
		taskID   string
		runCount int
	)

	// Minimal Hermes Runs stub that calls the real Go MCP server using the
	// trusted headers forwarded by vehicle/runs.Client.
	hermesStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+hermesKey {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/v1/runs" {
			var req struct {
				SessionID string `json:"session_id"`
				Input     string `json:"input"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			handle := r.Header.Get(contract.TurnHandleHeader)
			hermesSession := r.Header.Get(contract.HermesSessionHeader)

			mu.Lock()
			runCount++
			currentRun := runCount
			targetMCP := mcpURL
			savedTaskID := taskID
			mu.Unlock()

			var (
				toolName string
				args     map[string]any
			)
			if strings.Contains(req.Input, "关一下") {
				toolName = contract.CreateVehicleTask
				args = map[string]any{"device": "window", "tool": "close", "params": map[string]any{}}
			} else {
				toolName = contract.DispatchVehicleCommand
				args = map[string]any{
					"task_id": savedTaskID,
					"device":  "window",
					"tool":    "close",
					"params":  map[string]any{"position": "rear_left"},
				}
			}

			res := callMCPTool(t, targetMCP, mcpToken, handle, hermesSession, toolName, args)
			if toolName == contract.CreateVehicleTask {
				if id, ok := res["task_id"].(string); ok && id != "" {
					mu.Lock()
					taskID = id
					mu.Unlock()
				}
			}

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"run_id": "run_" + string(rune('0'+currentRun)),
				"status": "queued",
			})
			return
		}
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/events") {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "id: 1\nevent: run.completed\ndata: {\"status\":\"completed\",\"output\":\"ok\"}\n\n")
			return
		}
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/runs/") {
			id := strings.TrimPrefix(r.URL.Path, "/v1/runs/")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"run_id": id, "status": "completed", "output": "ok"})
			return
		}
		http.NotFound(w, r)
	}))
	defer hermesStub.Close()

	cfg := config{
		SQLitePath:      filepath.Join(t.TempDir(), "demo.sqlite"),
		HermesURL:       hermesStub.URL,
		HermesAPIKey:    hermesKey,
		MCPServiceToken: mcpToken,
		DemoAuthToken:   demoToken,
		DemoUserID:      "demo-user",
		DemoVehicleID:   "sim-car",
		PromptFile:      filepath.Join("..", "..", "hermes", "prompts", "vehicle_system_prompt.md"),
		SimMode:         "accepted_only",
		TTSMode:         "tone",
		DebugEndpoints:  true,
	}

	app, err := buildApp(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	defer app.Close()

	stopBG := app.startBackground(ctx)
	defer stopBG()

	mcpSrv := httptest.NewServer(app.mcpHandler)
	defer mcpSrv.Close()
	mu.Lock()
	mcpURL = mcpSrv.URL + "/mcp"
	mu.Unlock()

	pubSrv := httptest.NewServer(app.publicHandler)
	defer pubSrv.Close()

	// 1. /healthz
	resp, err := http.Get(pubSrv.URL + "/healthz")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz failed: %v %v", resp, err)
	}
	resp.Body.Close()

	// 2. Turn 1: create pending task via text SSE endpoint
	reply1 := postUtterance(t, pubSrv.URL+"/voice/utterances", demoToken, map[string]string{
		"session_id": "demo-sess",
		"vehicle_id": "sim-car",
		"request_id": "req-1",
		"text":       "把车窗关一下",
	})
	if reply1.Status != contract.PendingReply || reply1.TaskID == "" || !strings.Contains(reply1.Text, "车窗") {
		t.Fatalf("unexpected turn 1 reply: %+v", reply1)
	}

	// 3. Turn 2: resume & dispatch command
	reply2 := postUtterance(t, pubSrv.URL+"/voice/utterances", demoToken, map[string]string{
		"session_id": "demo-sess",
		"vehicle_id": "sim-car",
		"request_id": "req-2",
		"text":       "左后窗",
	})
	if reply2.TaskID != reply1.TaskID || reply2.CommandID == "" ||
		(reply2.Status != contract.PendingDispatch && reply2.Status != contract.Executing) {
		t.Fatalf("unexpected turn 2 reply: %+v", reply2)
	}

	// Wait for executor worker to pick up the command into executing.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		cmd, err := app.taskStore.GetCommand(ctx, reply2.CommandID)
		if err == nil && cmd.Status == contract.Executing {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// 4. Inject completion receipt via /debug/receipt
	recBody, _ := json.Marshal(map[string]string{
		"command_id": reply2.CommandID,
		"status":     "completed",
	})
	recReq, _ := http.NewRequest(http.MethodPost, pubSrv.URL+"/debug/receipt", bytes.NewReader(recBody))
	recReq.Header.Set("Authorization", "Bearer "+demoToken)
	recReq.Header.Set("Content-Type", "application/json")
	recResp, err := http.DefaultClient.Do(recReq)
	if err != nil || recResp.StatusCode != http.StatusOK {
		t.Fatalf("debug receipt failed: resp=%v err=%v", recResp, err)
	}
	recResp.Body.Close()

	// 5. Verify /debug/state shows closed window telemetry & completed result event.
	stateReq, _ := http.NewRequest(http.MethodGet, pubSrv.URL+"/debug/state?session_id=demo-sess&vehicle_id=sim-car", nil)
	stateReq.Header.Set("Authorization", "Bearer "+demoToken)
	stateResp, err := http.DefaultClient.Do(stateReq)
	if err != nil || stateResp.StatusCode != http.StatusOK {
		t.Fatalf("debug state failed: resp=%v err=%v", stateResp, err)
	}
	defer stateResp.Body.Close()
	var state struct {
		Telemetry map[string]struct {
			Value any `json:"Value"`
		} `json:"telemetry"`
		VehicleResults []contract.VehicleResultEvent `json:"vehicle_results"`
	}
	if err := json.NewDecoder(stateResp.Body).Decode(&state); err != nil {
		t.Fatalf("decode debug state: %v", err)
	}
	if got := state.Telemetry["window.rear_left"].Value; got != "closed" {
		t.Fatalf("expected rear_left window closed, got %v", got)
	}
	if len(state.VehicleResults) != 1 || state.VehicleResults[0].Status != contract.Completed {
		t.Fatalf("expected 1 completed result event, got %+v", state.VehicleResults)
	}
}

func TestToneTTSProducesValidPCM(t *testing.T) {
	tts, name, err := selectDemoTTS("tone", nil)
	if err != nil || name != "tone" {
		t.Fatalf("selectDemoTTS(tone): name=%q err=%v", name, err)
	}
	var total int
	err = tts.Synthesize(context.Background(), "测试", func(pcm []byte) error {
		if len(pcm) == 0 || len(pcm)%2 != 0 {
			t.Fatalf("invalid pcm chunk length: %d", len(pcm))
		}
		total += len(pcm)
		return nil
	})
	if err != nil || total == 0 {
		t.Fatalf("Synthesize failed: total=%d err=%v", total, err)
	}
}

func postUtterance(t *testing.T, url, token string, payload map[string]string) contract.SpeechReply {
	t.Helper()
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post utterance: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("utterance HTTP %d: %s", resp.StatusCode, string(raw))
	}
	var currentEvent string
	for _, line := range strings.Split(string(raw), "\n") {
		if ev, ok := strings.CutPrefix(line, "event: "); ok {
			currentEvent = strings.TrimSpace(ev)
			continue
		}
		if data, ok := strings.CutPrefix(line, "data: "); ok && (currentEvent == "" || currentEvent == "speech.reply") {
			var reply contract.SpeechReply
			if err := json.Unmarshal([]byte(data), &reply); err != nil {
				t.Fatalf("decode SSE reply: %v", err)
			}
			return reply
		}
	}
	t.Fatalf("missing SSE data line in response: %s", string(raw))
	return contract.SpeechReply{}
}

func callMCPTool(t *testing.T, url, mcpToken, handle, hermesSession, tool string, args map[string]any) map[string]any {
	t.Helper()
	rpc := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      tool,
			"arguments": args,
			"_meta": map[string]string{
				"vehicle.turn_handle":       handle,
				"vehicle.hermes_session_id": hermesSession,
			},
		},
	}
	body, _ := json.Marshal(rpc)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+mcpToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	req.Header.Set(contract.TurnHandleHeader, handle)
	req.Header.Set(contract.HermesSessionHeader, hermesSession)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("mcp call %s: %v", tool, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mcp call %s HTTP %d: %s", tool, resp.StatusCode, string(raw))
	}
	var decoded struct {
		Result struct {
			IsError           bool           `json:"isError"`
			StructuredContent map[string]any `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode mcp response: %v (%s)", err, string(raw))
	}
	if decoded.Result.IsError {
		t.Fatalf("mcp call %s returned business error: %s", tool, string(raw))
	}
	return decoded.Result.StructuredContent
}
