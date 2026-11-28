package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kirin7002/app_personal_agent/contract"
	"github.com/kirin7002/app_personal_agent/vehicle/turns"
	"github.com/kirin7002/app_personal_agent/voice/gateway"
)

type parsedSSE struct {
	ID    string
	Event string
	Data  string
}

func readSSEUntil(t *testing.T, r io.Reader, wantCount int, timeout time.Duration) []parsedSSE {
	t.Helper()
	ch := make(chan parsedSSE, wantCount+4)
	go func() {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 4096), 1<<20)
		var cur parsedSSE
		for sc.Scan() {
			line := sc.Text()
			if line == "" {
				if cur.Event != "" || cur.Data != "" || cur.ID != "" {
					ch <- cur
					cur = parsedSSE{}
				}
				continue
			}
			switch {
			case strings.HasPrefix(line, "id: "):
				cur.ID = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				cur.Event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				cur.Data += strings.TrimPrefix(line, "data: ")
			}
		}
		close(ch)
	}()

	var out []parsedSSE
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for len(out) < wantCount {
		select {
		case f, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, f)
		case <-timer.C:
			return out
		}
	}
	return out
}

func TestEventBusScopeIsolationMonotonicSeqAndSanitizer(t *testing.T) {
	bus := NewEventBus(10)
	bus.AddSecrets("super-secret-hermes-key", "super-secret-mcp-token", "mysql://root:pw@127.0.0.1/db")

	scopeA := contract.Scope{UserID: "U1", SessionID: "S1", VehicleID: "car-A"}
	scopeB := contract.Scope{UserID: "U1", SessionID: "S2", VehicleID: "car-A"}

	_, chA, _, _, cancelA := bus.Subscribe(scopeA, 0)
	defer cancelA()
	_, chB, _, _, cancelB := bus.Subscribe(scopeB, 0)
	defer cancelB()

	evA1 := bus.Publish(scopeA, "process.step", map[string]any{
		"turn_id":                   "T1",
		"request_id":                "R1",
		"turn_handle":               "SHOULD-BE-STRIPPED-HANDLE",
		"X-Vehicle-Turn-Handle":     "SHOULD-BE-STRIPPED-HEADER",
		"Authorization":             "Bearer leak-bearer-12345",
		"DASHSCOPE_API_KEY":         "dash-secret",
		"HERMES_API_KEY":            "hermes-secret",
		"VEHICLE_MCP_SERVICE_TOKEN": "mcp-secret",
		"vehicle_demo_session":      "cookie-secret",
		"mysql_dsn":                 "mysql://root:pw@127.0.0.1/db",
		"step": map[string]any{
			"step_id": "s1",
			"stage":   "mcp.tool",
			"detail":  "error contacting super-secret-hermes-key with Bearer inlineToken999",
			"observation": map[string]any{
				"input": map[string]any{
					"position":    "rear_left",
					"turn_handle": "NESTED-HANDLE",
					"api_key":     "NESTED-API-KEY",
				},
				"output": "using super-secret-mcp-token and mysql://root:pw@127.0.0.1/db",
			},
		},
	})
	evA2 := bus.Publish(scopeA, "message.upsert", map[string]any{
		"message_id": "user:T1",
		"turn_id":    "T1",
		"role":       "user",
		"text":       "把车窗关一下",
	})
	evB1 := bus.Publish(scopeB, "message.upsert", map[string]any{
		"message_id": "user:TB",
		"turn_id":    "TB",
		"role":       "user",
		"text":       "打开空调",
	})

	if evA1.Seq != 1 || evA2.Seq != 2 || evB1.Seq != 1 {
		t.Fatalf("unexpected per-scope seqs: A1=%d A2=%d B1=%d", evA1.Seq, evA2.Seq, evB1.Seq)
	}

	// Verify strict sanitization: zero forbidden keys or secret substrings.
	raw := string(evA1.Data)
	for _, forbidden := range []string{
		"SHOULD-BE-STRIPPED-HANDLE",
		"SHOULD-BE-STRIPPED-HEADER",
		"leak-bearer-12345",
		"dash-secret",
		"hermes-secret",
		"mcp-secret",
		"cookie-secret",
		"super-secret-hermes-key",
		"super-secret-mcp-token",
		"mysql://root:pw@127.0.0.1/db",
		"NESTED-HANDLE",
		"NESTED-API-KEY",
		"inlineToken999",
		"turn_handle",
	} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("forbidden secret/key %q leaked in sanitized SSE payload: %s", forbidden, raw)
		}
	}
	if !strings.Contains(raw, "rear_left") {
		t.Fatalf("legitimate business parameter stripped: %s", raw)
	}

	// Verify strict cross-Scope isolation.
	gotA1 := <-chA
	gotA2 := <-chA
	if gotA1.Seq != 1 || gotA2.Seq != 2 {
		t.Fatalf("unexpected events on scopeA: %+v %+v", gotA1, gotA2)
	}
	select {
	case unexpected := <-chA:
		t.Fatalf("cross-scope leak into scopeA: %+v", unexpected)
	default:
	}

	gotB1 := <-chB
	if gotB1.Seq != 1 || !strings.Contains(string(gotB1.Data), "打开空调") {
		t.Fatalf("unexpected event on scopeB: %+v", gotB1)
	}
	select {
	case unexpected := <-chB:
		t.Fatalf("cross-scope leak into scopeB: %+v", unexpected)
	default:
	}
}

func TestEventBusSSEReplayResetAndPing(t *testing.T) {
	bus := NewEventBus(3)
	bus.pingInterval = 40 * time.Millisecond
	scope := contract.Scope{UserID: "U1", SessionID: "S1", VehicleID: "sim-car"}

	for i := 1; i <= 5; i++ {
		bus.Publish(scope, "message.upsert", map[string]any{
			"message_id": "m:" + strconv.Itoa(i),
			"role":       "user",
			"text":       "msg-" + strconv.Itoa(i),
		})
	}
	// Ring buffer capacity is 3 -> retains seqs [3, 4, 5].

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bus.ServeSSE(w, r, scope)
	}))
	defer srv.Close()

	// 1. Last-Event-ID=3 (higher priority than ?after=1) replays [4, 5] and then sends ping.
	ctx1, cancel1 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel1()
	req1, _ := http.NewRequestWithContext(ctx1, http.MethodGet, srv.URL+"?after=1", nil)
	req1.Header.Set("Last-Event-ID", "3")
	resp1, err := http.DefaultClient.Do(req1)
	if err != nil {
		t.Fatal(err)
	}
	if resp1.Header.Get("X-Accel-Buffering") != "no" || !strings.HasPrefix(resp1.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("missing SSE headers: %v", resp1.Header)
	}
	frames1 := readSSEUntil(t, resp1.Body, 3, time.Second)
	cancel1()
	resp1.Body.Close()

	if len(frames1) < 3 || frames1[0].ID != "4" || frames1[1].ID != "5" || frames1[2].Event != "ping" {
		t.Fatalf("expected replay [4,5] + ping, got %+v", frames1)
	}

	// 2. after=1 (seq 2 was evicted!) triggers event: stream.reset with cursor=5.
	req2, _ := http.NewRequest(http.MethodGet, srv.URL+"?after=1", nil)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	frames2 := readSSEUntil(t, resp2.Body, 1, time.Second)
	resp2.Body.Close()
	if len(frames2) != 1 || frames2[0].Event != "stream.reset" || !strings.Contains(frames2[0].Data, `"cursor":5`) {
		t.Fatalf("expected stream.reset(cursor=5) on evicted cursor, got %+v", frames2)
	}

	// 3. after=99 (ahead of server after restart) also triggers stream.reset.
	req3, _ := http.NewRequest(http.MethodGet, srv.URL+"?after=99", nil)
	resp3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatal(err)
	}
	frames3 := readSSEUntil(t, resp3.Body, 1, time.Second)
	resp3.Body.Close()
	if len(frames3) != 1 || frames3[0].Event != "stream.reset" || !strings.Contains(frames3[0].Data, `"cursor":5`) {
		t.Fatalf("expected stream.reset(cursor=5) on future cursor, got %+v", frames3)
	}
}

func TestDualSSEChannelsTwoTurnWindowClose(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	mcpToken := "mcp-tok-dual"
	hermesKey := "hermes-key-dual"
	demoToken := "demo-tok-dual"

	var (
		mu       sync.Mutex
		mcpURL   string
		taskID   string
		runCount int
	)

	hermesStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/runs" {
			var req struct {
				Input string `json:"input"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			handle := r.Header.Get(contract.TurnHandleHeader)
			hermesSession := r.Header.Get(contract.HermesSessionHeader)

			mu.Lock()
			runCount++
			cur := runCount
			targetMCP := mcpURL
			savedTask := taskID
			mu.Unlock()

			toolName := contract.CreateVehicleTask
			args := map[string]any{"device": "window", "tool": "close", "params": map[string]any{}}
			if !strings.Contains(req.Input, "关一下") {
				toolName = contract.DispatchVehicleCommand
				args = map[string]any{"task_id": savedTask, "device": "window", "tool": "close", "params": map[string]any{"position": "rear_left"}}
			}
			res := callMCPTool(t, targetMCP, mcpToken, handle, hermesSession, toolName, args)
			if toolName == contract.CreateVehicleTask {
				if id, _ := res["task_id"].(string); id != "" {
					mu.Lock()
					taskID = id
					mu.Unlock()
				}
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{"run_id": "run_dual_" + strconv.Itoa(cur), "status": "queued"})
			return
		}
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/events") {
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/runs/"), "/events")
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"event\":\"tool.completed\",\"run_id\":\""+id+"\",\"tool\":\"create_vehicle_task\",\"seq\":1}\n\n")
			_, _ = io.WriteString(w, "data: {\"event\":\"run.completed\",\"run_id\":\""+id+"\",\"status\":\"completed\",\"output\":\"ok\",\"seq\":2}\n\n")
			return
		}
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/runs/") {
			id := strings.TrimPrefix(r.URL.Path, "/v1/runs/")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"run_id": id, "status": "running"})
			return
		}
		http.NotFound(w, r)
	}))
	defer hermesStub.Close()

	cfg := config{
		SQLitePath:      filepath.Join(t.TempDir(), "dual-sse.sqlite"),
		HermesURL:       hermesStub.URL,
		HermesAPIKey:    hermesKey,
		MCPServiceToken: mcpToken,
		DemoAuthToken:   demoToken,
		DemoUserID:      "demo-user",
		DemoVehicleID:   "sim-car",
		PromptFile:      filepath.Join("..", "..", "hermes", "prompts", "vehicle_system_prompt.md"),
		SimMode:         "auto_complete",
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

	// Subscribe to session-wide GET /api/events/stream in background.
	sseCtx, sseCancel := context.WithCancel(ctx)
	defer sseCancel()
	sseReq, _ := http.NewRequestWithContext(sseCtx, http.MethodGet, pubSrv.URL+"/api/events/stream?session_id=dual-sess&vehicle_id=sim-car", nil)
	sseReq.Header.Set("Authorization", "Bearer "+demoToken)
	sseResp, err := http.DefaultClient.Do(sseReq)
	if err != nil || sseResp.StatusCode != http.StatusOK {
		t.Fatalf("open /api/events/stream failed: resp=%v err=%v", sseResp, err)
	}
	defer sseResp.Body.Close()

	var (
		sseMu     sync.Mutex
		sseEvents []parsedSSE
	)
	go func() {
		sc := bufio.NewScanner(sseResp.Body)
		sc.Buffer(make([]byte, 4096), 1<<20)
		var cur parsedSSE
		for sc.Scan() {
			line := sc.Text()
			if line == "" {
				if cur.Event != "" {
					sseMu.Lock()
					sseEvents = append(sseEvents, cur)
					sseMu.Unlock()
					cur = parsedSSE{}
				}
				continue
			}
			switch {
			case strings.HasPrefix(line, "id: "):
				cur.ID = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				cur.Event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				cur.Data += strings.TrimPrefix(line, "data: ")
			}
		}
	}()

	// Turn 1: POST /voice/utterances must stream process.step frames (including mcp.tool!) before speech.reply.
	raw1 := postUtteranceRaw(t, pubSrv.URL+"/voice/utterances", demoToken, map[string]string{
		"session_id": "dual-sess", "vehicle_id": "sim-car", "request_id": "dual-r1", "text": "把车窗关一下",
	})
	if !strings.Contains(raw1, "event: process.step") || !strings.Contains(raw1, `"stage":"mcp.tool"`) || !strings.Contains(raw1, "event: speech.reply") {
		t.Fatalf("expected process.step (including mcp.tool) and speech.reply in Turn 1 stream:\n%s", raw1)
	}
	if strings.Contains(raw1, mcpToken) || strings.Contains(raw1, hermesKey) || strings.Contains(raw1, demoToken) {
		t.Fatalf("secret leaked in POST /voice/utterances stream:\n%s", raw1)
	}

	// Turn 2: dispatch window.close -> executor auto_complete -> vehicle.result & telemetry.updated on /api/events/stream.
	raw2 := postUtteranceRaw(t, pubSrv.URL+"/voice/utterances", demoToken, map[string]string{
		"session_id": "dual-sess", "vehicle_id": "sim-car", "request_id": "dual-r2", "text": "左后窗",
	})
	if !strings.Contains(raw2, "event: speech.reply") {
		t.Fatalf("missing speech.reply in Turn 2:\n%s", raw2)
	}

	// Wait until session SSE stream receives vehicle.result and telemetry.updated.
	deadline := time.Now().Add(4 * time.Second)
	var gotResult, gotTelemetry, gotStep, gotMsg, gotTask bool
	for time.Now().Before(deadline) {
		sseMu.Lock()
		for _, ev := range sseEvents {
			switch ev.Event {
			case "process.step":
				gotStep = true
			case "message.upsert":
				gotMsg = true
			case "task.updated":
				gotTask = true
			case "vehicle.result":
				gotResult = true
			case "telemetry.updated":
				gotTelemetry = true
			}
		}
		sseMu.Unlock()
		if gotResult && gotTelemetry && gotStep && gotMsg && gotTask {
			break
		}
		time.Sleep(40 * time.Millisecond)
	}
	sseCancel()

	if !gotStep || !gotMsg || !gotTask || !gotResult || !gotTelemetry {
		t.Fatalf("missing expected session SSE events: step=%v msg=%v task=%v result=%v telemetry=%v",
			gotStep, gotMsg, gotTask, gotResult, gotTelemetry)
	}
}

func postUtteranceRaw(t *testing.T, url, token string, payload map[string]string) string {
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
	return string(raw)
}

func decodeStepPayload(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	step, ok := env["step"].(map[string]any)
	if !ok {
		t.Fatalf("missing step in envelope: %s", string(raw))
	}
	return step
}

func TestHandleHermesStreamEventPairsStartAndCompleteWithCallID(t *testing.T) {
	bus := NewEventBus(32)
	scope := contract.Scope{UserID: "U1", SessionID: "S1", VehicleID: "sim-car"}
	bus.BindRun("run_pair_1", scope, "T1", "R1", "voice")

	_, ch, _, _, cancel := bus.Subscribe(scope, 0)
	defer cancel()

	bus.HandleHermesStreamEvent("run_pair_1", "tool.started", json.RawMessage(`{
		"event": "tool.started",
		"run_id": "run_pair_1",
		"tool": "mcp__vehicle__search_vehicle_capabilities",
		"tool_call_id": "toolu_cd4a984901f54c0da93ffc75",
		"preview": "车控能力 空调 车窗"
	}`))

	time.Sleep(15 * time.Millisecond)

	bus.HandleHermesStreamEvent("run_pair_1", "tool.completed", json.RawMessage(`{
		"event": "tool.completed",
		"run_id": "run_pair_1",
		"tool": "mcp__vehicle__search_vehicle_capabilities",
		"tool_call_id": "toolu_cd4a984901f54c0da93ffc75",
		"duration": 0,
		"error": false,
		"preview": ""
	}`))

	evStart := <-ch
	evDone := <-ch

	stepStart := decodeStepPayload(t, evStart.Data)
	stepDone := decodeStepPayload(t, evDone.Data)

	wantStepID := "hermes:tool:toolu_cd4a984901f54c0da93ffc75"
	if stepStart["step_id"] != wantStepID || stepDone["step_id"] != wantStepID {
		t.Fatalf("expected identical step_id %q, got start=%v done=%v", wantStepID, stepStart["step_id"], stepDone["step_id"])
	}
	if stepStart["status"] != "running" || stepStart["finished_at"] != nil || stepStart["duration_ms"] != nil {
		t.Fatalf("unexpected start step: %+v", stepStart)
	}
	if stepDone["status"] != "done" {
		t.Fatalf("expected done status, got %+v", stepDone)
	}
	if stepDone["started_at"] != stepStart["started_at"] {
		t.Fatalf("expected started_at preserved: start=%v done=%v", stepStart["started_at"], stepDone["started_at"])
	}
	if stepDone["finished_at"] == nil {
		t.Fatalf("expected non-nil finished_at: %+v", stepDone)
	}
	dur, ok := stepDone["duration_ms"].(float64)
	if !ok || dur < 10 {
		t.Fatalf("expected measured duration_ms >= 10, got %v (%T)", stepDone["duration_ms"], stepDone["duration_ms"])
	}
	obs, _ := stepDone["observation"].(map[string]any)
	input, _ := obs["input"].(map[string]any)
	if input["preview"] != "车控能力 空调 车窗" {
		t.Fatalf("expected input preview preserved in completed observation, got %+v", obs)
	}

	// Snapshot must deduplicate to 1 completed step with zero running steps left behind.
	_, _, snapSteps := bus.Snapshot(scope)
	if len(snapSteps) != 1 {
		t.Fatalf("expected 1 deduplicated step in snapshot, got %d", len(snapSteps))
	}
	finalStep := decodeStepPayload(t, snapSteps[0])
	if finalStep["status"] != "done" || finalStep["step_id"] != wantStepID {
		t.Fatalf("unexpected snapshot step: %+v", finalStep)
	}
}

func TestHandleHermesStreamEventPairsWithoutCallIDErrorFlagAndSubagent(t *testing.T) {
	bus := NewEventBus(32)
	scope := contract.Scope{UserID: "U1", SessionID: "S2", VehicleID: "sim-car"}
	bus.BindRun("run_fifo_1", scope, "T2", "R2", "text")

	// Two calls to the same tool without call_id -> FIFO pairing, second returns error=true.
	bus.HandleHermesStreamEvent("run_fifo_1", "tool.started", json.RawMessage(`{"tool":"mcp__vehicle__get_vehicle_status"}`))
	bus.HandleHermesStreamEvent("run_fifo_1", "tool.started", json.RawMessage(`{"tool":"mcp__vehicle__get_vehicle_status"}`))
	bus.HandleHermesStreamEvent("run_fifo_1", "tool.completed", json.RawMessage(`{"tool":"mcp__vehicle__get_vehicle_status","error":false}`))
	bus.HandleHermesStreamEvent("run_fifo_1", "tool.completed", json.RawMessage(`{"tool":"mcp__vehicle__get_vehicle_status","error":true}`))

	// Subagent lifecycle uses Hermes's actual event names: subagent.start -> subagent.complete.
	bus.HandleHermesStreamEvent("run_fifo_1", "subagent.start", json.RawMessage(`{"name":"researcher"}`))
	bus.HandleHermesStreamEvent("run_fifo_1", "subagent.complete", json.RawMessage(`{"name":"researcher"}`))

	_, _, snapSteps := bus.Snapshot(scope)
	if len(snapSteps) != 3 {
		t.Fatalf("expected 3 deduplicated steps (2 tools + 1 subagent), got %d", len(snapSteps))
	}

	s0 := decodeStepPayload(t, snapSteps[0])
	s1 := decodeStepPayload(t, snapSteps[1])
	s2 := decodeStepPayload(t, snapSteps[2])

	if s0["status"] != "done" || s0["duration_ms"] == nil {
		t.Fatalf("expected first tool done with duration_ms, got %+v", s0)
	}
	if s1["status"] != "error" || s1["duration_ms"] == nil {
		t.Fatalf("expected second tool error with duration_ms, got %+v", s1)
	}
	if s0["step_id"] == s1["step_id"] {
		t.Fatalf("expected distinct step_ids for two calls, got %v", s0["step_id"])
	}
	if s2["status"] != "done" || s2["duration_ms"] == nil || !strings.HasPrefix(s2["step_id"].(string), "hermes:subagent:") {
		t.Fatalf("expected subagent step done with hermes:subagent: prefix, got %+v", s2)
	}
}

func TestHandleTurnEventReconcilesOrphanHermesSteps(t *testing.T) {
	bus := NewEventBus(32)
	scope := contract.Scope{UserID: "U1", SessionID: "S3", VehicleID: "sim-car"}
	runID := "run_orphan_1"
	started := time.Now().UTC().Add(-50 * time.Millisecond)

	bus.HandleTurnEvent(turns.TurnEvent{
		Scope:        scope,
		TurnID:       "T3",
		RequestID:    "R3",
		RunID:        runID,
		Channel:      "voice",
		StepID:       "hermes.run",
		Stage:        "hermes.run",
		Title:        "Hermes 意图理解与编排",
		Status:       "running",
		StartedAt:    started,
		DurationKind: "boundary_wait",
	})

	// Tool started, but tool.completed is dropped/never arrives.
	bus.HandleHermesStreamEvent(runID, "tool.started", json.RawMessage(`{
		"tool": "mcp__vehicle__search_vehicle_capabilities",
		"tool_call_id": "toolu_orphan_1",
		"preview": "空调"
	}`))

	time.Sleep(15 * time.Millisecond)
	finished := time.Now().UTC()
	durMs := finished.Sub(started).Milliseconds()
	bus.HandleTurnEvent(turns.TurnEvent{
		Scope:        scope,
		TurnID:       "T3",
		RequestID:    "R3",
		RunID:        runID,
		Channel:      "voice",
		StepID:       "hermes.run",
		Stage:        "hermes.run",
		Title:        "Hermes 意图理解与编排",
		Status:       "done",
		RunStatus:    "completed",
		StartedAt:    started,
		FinishedAt:   &finished,
		DurationMs:   &durMs,
		DurationKind: "boundary_wait",
	})

	// Late tool.started arriving after hermes.run already closed must also not stick in running.
	bus.HandleHermesStreamEvent(runID, "tool.started", json.RawMessage(`{
		"tool": "mcp__vehicle__get_vehicle_status",
		"tool_call_id": "toolu_late_1"
	}`))

	_, _, snapSteps := bus.Snapshot(scope)
	if len(snapSteps) != 3 {
		t.Fatalf("expected 3 steps (hermes.run + orphan + late), got %d", len(snapSteps))
	}
	for _, raw := range snapSteps {
		st := decodeStepPayload(t, raw)
		if st["status"] == "running" {
			t.Fatalf("found stuck running step after hermes.run finished: %+v", st)
		}
		if st["finished_at"] == nil || st["duration_ms"] == nil {
			t.Fatalf("expected non-nil finished_at & duration_ms for %v: %+v", st["step_id"], st)
		}
	}
}

func TestHandleGatewayEventResultPlaybackCorrelation(t *testing.T) {
	bus := NewEventBus(32)
	scope := contract.Scope{UserID: "U1", SessionID: "S4", VehicleID: "sim-car"}
	started := time.Now().UTC().Add(-760 * time.Millisecond)
	finished := time.Now().UTC()
	durMs := int64(761)

	bus.HandleGatewayEvent(gateway.GatewayEvent{
		Scope:          scope,
		RequestID:      "web-req-42",
		TaskID:         "T42",
		CommandID:      "C42",
		Stage:          "voice.playback",
		Status:         "running",
		StartedAt:      started,
		DurationKind:   "boundary_wait",
		PlaybackID:     "result-abc123",
		PlaybackSource: "result",
	})
	bus.HandleGatewayEvent(gateway.GatewayEvent{
		Scope:           scope,
		RequestID:       "web-req-42",
		TaskID:          "T42",
		CommandID:       "C42",
		Stage:           "voice.playback",
		Status:          "done",
		StartedAt:       started,
		FinishedAt:      &finished,
		DurationMs:      &durMs,
		DurationKind:    "boundary_wait",
		PlaybackID:      "result-abc123",
		PlaybackSource:  "result",
		PlaybackReason:  "completed",
		AudioBytes:      60000,
		AudioDurationMs: 1250,
	})

	_, _, snapSteps := bus.Snapshot(scope)
	if len(snapSteps) != 1 {
		t.Fatalf("expected 1 deduplicated playback step, got %d", len(snapSteps))
	}
	var env map[string]any
	if err := json.Unmarshal(snapSteps[0], &env); err != nil {
		t.Fatal(err)
	}
	if env["turn_id"] != "web-req-42" || env["request_id"] != "web-req-42" || env["task_id"] != "T42" || env["command_id"] != "C42" {
		t.Fatalf("expected non-empty correlation IDs on result playback step, got %+v", env)
	}
	st := decodeStepPayload(t, snapSteps[0])
	if st["status"] != "done" || st["duration_ms"].(float64) != 761 {
		t.Fatalf("unexpected final step payload: %+v", st)
	}
}
