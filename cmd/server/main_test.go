package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kirin7002/app_personal_agent/contract"
	"github.com/kirin7002/app_personal_agent/realtime/qwen"
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

func TestWebDemoCookieSessionAPIStateCapabilitiesAndStaticHosting(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	mcpToken := "w03-mcp-secret"
	hermesKey := "w03-hermes-secret"
	demoToken := "w03-demo-secret"

	var (
		mu       sync.Mutex
		mcpURL   string
		taskID   string
		runCount int
	)

	hermesStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/health" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"ok"}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+hermesKey {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
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
			_ = json.NewEncoder(w).Encode(map[string]any{"run_id": "run_w03_" + string(rune('0'+cur)), "status": "queued"})
			return
		}
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/events") {
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/runs/"), "/events")
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"event\":\"run.completed\",\"run_id\":\""+id+"\",\"status\":\"completed\",\"output\":\"ok\",\"seq\":1}\n\n")
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

	// Create a temporary -web-dir override directory to verify static hosting without touching cmd/server/web/.
	webDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(webDir, "index.html"), []byte("<!doctype html><title>Web Demo Test</title>"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(webDir, "app.js"), []byte("console.log('web-demo');"), 0600); err != nil {
		t.Fatal(err)
	}

	cfg := config{
		SQLitePath:          filepath.Join(t.TempDir(), "w03.sqlite"),
		HermesURL:           hermesStub.URL,
		HermesAPIKey:        hermesKey,
		MCPServiceToken:     mcpToken,
		DemoAuthToken:       demoToken,
		DemoUserID:          "demo-user",
		DemoVehicleID:       "sim-car",
		PromptFile:          filepath.Join("..", "..", "hermes", "prompts", "vehicle_system_prompt.md"),
		SimMode:             "accepted_only",
		TTSMode:             "tone",
		DebugEndpoints:      true,
		WebDir:              webDir,
		RequireSessionToken: true,
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

	// 1. Static hosting works without auth: GET / and GET /static/app.js; hidden files and unknown /api/* return 404.
	for _, tc := range []struct {
		path string
		want int
		body string
	}{
		{"/", http.StatusOK, "Web Demo Test"},
		{"/static/app.js", http.StatusOK, "web-demo"},
		{"/.gitkeep", http.StatusNotFound, ""},
		{"/api/does-not-exist", http.StatusNotFound, ""},
	} {
		resp, err := http.Get(pubSrv.URL + tc.path)
		if err != nil {
			t.Fatalf("GET %s: %v", tc.path, err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Fatalf("GET %s status=%d want=%d body=%s", tc.path, resp.StatusCode, tc.want, string(raw))
		}
		if tc.body != "" && !strings.Contains(string(raw), tc.body) {
			t.Fatalf("GET %s body=%q missing %q", tc.path, string(raw), tc.body)
		}
	}

	// 2. Unauthenticated requests without Cookie or Bearer must return 401.
	for _, ep := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/config"},
		{http.MethodGet, "/api/capabilities"},
		{http.MethodGet, "/api/state"},
		{http.MethodGet, "/api/events/stream"},
		{http.MethodPost, "/voice/utterances"},
		{http.MethodGet, "/debug/state"},
		{http.MethodPost, "/debug/receipt"},
	} {
		req, _ := http.NewRequest(ep.method, pubSrv.URL+ep.path, strings.NewReader(`{}`))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", ep.method, ep.path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("unauthenticated %s %s expected 401, got %d", ep.method, ep.path, resp.StatusCode)
		}
	}

	// 3. POST /api/session with RequireSessionToken rejects missing/wrong token, then issues HttpOnly SameSite=Strict Cookie.
	badSessResp, err := http.Post(pubSrv.URL+"/api/session", "application/json", strings.NewReader(`{"session_id":"web-s1","token":"wrong"}`))
	if err != nil {
		t.Fatal(err)
	}
	badSessResp.Body.Close()
	if badSessResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for wrong session token, got %d", badSessResp.StatusCode)
	}

	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar}
	sessBody, _ := json.Marshal(map[string]string{
		"session_id": "web-s1",
		"vehicle_id": "sim-car",
		"token":      demoToken,
	})
	sessResp, err := browser.Post(pubSrv.URL+"/api/session", "application/json", bytes.NewReader(sessBody))
	if err != nil {
		t.Fatal(err)
	}
	defer sessResp.Body.Close()
	if sessResp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(sessResp.Body)
		t.Fatalf("POST /api/session status=%d body=%s", sessResp.StatusCode, string(raw))
	}
	var sessionCookie *http.Cookie
	for _, c := range sessResp.Cookies() {
		if c.Name == sessionCookieName {
			sessionCookie = c
		}
	}
	if sessionCookie == nil || !sessionCookie.HttpOnly || sessionCookie.SameSite != http.SameSiteStrictMode || sessionCookie.Path != "/" {
		t.Fatalf("invalid session cookie attributes: %+v", sessionCookie)
	}
	var sessOut struct {
		UserID    string `json:"user_id"`
		SessionID string `json:"session_id"`
		VehicleID string `json:"vehicle_id"`
		Cursor    uint64 `json:"cursor"`
	}
	if err := json.NewDecoder(sessResp.Body).Decode(&sessOut); err != nil {
		t.Fatal(err)
	}
	if sessOut.UserID != "demo-user" || sessOut.SessionID != "web-s1" || sessOut.VehicleID != "sim-car" {
		t.Fatalf("unexpected /api/session response: %+v", sessOut)
	}

	// 4. GET /api/config via Cookie only (no Authorization header).
	cfgResp, err := browser.Get(pubSrv.URL + "/api/config")
	if err != nil {
		t.Fatal(err)
	}
	cfgRaw, _ := io.ReadAll(cfgResp.Body)
	cfgResp.Body.Close()
	if cfgResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/config status=%d body=%s", cfgResp.StatusCode, string(cfgRaw))
	}
	for _, secret := range []string{hermesKey, mcpToken, demoToken, sessionCookie.Value} {
		if strings.Contains(string(cfgRaw), secret) {
			t.Fatalf("secret leaked in /api/config: %s", string(cfgRaw))
		}
	}
	var cfgData struct {
		HermesReady bool     `json:"hermes_ready"`
		SimMode     string   `json:"sim_mode"`
		Suggestions []string `json:"suggestions"`
		DefaultVeh  struct {
			VehicleID string `json:"vehicle_id"`
		} `json:"default_vehicle"`
	}
	if err := json.Unmarshal(cfgRaw, &cfgData); err != nil || !cfgData.HermesReady || cfgData.SimMode != "accepted_only" || len(cfgData.Suggestions) == 0 || cfgData.DefaultVeh.VehicleID != "sim-car" {
		t.Fatalf("unexpected /api/config payload: %s", string(cfgRaw))
	}

	// 5. GET /api/capabilities via Cookie only.
	capResp, err := browser.Get(pubSrv.URL + "/api/capabilities")
	if err != nil {
		t.Fatal(err)
	}
	var capData struct {
		VehicleID    string `json:"vehicle_id"`
		Capabilities []struct {
			Device string `json:"device"`
			Tool   string `json:"tool"`
		} `json:"capabilities"`
	}
	if err := json.NewDecoder(capResp.Body).Decode(&capData); err != nil {
		capResp.Body.Close()
		t.Fatal(err)
	}
	capResp.Body.Close()
	if capResp.StatusCode != http.StatusOK || capData.VehicleID != "sim-car" || len(capData.Capabilities) != 7 {
		t.Fatalf("unexpected /api/capabilities: status=%d data=%+v", capResp.StatusCode, capData)
	}

	// 6. Open GET /api/events/stream via Cookie only.
	sseCtx, sseCancel := context.WithCancel(ctx)
	defer sseCancel()
	sseReq, _ := http.NewRequestWithContext(sseCtx, http.MethodGet, pubSrv.URL+"/api/events/stream", nil)
	sseResp, err := browser.Do(sseReq)
	if err != nil || sseResp.StatusCode != http.StatusOK {
		t.Fatalf("Cookie GET /api/events/stream failed: resp=%v err=%v", sseResp, err)
	}
	defer sseResp.Body.Close()

	// 7. Two-turn window close via Cookie-authenticated POST /voice/utterances.
	postCookieUtterance := func(reqID, text string) contract.SpeechReply {
		t.Helper()
		b, _ := json.Marshal(map[string]string{
			"session_id": "web-s1", "vehicle_id": "sim-car", "request_id": reqID, "text": text,
		})
		resp, err := browser.Post(pubSrv.URL+"/voice/utterances", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatalf("cookie utterance %s: %v", reqID, err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("cookie utterance %s HTTP %d: %s", reqID, resp.StatusCode, string(raw))
		}
		var currentEvent string
		for _, line := range strings.Split(string(raw), "\n") {
			if ev, ok := strings.CutPrefix(line, "event: "); ok {
				currentEvent = strings.TrimSpace(ev)
				continue
			}
			if data, ok := strings.CutPrefix(line, "data: "); ok && currentEvent == "speech.reply" {
				var reply contract.SpeechReply
				if err := json.Unmarshal([]byte(data), &reply); err != nil {
					t.Fatalf("decode reply: %v", err)
				}
				return reply
			}
		}
		t.Fatalf("missing speech.reply in response: %s", string(raw))
		return contract.SpeechReply{}
	}

	r1 := postCookieUtterance("cookie-r1", "把车窗关一下")
	if r1.Status != contract.PendingReply || r1.TaskID == "" {
		t.Fatalf("unexpected cookie Turn 1 reply: %+v", r1)
	}
	r2 := postCookieUtterance("cookie-r2", "左后窗")
	if r2.CommandID == "" || r2.TaskID != r1.TaskID {
		t.Fatalf("unexpected cookie Turn 2 reply: %+v", r2)
	}

	// Wait for executor to transition command to executing.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		cmd, err := app.taskStore.GetCommand(ctx, r2.CommandID)
		if err == nil && cmd.Status == contract.Executing {
			break
		}
		time.Sleep(40 * time.Millisecond)
	}

	// 8. GET /api/state via Cookie must show active_commands, messages, process_steps, cursor > 0.
	stateResp, err := browser.Get(pubSrv.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	var state1 struct {
		Cursor         uint64                `json:"cursor"`
		ActiveCommands []activeCommandView   `json:"active_commands"`
		RecentTasks    []contract.TaskResult `json:"recent_tasks"`
		Messages       []json.RawMessage     `json:"messages"`
		ProcessSteps   []json.RawMessage     `json:"process_steps"`
	}
	if err := json.NewDecoder(stateResp.Body).Decode(&state1); err != nil {
		stateResp.Body.Close()
		t.Fatal(err)
	}
	stateResp.Body.Close()
	if state1.Cursor == 0 || len(state1.ActiveCommands) != 1 || state1.ActiveCommands[0].CommandID != r2.CommandID || len(state1.Messages) < 4 || len(state1.ProcessSteps) < 4 {
		t.Fatalf("unexpected /api/state before receipt: cursor=%d active=%+v msgs=%d steps=%d",
			state1.Cursor, state1.ActiveCommands, len(state1.Messages), len(state1.ProcessSteps))
	}

	if len(state1.RecentTasks) != 1 || state1.RecentTasks[0].TaskID != r2.TaskID ||
		state1.RecentTasks[0].Status != contract.Executing || state1.RecentTasks[0].Operation == nil ||
		*state1.RecentTasks[0].Operation != (contract.Operation{Device: "window", Tool: "close"}) ||
		state1.RecentTasks[0].CreatedAt == nil {
		t.Fatalf("recent tasks should include the executing command: %+v", state1.RecentTasks)
	}

	// 9. POST /debug/receipt via Cookie completes the command and updates telemetry.
	recBody, _ := json.Marshal(map[string]string{"command_id": r2.CommandID, "status": "completed"})
	recResp, err := browser.Post(pubSrv.URL+"/debug/receipt", "application/json", bytes.NewReader(recBody))
	if err != nil {
		t.Fatal(err)
	}
	recResp.Body.Close()
	if recResp.StatusCode != http.StatusOK {
		t.Fatalf("Cookie POST /debug/receipt status=%d", recResp.StatusCode)
	}

	r3 := postCookieUtterance("cookie-r3", "把车窗关一下")
	if r3.Status != contract.PendingReply || r3.TaskID == r2.TaskID {
		t.Fatalf("expected a new task waiting for input: %+v", r3)
	}

	// 10. GET /api/state includes the new pending task and the previous completion.
	stateResp2, err := browser.Get(pubSrv.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	var state2 struct {
		Telemetry map[string]struct {
			Value any `json:"Value"`
		} `json:"telemetry"`
		ActiveCommands []activeCommandView           `json:"active_commands"`
		VehicleResults []contract.VehicleResultEvent `json:"vehicle_results"`
		RecentTasks    []contract.TaskResult         `json:"recent_tasks"`
	}
	if err := json.NewDecoder(stateResp2.Body).Decode(&state2); err != nil {
		stateResp2.Body.Close()
		t.Fatal(err)
	}
	stateResp2.Body.Close()
	if state2.Telemetry["window.rear_left"].Value != "closed" || len(state2.ActiveCommands) != 0 || len(state2.VehicleResults) != 1 || state2.VehicleResults[0].Status != contract.Completed {
		t.Fatalf("unexpected /api/state after receipt: %+v", state2)
	}
	if len(state2.RecentTasks) != 2 ||
		state2.RecentTasks[0].TaskID != r3.TaskID || state2.RecentTasks[0].Status != contract.PendingReply ||
		state2.RecentTasks[1].TaskID != r2.TaskID || state2.RecentTasks[1].Status != contract.Completed {
		t.Fatalf("expected latest pending followed by completed: %+v", state2.RecentTasks)
	}
	sseCancel()
}

func TestSelectDemoTTSPrefersQwenAndEscalatesRepeatHint(t *testing.T) {
	factory, err := qwen.New(qwen.Config{APIKey: "mock-key"})
	if err != nil {
		t.Fatalf("qwen.New: %v", err)
	}
	if factory.Voice() != qwen.DefaultVoice {
		t.Fatalf("expected default voice %q, got %q", qwen.DefaultVoice, factory.Voice())
	}

	synth, backend, err := selectDemoTTS("auto", factory)
	if err != nil || backend != "qwen" {
		t.Fatalf("expected auto mode with qwenFactory to select qwen, got backend=%q err=%v", backend, err)
	}
	qtts, ok := synth.(*qwenReadAloudTTS)
	if !ok {
		t.Fatalf("expected *qwenReadAloudTTS, got %T", synth)
	}

	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	qtts.now = func() time.Time { return now }

	// 1. Normal polite turn -> calm & warm, explicitly NOT impatient.
	ctxPolite := contract.WithUserUtterance(context.Background(), "帮我把车窗开一下")
	h1 := qtts.nextStyleHint(ctxPolite, "要打开哪扇车窗？")
	if !strings.Contains(h1, "普通正常对话") || !strings.Contains(h1, "不要表现不耐烦") {
		t.Fatalf("expected calm normal turn hint on first call, got %q", h1)
	}

	// 2. Repeated same question -> subtle impatience escalated.
	now = now.Add(15 * time.Second)
	h2 := qtts.nextStyleHint(ctxPolite, "要打开哪扇车窗？")
	if !strings.Contains(h2, "反复触发") || !strings.Contains(h2, "第 2 次") {
		t.Fatalf("expected escalated repeat hint on duplicate call, got %q", h2)
	}

	// 3. Pushy/impatient user tone -> triggers tone-aware subtle impatience.
	now = now.Add(15 * time.Second)
	ctxPushy := contract.WithUserUtterance(context.Background(), "快点把空调打开，怎么这么慢")
	h3 := qtts.nextStyleHint(ctxPushy, "空调要调到几度？")
	if !strings.Contains(h3, "催促或不太客气") {
		t.Fatalf("expected pushy-tone hint, got %q", h3)
	}

	// 4. 4th consecutive turn in short window -> mild back-and-forth fatigue cue.
	now = now.Add(15 * time.Second)
	h4 := qtts.nextStyleHint(ctxPolite, "车辆已确认执行完成。")
	if !strings.Contains(h4, "连续来回沟通了 4 轮") {
		t.Fatalf("expected multi-turn hint on 4th consecutive call, got %q", h4)
	}

	// Verify auto mode falls back locally when qwenFactory is nil.
	_, fallbackBackend, err := selectDemoTTS("auto", nil)
	if err != nil || (fallbackBackend != "say" && fallbackBackend != "tone") {
		t.Fatalf("expected local fallback when qwenFactory is nil, got %q err=%v", fallbackBackend, err)
	}
}
