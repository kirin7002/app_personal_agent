package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kirin7002/app_personal_agent/contract"
	"github.com/kirin7002/app_personal_agent/vehicle/catalog"
	"github.com/kirin7002/app_personal_agent/vehicle/mcp"
	"github.com/kirin7002/app_personal_agent/vehicle/runs"
	"github.com/kirin7002/app_personal_agent/vehicle/tasks"
	"github.com/kirin7002/app_personal_agent/vehicle/turns"
)

func seedCommand(t *testing.T, store *tasks.MemoryStore, scope contract.Scope, reqID string, op contract.Operation, params map[string]any, now time.Time) contract.TaskResult {
	t.Helper()
	res, err := store.DispatchCommand(context.Background(), tasks.TaskWriteRequest{
		Turn: contract.TurnContext{
			VehicleTurn:     contract.VehicleTurn{Scope: scope, VoiceTurnID: "VT-" + reqID, RequestID: reqID},
			HermesSessionID: "HS-" + scope.SessionID,
			TurnHandle:      "TH-" + reqID,
			ExpiresAt:       now.Add(5 * time.Minute),
		},
		Mutation: contract.TaskMutation{Operation: op, Params: params},
		Now:      now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestAcceptedDuplicateAndOutOfOrderReceipts(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	tel := catalog.NewMemoryTelemetry()
	cat := catalog.NewService(tel)
	store := tasks.NewMemoryStore(tasks.NewCatalogValidator(cat))
	sim := NewSimulatedChannel(tel)
	sim.Mode = SimAcceptedOnly
	sim.Caps.SupportsStatusQuery = false
	sim.Caps.SupportsTelemetryConfirm = false

	var events []contract.VehicleResultEvent
	w := NewWorker("W1", store, cat, sim)
	w.Now = func() time.Time { return now }
	w.OnEvent = func(ev contract.VehicleResultEvent) { events = append(events, ev) }

	scope := contract.Scope{UserID: "U1", SessionID: "S1", VehicleID: "VCL1"}
	res := seedCommand(t, store, scope, "R1", contract.Operation{Device: "window", Tool: "close"}, map[string]any{"position": "rear_left"}, now)

	// 1. Worker sends -> accepted only -> task is executing, ZERO result events yet.
	if processed, err := w.ProcessNext(ctx); err != nil || !processed {
		t.Fatalf("ProcessNext failed: processed=%v err=%v", processed, err)
	}
	task, _ := store.GetTask(ctx, scope, res.TaskID)
	if task.Status != contract.Executing || len(events) != 0 {
		t.Fatalf("accepted-only must stay executing with 0 events, got status=%s events=%d", task.Status, len(events))
	}

	// 2. Duplicate completed receipts + out-of-order late accepted receipt -> exactly 1 completed event.
	now = now.Add(3 * time.Second)
	for range 3 {
		if err := w.HandleReceipt(ctx, VehicleReceipt{CommandID: res.CommandID, Status: ReceiptCompleted, OccurredAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.HandleReceipt(ctx, VehicleReceipt{CommandID: res.CommandID, Status: ReceiptAccepted, OccurredAt: now.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}

	task, _ = store.GetTask(ctx, scope, res.TaskID)
	if task.Status != contract.Completed {
		t.Fatalf("expected completed, got %s", task.Status)
	}
	if len(events) != 1 || events[0].EventID != res.CommandID+":completed" {
		t.Fatalf("expected exactly 1 deduplicated completion event, got %+v", events)
	}
	retained, err := store.ListVehicleResults(ctx, scope, now.Add(-time.Hour))
	if err != nil || len(retained) != 1 || retained[0].EventID != res.CommandID+":completed" {
		t.Fatalf("expected 1 retained outbox result event, got %+v, %v", retained, err)
	}
}

func TestCrashBeforeSendAndCrashAfterSendRecovery(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	tel := catalog.NewMemoryTelemetry()
	cat := catalog.NewService(tel)
	store := tasks.NewMemoryStore(tasks.NewCatalogValidator(cat))
	sim := NewSimulatedChannel(tel)
	sim.Now = func() time.Time { return now }
	scope := contract.Scope{UserID: "U1", SessionID: "S1", VehicleID: "VCL1"}

	// --- Scenario 1: Crash BEFORE send ---
	res1 := seedCommand(t, store, scope, "R_BEFORE", contract.Operation{Device: "window", Tool: "close"}, map[string]any{"position": "front_left"}, now)
	w1 := NewWorker("W1", store, cat, sim)
	w1.Now = func() time.Time { return now }
	w1.CrashBeforeSendOnce = true
	if processed, err := w1.ProcessNext(ctx); err != nil || !processed {
		t.Fatalf("w1 claim failed: %v", err)
	}
	if sim.SendCount(res1.CommandID) != 0 {
		t.Fatalf("crash-before-send must not call vehicle channel, got %d", sim.SendCount(res1.CommandID))
	}

	// Lease expires -> W2 claims and sends cleanly once with the same command_id.
	now = now.Add(20 * time.Second)
	w2 := NewWorker("W2", store, cat, sim)
	w2.Now = func() time.Time { return now }
	if processed, err := w2.ProcessNext(ctx); err != nil || !processed {
		t.Fatalf("w2 recovery failed: %v", err)
	}
	if sim.SendCount(res1.CommandID) != 1 {
		t.Fatalf("expected exactly 1 vehicle send after crash-before-send recovery, got %d", sim.SendCount(res1.CommandID))
	}
	t1, _ := store.GetTask(ctx, scope, res1.TaskID)
	if t1.Status != contract.Completed {
		t.Fatalf("expected completed, got %s", t1.Status)
	}

	// --- Scenario 2: Crash after transport, before its response is persisted. ---
	sim.Caps.SupportsIdempotency = false
	res2 := seedCommand(t, store, scope, "R_AFTER", contract.Operation{Device: "window", Tool: "close"}, map[string]any{"position": "rear_left"}, now)
	w1.CrashAfterSendOnce = true
	if processed, err := w1.ProcessNext(ctx); err != nil || !processed {
		t.Fatalf("w1 send+crash failed: %v", err)
	}
	if sim.SendCount(res2.CommandID) != 1 {
		t.Fatalf("expected 1 send before crash, got %d", sim.SendCount(res2.CommandID))
	}

	inFlight, err := store.GetCommand(ctx, res2.CommandID)
	if err != nil || inFlight.Status != contract.Executing || inFlight.DispatchedAt == nil || inFlight.DownstreamID != "" {
		t.Fatalf("send intent must survive without the downstream response: %+v, %v", inFlight, err)
	}

	// Lease expires -> W2 recovers executing command via reconciliation WITHOUT resending!
	now = now.Add(20 * time.Second)
	if processed, err := w2.ProcessNext(ctx); err != nil || !processed {
		t.Fatalf("w2 reconcile failed: %v", err)
	}
	if sim.SendCount(res2.CommandID) != 1 {
		t.Fatalf("crash-after-send recovery MUST NOT resend command, got send count=%d", sim.SendCount(res2.CommandID))
	}
	t2, _ := store.GetTask(ctx, scope, res2.TaskID)
	if t2.Status != contract.Completed {
		t.Fatalf("expected completed via reconciliation, got %s", t2.Status)
	}
}

func TestTelemetryConfirmationHVACStaleTimeoutAndLateReceipt(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	tel := catalog.NewMemoryTelemetry()
	cat := catalog.NewService(tel)
	store := tasks.NewMemoryStore(tasks.NewCatalogValidator(cat))
	sim := NewSimulatedChannel(tel)
	sim.Mode = SimAcceptedOnly
	sim.Caps.SupportsStatusQuery = false
	sim.Caps.SupportsTelemetryConfirm = true
	sim.UpdateTelemetryOnSend = false

	var events []contract.VehicleResultEvent
	w := NewWorker("W1", store, cat, sim)
	w.Now = func() time.Time { return now }
	w.ConfirmTimeout = 30 * time.Second
	w.ReconcileWindow = 2 * time.Minute
	w.OnEvent = func(ev contract.VehicleResultEvent) { events = append(events, ev) }

	scope := contract.Scope{UserID: "U1", SessionID: "S1", VehicleID: "VCL1"}
	// 1. HVAC confirmation checks BOTH power=on and target temperature_c=25 using post-dispatch fresh samples.
	hvacRes := seedCommand(t, store, scope, "R_HVAC", contract.Operation{Device: "hvac", Tool: "set_temperature"}, map[string]any{"temperature_c": 25}, now)
	_, _ = w.ProcessNext(ctx)

	// Stale pre-dispatch sample does NOT confirm.
	tel.Set("VCL1", "hvac.power", "on", now.Add(-5*time.Second))
	tel.Set("VCL1", "hvac.temperature_c", 25.0, now.Add(-5*time.Second))
	_ = w.ReconcileCommand(ctx, hvacRes.CommandID)
	if tRec, _ := store.GetTask(ctx, scope, hvacRes.TaskID); tRec.Status != contract.Executing {
		t.Fatalf("pre-dispatch telemetry must not confirm HVAC, got %s", tRec.Status)
	}

	// Post-dispatch temp=25 but power=off does NOT confirm!
	now = now.Add(5 * time.Second)
	tel.Set("VCL1", "hvac.power", "off", now)
	tel.Set("VCL1", "hvac.temperature_c", 25.0, now)
	_ = w.ReconcileCommand(ctx, hvacRes.CommandID)
	if tRec, _ := store.GetTask(ctx, scope, hvacRes.TaskID); tRec.Status != contract.Executing {
		t.Fatalf("HVAC power=off must not confirm, got %s", tRec.Status)
	}

	// Post-dispatch power=on AND temp=25 confirms!
	now = now.Add(2 * time.Second)
	tel.Set("VCL1", "hvac.power", "on", now)
	tel.Set("VCL1", "hvac.temperature_c", 25.0, now)
	_ = w.ReconcileCommand(ctx, hvacRes.CommandID)
	if tRec, _ := store.GetTask(ctx, scope, hvacRes.TaskID); tRec.Status != contract.Completed {
		t.Fatalf("expected HVAC completed when power=on & temp=25 fresh, got %s", tRec.Status)
	}

	// 2. Confirm timeout -> result_unknown, then late success WITHIN reconcile window promotes to completed.
	winRes1 := seedCommand(t, store, scope, "R_TO1", contract.Operation{Device: "window", Tool: "close"}, map[string]any{"position": "rear_right"}, now)
	_, _ = w.ProcessNext(ctx)
	now = now.Add(35 * time.Second) // exceeds 30s ConfirmTimeout
	_ = w.ReconcileCommand(ctx, winRes1.CommandID)
	if tRec, _ := store.GetTask(ctx, scope, winRes1.TaskID); tRec.Status != contract.ResultUnknown {
		t.Fatalf("expected result_unknown after confirm timeout, got %s", tRec.Status)
	}
	now = now.Add(30 * time.Second) // still within 2m ReconcileWindow
	if err := w.HandleReceipt(ctx, VehicleReceipt{CommandID: winRes1.CommandID, Status: ReceiptCompleted, OccurredAt: now}); err != nil {
		t.Fatal(err)
	}
	if tRec, _ := store.GetTask(ctx, scope, winRes1.TaskID); tRec.Status != contract.Completed {
		t.Fatalf("late success within reconcile window must promote to completed, got %s", tRec.Status)
	}

	// 3. Late success AFTER reconcile window -> stays result_unknown and logs LateEvidence.
	winRes2 := seedCommand(t, store, scope, "R_TO2", contract.Operation{Device: "window", Tool: "open"}, map[string]any{"position": "rear_right"}, now)
	_, _ = w.ProcessNext(ctx)
	now = now.Add(35 * time.Second)
	_ = w.ReconcileCommand(ctx, winRes2.CommandID)
	now = now.Add(3 * time.Minute) // past 2m ReconcileWindow
	if err := w.HandleReceipt(ctx, VehicleReceipt{CommandID: winRes2.CommandID, Status: ReceiptCompleted, OccurredAt: now}); err != nil {
		t.Fatal(err)
	}
	if tRec, _ := store.GetTask(ctx, scope, winRes2.TaskID); tRec.Status != contract.ResultUnknown {
		t.Fatalf("receipt after reconcile window must not mutate closed result_unknown task, got %s", tRec.Status)
	}
	if len(w.LateEvidences()) != 1 || w.LateEvidences()[0].CommandID != winRes2.CommandID {
		t.Fatalf("expected 1 LateEvidence recorded, got %+v", w.LateEvidences())
	}
}

func TestUncertainDeliveryWithoutIdempotencyReconcilesInsteadOfResending(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	tel := catalog.NewMemoryTelemetry()
	cat := catalog.NewService(tel)
	store := tasks.NewMemoryStore(tasks.NewCatalogValidator(cat))
	sim := NewSimulatedChannel(tel)
	sim.Now = func() time.Time { return now }
	sim.Caps.SupportsIdempotency = false
	sim.Caps.SupportsStatusQuery = true
	sim.Mode = SimUncertainExecuted

	w := NewWorker("W1", store, cat, sim)
	w.Now = func() time.Time { return now }
	scope := contract.Scope{UserID: "U1", SessionID: "S1", VehicleID: "VCL1"}
	res := seedCommand(t, store, scope, "R_UNC", contract.Operation{Device: "window", Tool: "close"}, map[string]any{"position": "front_right"}, now)

	if processed, err := w.ProcessNext(ctx); err != nil || !processed {
		t.Fatalf("ProcessNext failed: %v", err)
	}
	if sim.SendCount(res.CommandID) != 1 {
		t.Fatalf("uncertain delivery without idempotency must NEVER resend, got send count=%d", sim.SendCount(res.CommandID))
	}
	task, _ := store.GetTask(ctx, scope, res.TaskID)
	if task.Status != contract.Completed {
		t.Fatalf("reconciled uncertain delivery should reach completed via query/telemetry, got %s", task.Status)
	}
}

// --- End-to-End Text Entry (POST /voice/utterances) + H02 + H03 + H04 + H05 + H06 Verification ---

type staticAuth struct{ scope contract.Scope }

func (a staticAuth) Authenticate(_ context.Context, bearer, sessionID, vehicleID string) (contract.Scope, error) {
	if bearer != "car-token" || sessionID != a.scope.SessionID {
		return contract.Scope{}, &contract.BusinessError{Code: contract.TurnContextInvalid, Message: "unauthorized"}
	}
	if vehicleID != "" && vehicleID != a.scope.VehicleID {
		return contract.Scope{}, &contract.BusinessError{Code: contract.TurnContextInvalid, Message: "wrong vehicle"}
	}
	return a.scope, nil
}

// simulatedHermesRunner mimics Hermes calling Go's H04 MCP HTTP endpoint with the exact
// trusted headers (X-Vehicle-Turn-Handle, X-Vehicle-Hermes-Session-ID) and params._meta.
type simulatedHermesRunner struct {
	mcpURL   string
	mcpToken string
	client   *http.Client
	outputs  map[string]runs.Run
	seq      int
}

func (r *simulatedHermesRunner) Start(ctx context.Context, req runs.StartRequest, meta contract.TurnMetadata, _ string) (runs.Run, error) {
	r.seq++
	runID := "sim-run-" + string(rune('0'+r.seq))

	// Parse pending_tasks from H02's instructions JSON line.
	var parsed struct {
		Pending []contract.PendingCandidate `json:"pending_tasks"`
	}
	if idx := strings.Index(req.Instructions, "\n{"); idx >= 0 {
		_ = json.Unmarshal([]byte(req.Instructions[idx+1:]), &parsed)
	}

	callMCP := func(toolName string, args map[string]any) (bool, map[string]any) {
		rpcReq := map[string]any{
			"jsonrpc": "2.0",
			"id":      r.seq,
			"method":  "tools/call",
			"params": map[string]any{
				"name":      toolName,
				"arguments": args,
				"_meta": map[string]any{
					mcp.MetaTurnHandleKey:    meta.TurnHandle,
					mcp.MetaHermesSessionKey: meta.HermesSessionID,
				},
			},
		}
		b, _ := json.Marshal(rpcReq)
		httpReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, r.mcpURL, bytes.NewReader(b))
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+r.mcpToken)
		httpReq.Header.Set(contract.TurnHandleHeader, meta.TurnHandle)
		httpReq.Header.Set(contract.HermesSessionHeader, meta.HermesSessionID)
		resp, err := r.client.Do(httpReq)
		if err != nil {
			return true, map[string]any{"message": err.Error()}
		}
		defer resp.Body.Close()
		var out struct {
			Result struct {
				IsError           bool           `json:"isError"`
				StructuredContent map[string]any `json:"structuredContent"`
			} `json:"result"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return out.Result.IsError, out.Result.StructuredContent
	}

	outText := "好的。"
	switch req.Input {
	case "把车窗关一下":
		isErr, res := callMCP(contract.CreateVehicleTask, map[string]any{
			"device": "window", "tool": "close", "params": map[string]any{},
		})
		if !isErr {
			outText, _ = res["question"].(string)
		}
	case "打开空调":
		isErr, res := callMCP(contract.CreateVehicleTask, map[string]any{
			"device": "hvac", "tool": "set_climate", "params": map[string]any{"power": "on"},
		})
		if !isErr {
			outText, _ = res["question"].(string)
		}
	case "左后窗":
		for _, p := range parsed.Pending {
			if p.Device == "window" && p.Tool == "close" {
				isErr, res := callMCP(contract.DispatchVehicleCommand, map[string]any{
					"task_id": p.TaskID,
					"device":  "window",
					"tool":    "close",
					"params":  map[string]any{"position": "rear_left"},
				})
				if isErr {
					outText, _ = res["message"].(string)
				} else {
					outText = "正在关闭左后窗。"
				}
				break
			}
		}
	}

	if r.outputs == nil {
		r.outputs = map[string]runs.Run{}
	}
	r.outputs[runID] = runs.Run{ID: runID, Status: "completed", Output: outText}
	return runs.Run{ID: runID, Status: "started"}, nil
}

func (r *simulatedHermesRunner) Wait(_ context.Context, runID string) (runs.Run, error) {
	return r.outputs[runID], nil
}

func (r *simulatedHermesRunner) Stop(_ context.Context, _ string) error { return nil }

func postUtterance(t *testing.T, handler http.Handler, sessionID, vehicleID, reqID, text string) contract.SpeechReply {
	t.Helper()
	b, _ := json.Marshal(map[string]any{
		"session_id": sessionID, "vehicle_id": vehicleID, "request_id": reqID, "text": text,
	})
	req := httptest.NewRequest(http.MethodPost, "/voice/utterances", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer car-token")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /voice/utterances status=%d body=%s", rr.Code, rr.Body.String())
	}
	for _, line := range strings.Split(rr.Body.String(), "\n") {
		if strings.HasPrefix(line, "data: ") {
			var reply contract.SpeechReply
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &reply); err != nil {
				t.Fatalf("decode speech.reply: %v line=%s", err, line)
			}
			return reply
		}
	}
	t.Fatalf("no speech.reply event in SSE response: %s", rr.Body.String())
	return contract.SpeechReply{}
}

func TestEndToEndTextEntrySingleFollowUpDualMatchTenMinExpiryAndDuplicateReceipts(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	scope := contract.Scope{UserID: "U1", SessionID: "S1", VehicleID: "VCL1"}

	tel := catalog.NewMemoryTelemetry()
	cat := catalog.NewService(tel)
	turnStore := turns.NewMemoryStore()
	taskStore := tasks.NewMemoryStore(tasks.NewCatalogValidator(cat))
	taskStore.Now = func() time.Time { return now }

	mcpServer := mcp.NewServer(mcp.Config{
		ServiceToken: "mcp-svc-token",
		RequireMeta:  true,
		Turns:        turnStore,
		Tasks:        taskStore,
		Catalog:      cat,
		Now:          func() time.Time { return now },
	})
	mcpHTTP := httptest.NewServer(mcpServer)
	defer mcpHTTP.Close()

	runner := &simulatedHermesRunner{
		mcpURL:   mcpHTTP.URL,
		mcpToken: "mcp-svc-token",
		client:   mcpHTTP.Client(),
	}
	turnSvc := &turns.Service{
		Store:        turnStore,
		Pending:      taskStore,
		Capabilities: cat,
		Progress:     taskStore,
		Runs:         runner,
		Now:          func() time.Time { return now },
	}
	utteranceHandler := turns.UtteranceHandler(turnSvc, staticAuth{scope: scope})

	sim := NewSimulatedChannel(tel)
	sim.Now = func() time.Time { return now }
	sim.Mode = SimAcceptedOnly
	sim.Caps.SupportsStatusQuery = false
	var emittedEvents []contract.VehicleResultEvent
	worker := NewWorker("W_E2E", taskStore, cat, sim)
	worker.Now = func() time.Time { return now }
	worker.OnEvent = func(ev contract.VehicleResultEvent) { emittedEvents = append(emittedEvents, ev) }

	// 1. Single-task follow-up: "把车窗关一下" -> T101 pending_reply -> "左后窗" -> T101 pending_dispatch -> simulated vehicle -> duplicate receipts -> 1 completed event.
	r1 := postUtterance(t, utteranceHandler, "S1", "VCL1", "R1", "把车窗关一下")
	if r1.Status != contract.PendingReply || r1.TaskID == "" || !strings.Contains(r1.Text, "车窗") {
		t.Fatalf("expected follow-up question for R1, got %+v", r1)
	}
	now = now.Add(30 * time.Second)
	r2 := postUtterance(t, utteranceHandler, "S1", "VCL1", "R2", "左后窗")
	if r2.TaskID != r1.TaskID || r2.Status != contract.PendingDispatch || r2.CommandID == "" {
		t.Fatalf("expected resumed dispatch for same task, got r1=%+v r2=%+v", r1, r2)
	}
	// Dispatch to simulated vehicle + deliver duplicate completed receipts.
	if processed, err := worker.ProcessNext(ctx); err != nil || !processed {
		t.Fatalf("worker ProcessNext failed: %v", err)
	}
	_ = worker.HandleReceipt(ctx, VehicleReceipt{CommandID: r2.CommandID, Status: ReceiptCompleted, OccurredAt: now.Add(2 * time.Second)})
	_ = worker.HandleReceipt(ctx, VehicleReceipt{CommandID: r2.CommandID, Status: ReceiptCompleted, OccurredAt: now.Add(3 * time.Second)})
	if len(emittedEvents) != 1 || emittedEvents[0].Status != contract.Completed || FeedbackText(emittedEvents[0]) != "车窗已关闭。" {
		t.Fatalf("expected exactly 1 completed event with Chinese feedback, got %+v", emittedEvents)
	}

	// 2. Dual-task matching: T_win created first, then T_hvac newer -> "左后窗" resumes ONLY T_win, T_hvac remains pending_reply.
	now = now.Add(time.Minute)
	rWin := postUtterance(t, utteranceHandler, "S1", "VCL1", "R3", "把车窗关一下")
	now = now.Add(time.Minute)
	rHVAC := postUtterance(t, utteranceHandler, "S1", "VCL1", "R4", "打开空调")
	now = now.Add(10 * time.Second)
	rMatch := postUtterance(t, utteranceHandler, "S1", "VCL1", "R5", "左后窗")
	if rMatch.TaskID != rWin.TaskID || rMatch.Status != contract.PendingDispatch {
		t.Fatalf("expected dual-task match to resume window task %s, got %+v", rWin.TaskID, rMatch)
	}
	hvacTask, _ := taskStore.GetTask(ctx, scope, rHVAC.TaskID)
	if hvacTask.Status != contract.PendingReply {
		t.Fatalf("expected HVAC task %s to remain pending_reply, got %s", rHVAC.TaskID, hvacTask.Status)
	}

	// 3. 10-minute expiry: advance clock to exact 10m after rHVAC creation -> excluded & expired!
	now = now.Add(9*time.Minute + 50*time.Second) // exact 10m since R4
	pendingAfter10m, err := taskStore.ListPending(ctx, scope, now)
	if err != nil || len(pendingAfter10m) != 0 {
		t.Fatalf("expected 0 pending tasks at exact 10m boundary, got %+v, %v", pendingAfter10m, err)
	}
	expiredCount, err := taskStore.ExpirePending(ctx, now)
	if err != nil || expiredCount != 1 {
		t.Fatalf("expected expiry scanner to expire 1 HVAC task, got %d, %v", expiredCount, err)
	}
}

func TestRetryableSendRetainsStableCommand(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	cat := catalog.NewService(nil)
	store := tasks.NewMemoryStore(tasks.NewCatalogValidator(cat))
	channel := NewSimulatedChannel(nil)
	channel.Mode = SimRetryableOnce
	channel.Now = func() time.Time { return now }
	worker := NewWorker("retry-worker", store, cat, channel)
	worker.Now = func() time.Time { return now }
	scope := contract.Scope{UserID: "retry-user", SessionID: "retry-session", VehicleID: "sim-car"}
	first := seedCommand(t, store, scope, "retry-request", contract.Operation{Device: "window", Tool: "close"}, map[string]any{"position": "rear_left"}, now)
	if processed, err := worker.ProcessNext(ctx); !processed || err != nil {
		t.Fatalf("first attempt: processed=%v err=%v", processed, err)
	}
	cmd, err := store.GetCommand(ctx, first.CommandID)
	if err != nil || cmd.Status != contract.PendingDispatch || cmd.DispatchedAt != nil {
		t.Fatalf("retry was not requeued: %+v err=%v", cmd, err)
	}
	task, err := store.GetTask(ctx, scope, first.TaskID)
	if err != nil || task.Status != contract.Executing {
		t.Fatalf("in-progress task regressed after retryable send: %+v err=%v", task, err)
	}
	now = now.Add(worker.RetryBackoff)
	if processed, err := worker.ProcessNext(ctx); !processed || err != nil {
		t.Fatalf("second attempt: processed=%v err=%v", processed, err)
	}
	events, err := store.ListVehicleResults(ctx, scope, now.Add(-time.Minute))
	if err != nil || len(events) != 1 || events[0].CommandID != first.CommandID || events[0].Status != contract.Completed ||
		channel.SendCount(first.CommandID) != 2 {
		t.Fatalf("retry must reuse its ID and publish one completion: %+v, %v", events, err)
	}
}
