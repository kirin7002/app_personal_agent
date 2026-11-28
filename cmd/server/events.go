package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kirin7002/app_personal_agent/contract"
	"github.com/kirin7002/app_personal_agent/vehicle/executor"
	"github.com/kirin7002/app_personal_agent/vehicle/mcp"
	"github.com/kirin7002/app_personal_agent/vehicle/turns"
	"github.com/kirin7002/app_personal_agent/voice/gateway"
)

const (
	defaultEventRingCapacity = 500
	defaultSSEPingInterval   = 15 * time.Second
	subscriberChannelSize    = 128
	maxSnapshotItems         = 200
)

// BusEvent is a single sequenced, sanitized SSE frame stored in a Scope's ring buffer.
type BusEvent struct {
	Seq       uint64
	Event     string
	TurnID    string
	RequestID string
	Data      json.RawMessage
}

type runBinding struct {
	Scope        contract.Scope
	TurnID       string
	RequestID    string
	Channel      string
	UpdatedAt    time.Time
	Closed       bool
	ClosedStatus string
	ClosedAt     time.Time
}

type openHermesStep struct {
	StepID    string
	Kind      string // "tool" or "subagent"
	Name      string // tool name or subagent name
	Title     string
	CallID    string
	Preview   string
	StartedAt time.Time
}

type eventSubscriber struct {
	ch       chan BusEvent
	lagged   bool
	closed   bool
	filterFn func(BusEvent) bool
}

type scopeBucket struct {
	nextSeq uint64
	ring    []BusEvent
	subs    map[*eventSubscriber]struct{}

	// Ordered deduplicated state views for GET /api/state snapshots.
	msgOrder  []string
	msgByID   map[string]json.RawMessage
	stepOrder []string
	stepByKey map[string]json.RawMessage
}

// EventBus maintains Scope-isolated bounded ring buffers, monotonic per-Scope
// sequence numbers, fan-out SSE subscriptions, and unified payload sanitization.
type EventBus struct {
	mu              sync.RWMutex
	capacity        int
	pingInterval    time.Duration
	buckets         map[contract.Scope]*scopeBucket
	runs            map[string]runBinding
	openHermesSteps map[string][]*openHermesStep
	secrets         []string
	hermesSeq       uint64
}

func NewEventBus(capacity int) *EventBus {
	if capacity <= 0 {
		capacity = defaultEventRingCapacity
	}
	return &EventBus{
		capacity:        capacity,
		pingInterval:    defaultSSEPingInterval,
		buckets:         make(map[contract.Scope]*scopeBucket),
		runs:            make(map[string]runBinding),
		openHermesSteps: make(map[string][]*openHermesStep),
	}
}

// AddSecrets registers runtime secrets that must never appear in any emitted event string.
func (b *EventBus) AddSecrets(values ...string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, raw := range values {
		v := strings.TrimSpace(raw)
		if len(v) >= 4 {
			b.secrets = append(b.secrets, v)
		}
	}
}

// BindRun associates a Hermes run_id with its originating Scope and turn identifiers
// so Hermes stream events can be routed cleanly into the right Scope bucket.
func (b *EventBus) BindRun(runID string, scope contract.Scope, turnID, requestID, channel string) {
	if runID == "" || !scope.Valid() {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now().UTC()
	if len(b.runs) > 256 {
		cutoff := now.Add(-15 * time.Minute)
		for k, v := range b.runs {
			if v.UpdatedAt.Before(cutoff) {
				delete(b.runs, k)
				delete(b.openHermesSteps, k)
			}
		}
	}
	prev := b.runs[runID]
	b.runs[runID] = runBinding{
		Scope:        scope,
		TurnID:       turnID,
		RequestID:    requestID,
		Channel:      channel,
		UpdatedAt:    now,
		Closed:       prev.Closed,
		ClosedStatus: prev.ClosedStatus,
		ClosedAt:     prev.ClosedAt,
	}
}

func (b *EventBus) lookupRun(runID string) (runBinding, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	rb, ok := b.runs[runID]
	return rb, ok
}

func (b *EventBus) getOrCreateBucketLocked(scope contract.Scope) *scopeBucket {
	bucket := b.buckets[scope]
	if bucket == nil {
		bucket = &scopeBucket{
			subs:      make(map[*eventSubscriber]struct{}),
			msgByID:   make(map[string]json.RawMessage),
			stepByKey: make(map[string]json.RawMessage),
		}
		b.buckets[scope] = bucket
	}
	return bucket
}

// Publish sanitizes payload, assigns the next monotonic per-Scope seq, stores it
// in the Scope's ring buffer, and fans it out to all active subscribers.
func (b *EventBus) Publish(scope contract.Scope, eventType string, payload map[string]any) BusEvent {
	if !scope.Valid() || eventType == "" {
		return BusEvent{}
	}

	b.mu.Lock()
	secrets := append([]string(nil), b.secrets...)
	bucket := b.getOrCreateBucketLocked(scope)
	bucket.nextSeq++
	seq := bucket.nextSeq

	sanitized := sanitizeMap(payload, secrets)
	sanitized["seq"] = seq
	if _, ok := sanitized["ts"]; !ok {
		sanitized["ts"] = time.Now().UTC().Format(time.RFC3339Nano)
	}
	sanitized["scope"] = scope

	raw, err := json.Marshal(sanitized)
	if err != nil {
		b.mu.Unlock()
		return BusEvent{}
	}

	turnID, _ := sanitized["turn_id"].(string)
	requestID, _ := sanitized["request_id"].(string)
	ev := BusEvent{
		Seq:       seq,
		Event:     eventType,
		TurnID:    turnID,
		RequestID: requestID,
		Data:      raw,
	}

	bucket.ring = append(bucket.ring, ev)
	if len(bucket.ring) > b.capacity {
		drop := len(bucket.ring) - b.capacity
		bucket.ring = append([]BusEvent(nil), bucket.ring[drop:]...)
	}

	// Maintain bounded deduplicated views for GET /api/state.
	switch eventType {
	case "message.upsert":
		if msgID, _ := sanitized["message_id"].(string); msgID != "" {
			if _, exists := bucket.msgByID[msgID]; !exists {
				bucket.msgOrder = append(bucket.msgOrder, msgID)
			}
			bucket.msgByID[msgID] = raw
			if len(bucket.msgOrder) > maxSnapshotItems {
				evict := bucket.msgOrder[0]
				bucket.msgOrder = bucket.msgOrder[1:]
				delete(bucket.msgByID, evict)
			}
		}
	case "process.step":
		if stepObj, ok := sanitized["step"].(map[string]any); ok {
			stepID, _ := stepObj["step_id"].(string)
			key := turnID + "|" + stepID
			if stepID != "" {
				if _, exists := bucket.stepByKey[key]; !exists {
					bucket.stepOrder = append(bucket.stepOrder, key)
				}
				bucket.stepByKey[key] = raw
				if len(bucket.stepOrder) > maxSnapshotItems {
					evict := bucket.stepOrder[0]
					bucket.stepOrder = bucket.stepOrder[1:]
					delete(bucket.stepByKey, evict)
				}
			}
		}
	}

	for sub := range bucket.subs {
		if sub.closed {
			continue
		}
		if sub.filterFn != nil && !sub.filterFn(ev) {
			continue
		}
		select {
		case sub.ch <- ev:
		default:
			// Slow consumer protection: mark lagged and close so SSE emits stream.reset.
			sub.lagged = true
			sub.closed = true
			close(sub.ch)
			delete(bucket.subs, sub)
		}
	}
	b.mu.Unlock()
	return ev
}

// PublishVehicleTelemetry broadcasts a telemetry.updated event to all scopes watching
// vehicleID (plus primaryScope if valid and not yet in buckets).
func (b *EventBus) PublishVehicleTelemetry(primaryScope contract.Scope, vehicleID string, telemetry map[string]any) {
	b.mu.RLock()
	targets := make(map[contract.Scope]struct{})
	if primaryScope.Valid() {
		targets[primaryScope] = struct{}{}
	}
	for sc := range b.buckets {
		if sc.VehicleID == vehicleID {
			targets[sc] = struct{}{}
		}
	}
	b.mu.RUnlock()

	for sc := range targets {
		b.Publish(sc, "telemetry.updated", map[string]any{
			"vehicle_id": vehicleID,
			"telemetry":  telemetry,
		})
	}
}

// Snapshot returns the latest cursor, deduplicated messages, and process steps for scope.
func (b *EventBus) Snapshot(scope contract.Scope) (cursor uint64, messages []json.RawMessage, steps []json.RawMessage) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	bucket := b.buckets[scope]
	if bucket == nil {
		return 0, []json.RawMessage{}, []json.RawMessage{}
	}
	msgs := make([]json.RawMessage, 0, len(bucket.msgOrder))
	for _, id := range bucket.msgOrder {
		if raw, ok := bucket.msgByID[id]; ok {
			msgs = append(msgs, raw)
		}
	}
	stps := make([]json.RawMessage, 0, len(bucket.stepOrder))
	for _, key := range bucket.stepOrder {
		if raw, ok := bucket.stepByKey[key]; ok {
			stps = append(stps, raw)
		}
	}
	return bucket.nextSeq, msgs, stps
}

// Cursor returns the current latest sequence number for scope.
func (b *EventBus) Cursor(scope contract.Scope) uint64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if bucket := b.buckets[scope]; bucket != nil {
		return bucket.nextSeq
	}
	return 0
}

// Subscribe opens a replay+live subscription for scope starting strictly after afterSeq.
// If afterSeq > 0 is outside the retained ring window (evicted or after a server restart),
// needsReset is true and currentCursor indicates the latest cursor to sync from GET /api/state.
func (b *EventBus) Subscribe(scope contract.Scope, afterSeq uint64) (replay []BusEvent, ch <-chan BusEvent, needsReset bool, currentCursor uint64, cancel func()) {
	b.mu.Lock()
	bucket := b.getOrCreateBucketLocked(scope)
	currentCursor = bucket.nextSeq

	if afterSeq > 0 {
		if afterSeq > bucket.nextSeq {
			needsReset = true
		} else if len(bucket.ring) > 0 && afterSeq < bucket.ring[0].Seq-1 {
			needsReset = true
		}
	}
	if !needsReset {
		for _, ev := range bucket.ring {
			if ev.Seq > afterSeq {
				replay = append(replay, ev)
			}
		}
	}

	sub := &eventSubscriber{ch: make(chan BusEvent, subscriberChannelSize)}
	bucket.subs[sub] = struct{}{}
	b.mu.Unlock()

	cancel = func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if !sub.closed {
			sub.closed = true
			close(sub.ch)
			delete(bucket.subs, sub)
		}
	}
	return replay, sub.ch, needsReset, currentCursor, cancel
}

// SubscribeRequestSteps subscribes to live process.step payloads matching (scope, turnID, requestID),
// designed for turns.UtteranceOptions.SubscribeSteps.
func (b *EventBus) SubscribeRequestSteps(_ context.Context, scope contract.Scope, turnID, requestID string) (<-chan json.RawMessage, func()) {
	b.mu.Lock()
	bucket := b.getOrCreateBucketLocked(scope)
	sub := &eventSubscriber{
		ch: make(chan BusEvent, subscriberChannelSize),
		filterFn: func(ev BusEvent) bool {
			if ev.Event != "process.step" {
				return false
			}
			return (requestID != "" && ev.RequestID == requestID) || (turnID != "" && ev.TurnID == turnID)
		},
	}
	bucket.subs[sub] = struct{}{}
	b.mu.Unlock()

	out := make(chan json.RawMessage, subscriberChannelSize)
	go func() {
		defer close(out)
		for ev := range sub.ch {
			out <- ev.Data
		}
	}()

	stop := func() {
		b.mu.Lock()
		if !sub.closed {
			sub.closed = true
			close(sub.ch)
			delete(bucket.subs, sub)
		}
		b.mu.Unlock()
	}
	return out, stop
}

// ServeSSE streams GET /api/events/stream for an authenticated Scope.
func (b *EventBus) ServeSSE(w http.ResponseWriter, r *http.Request, scope contract.Scope) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}

	// Last-Event-ID takes precedence over query parameter `after`.
	var afterSeq uint64
	if raw := strings.TrimSpace(r.Header.Get("Last-Event-ID")); raw != "" {
		if parsed, err := strconv.ParseUint(raw, 10, 64); err == nil {
			afterSeq = parsed
		}
	} else if raw := strings.TrimSpace(r.URL.Query().Get("after")); raw != "" {
		if parsed, err := strconv.ParseUint(raw, 10, 64); err == nil {
			afterSeq = parsed
		}
	}

	replay, ch, needsReset, cursor, cancel := b.Subscribe(scope, afterSeq)
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}

	if needsReset {
		resetPayload, _ := json.Marshal(map[string]any{
			"cursor": cursor,
			"reason": "cursor_out_of_window",
			"ts":     time.Now().UTC().Format(time.RFC3339Nano),
		})
		_, _ = fmt.Fprintf(w, "event: stream.reset\ndata: %s\n\n", resetPayload)
		flush()
		return
	}

	var lastSent uint64 = afterSeq
	for _, ev := range replay {
		if ev.Seq <= lastSent {
			continue
		}
		_, _ = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.Seq, ev.Event, ev.Data)
		lastSent = ev.Seq
	}
	flush()

	pingInterval := b.pingInterval
	if pingInterval <= 0 {
		pingInterval = defaultSSEPingInterval
	}
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				// Lagged subscriber was closed by publisher; ask client to resync.
				resetPayload, _ := json.Marshal(map[string]any{
					"cursor": b.Cursor(scope),
					"reason": "slow_consumer",
					"ts":     time.Now().UTC().Format(time.RFC3339Nano),
				})
				_, _ = fmt.Fprintf(w, "event: stream.reset\ndata: %s\n\n", resetPayload)
				flush()
				return
			}
			if ev.Seq <= lastSent {
				continue
			}
			_, _ = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.Seq, ev.Event, ev.Data)
			lastSent = ev.Seq
			flush()
		case <-ticker.C:
			pingPayload, _ := json.Marshal(map[string]any{
				"ts": time.Now().UTC().Format(time.RFC3339Nano),
			})
			_, _ = fmt.Fprintf(w, "event: ping\ndata: %s\n\n", pingPayload)
			flush()
		}
	}
}

// --- W01 Hook Adapters ---

func (b *EventBus) PublishUserMessage(scope contract.Scope, turnID, requestID, channel, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	b.Publish(scope, "message.upsert", map[string]any{
		"message_id": "user:" + turnID,
		"turn_id":    turnID,
		"request_id": requestID,
		"role":       "user",
		"channel":    channel,
		"text":       text,
	})
}

func (b *EventBus) HandleTurnEvent(ev turns.TurnEvent) {
	if !ev.Scope.Valid() {
		return
	}
	if ev.RunID != "" {
		b.BindRun(ev.RunID, ev.Scope, ev.TurnID, ev.RequestID, ev.Channel)
	}
	obs := map[string]any{
		"action":     ev.Stage,
		"error_code": ev.ErrorCode,
	}
	detail := ev.Detail
	switch ev.Stage {
	case "catalog.recall":
		obs["input"] = map[string]any{
			"pending_count":    ev.PendingCount,
			"pending_task_ids": ev.PendingTaskIDs,
		}
		obs["output"] = map[string]any{
			"candidate_tools": ev.CandidateTools,
		}
		if detail == "" {
			detail = fmt.Sprintf("召回 %d 个候选能力，加载 %d 条待办", len(ev.CandidateTools), ev.PendingCount)
		}
	case "hermes.run":
		obs["output"] = map[string]any{
			"run_id": ev.RunID,
			"status": ev.RunStatus,
		}
		if detail == "" && ev.RunID != "" {
			detail = "run_id=" + ev.RunID
		}
	case "turn.reply":
		obs["output"] = map[string]any{
			"status":     ev.ReplyStatus,
			"task_id":    ev.TaskID,
			"command_id": ev.CommandID,
			"text":       ev.ReplyText,
		}
		if detail == "" && ev.ReplyStatus != "" {
			detail = "状态：" + string(ev.ReplyStatus)
		}
	}
	if ev.ErrorMessage != "" {
		obs["error_message"] = ev.ErrorMessage
	}

	if ev.Stage == "hermes.run" && ev.RunID != "" && isTerminalStepStatus(ev.Status) {
		b.reconcileHermesRunSteps(ev.RunID, ev.Status, ev.RunStatus, ev.ErrorCode, ev.FinishedAt)
	}

	b.Publish(ev.Scope, "process.step", map[string]any{
		"turn_id":    ev.TurnID,
		"request_id": ev.RequestID,
		"run_id":     ev.RunID,
		"task_id":    ev.TaskID,
		"command_id": ev.CommandID,
		"channel":    ev.Channel,
		"step": map[string]any{
			"step_id":       ev.StepID,
			"stage":         ev.Stage,
			"title":         ev.Title,
			"status":        ev.Status,
			"started_at":    ev.StartedAt.UTC().Format(time.RFC3339Nano),
			"finished_at":   formatOptTime(ev.FinishedAt),
			"duration_ms":   ev.DurationMs,
			"duration_kind": ev.DurationKind,
			"detail":        detail,
			"observation":   obs,
		},
	})

	if ev.Stage == "turn.reply" && ev.Status == "done" && strings.TrimSpace(ev.ReplyText) != "" {
		b.Publish(ev.Scope, "message.upsert", map[string]any{
			"message_id": "assistant:" + ev.TurnID,
			"turn_id":    ev.TurnID,
			"request_id": ev.RequestID,
			"run_id":     ev.RunID,
			"task_id":    ev.TaskID,
			"command_id": ev.CommandID,
			"role":       "assistant",
			"channel":    ev.Channel,
			"status":     ev.ReplyStatus,
			"text":       ev.ReplyText,
		})
	}
}

func isTerminalStepStatus(status string) bool {
	switch status {
	case "done", "error", "interrupted":
		return true
	default:
		return false
	}
}

func (b *EventBus) reconcileHermesRunSteps(runID, terminalStatus, runStatus, errorCode string, finishedAt *time.Time) {
	endTime := time.Now().UTC()
	if finishedAt != nil && !finishedAt.IsZero() {
		endTime = finishedAt.UTC()
	}

	b.mu.Lock()
	rb, ok := b.runs[runID]
	if ok {
		rb.Closed = true
		rb.ClosedStatus = terminalStatus
		rb.ClosedAt = endTime
		rb.UpdatedAt = time.Now().UTC()
		b.runs[runID] = rb
	}
	orphans := b.openHermesSteps[runID]
	delete(b.openHermesSteps, runID)
	b.mu.Unlock()

	if !ok || len(orphans) == 0 {
		return
	}

	for _, orphan := range orphans {
		dur := endTime.Sub(orphan.StartedAt).Milliseconds()
		if dur < 0 {
			dur = 0
		}
		obs := map[string]any{
			"action":     orphan.Kind + ".reconciled",
			"error_code": errorCode,
			"output": map[string]any{
				"run_id":     runID,
				"run_status": runStatus,
				"reconciled": true,
			},
		}
		if orphan.Preview != "" {
			obs["input"] = map[string]any{"preview": orphan.Preview}
		}
		b.Publish(rb.Scope, "process.step", map[string]any{
			"turn_id":    rb.TurnID,
			"request_id": rb.RequestID,
			"run_id":     runID,
			"channel":    rb.Channel,
			"step": map[string]any{
				"step_id":       orphan.StepID,
				"stage":         "hermes.run",
				"title":         orphan.Title,
				"status":        terminalStatus,
				"started_at":    orphan.StartedAt.UTC().Format(time.RFC3339Nano),
				"finished_at":   endTime.Format(time.RFC3339Nano),
				"duration_ms":   dur,
				"duration_kind": "boundary_wait",
				"detail":        "run ended (" + terminalStatus + ")",
				"observation":   obs,
			},
		})
	}
}

func firstNonEmptyString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, _ := m[k].(string); strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func asFloat64(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

func classifyHermesStreamEvent(eventType string, decoded map[string]any) (kind, status string, terminal bool) {
	switch {
	case strings.HasPrefix(eventType, "tool."):
		kind = "tool"
	case strings.HasPrefix(eventType, "subagent."):
		kind = "subagent"
	default:
		return "", "", false
	}

	lower := strings.ToLower(eventType)
	switch {
	case strings.HasSuffix(lower, ".failed"),
		strings.HasSuffix(lower, ".error"),
		strings.HasSuffix(lower, ".cancelled"),
		strings.HasSuffix(lower, ".canceled"),
		strings.HasSuffix(lower, ".aborted"):
		return kind, "error", true
	case strings.HasSuffix(lower, ".completed"),
		strings.HasSuffix(lower, ".complete"),
		strings.HasSuffix(lower, ".done"):
		errFlag, _ := decoded["error"].(bool)
		isErrFlag, _ := decoded["is_error"].(bool)
		if errFlag || isErrFlag {
			return kind, "error", true
		}
		return kind, "done", true
	default:
		return kind, "running", false
	}
}

func (b *EventBus) popMatchingOpenHermesStepLocked(runID, kind, name, explicitCallID string) *openHermesStep {
	list := b.openHermesSteps[runID]
	if len(list) == 0 {
		return nil
	}
	idx := -1
	if explicitCallID != "" {
		for i, s := range list {
			if s.CallID == explicitCallID && s.Kind == kind {
				idx = i
				break
			}
		}
		if idx < 0 {
			for i, s := range list {
				if s.CallID == explicitCallID {
					idx = i
					break
				}
			}
		}
	}
	if idx < 0 && name != "" {
		for i, s := range list {
			if s.Kind == kind && s.Name == name && (explicitCallID == "" || s.CallID == "") {
				idx = i
				break
			}
		}
	}
	if idx < 0 && name == "" {
		for i, s := range list {
			if s.Kind == kind && (explicitCallID == "" || s.CallID == "") {
				idx = i
				break
			}
		}
	}
	if idx < 0 {
		return nil
	}
	matched := list[idx]
	list = append(list[:idx], list[idx+1:]...)
	if len(list) == 0 {
		delete(b.openHermesSteps, runID)
	} else {
		b.openHermesSteps[runID] = list
	}
	return matched
}

func (b *EventBus) HandleHermesStreamEvent(runID, eventType string, raw json.RawMessage) {
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		decoded = map[string]any{}
	}
	// Only forward tool / subagent progress events; run lifecycle is covered by turns.Service hermes.run.
	kind, status, isTerminal := classifyHermesStreamEvent(eventType, decoded)
	if kind == "" {
		return
	}

	name := firstNonEmptyString(decoded, "tool", "name", "subagent")
	var explicitCallID string
	if kind == "subagent" {
		explicitCallID = firstNonEmptyString(decoded, "call_id", "tool_call_id", "subagent_id", "delegate_id")
	} else {
		explicitCallID = firstNonEmptyString(decoded, "call_id", "tool_call_id")
	}
	preview := firstNonEmptyString(decoded, "preview")

	title := "Hermes 工具进度"
	if kind == "subagent" {
		title = "Hermes 子任务进度"
		if name != "" {
			title = "Hermes 子任务 " + name
		}
	} else if name != "" {
		title = "Hermes 调用 " + name
	}

	now := time.Now().UTC()
	var (
		rb           runBinding
		stepID       string
		startedAt    time.Time
		finishedAt   any
		durationMs   any
		inputPreview string
	)

	b.mu.Lock()
	var ok bool
	rb, ok = b.runs[runID]
	if !ok {
		b.mu.Unlock()
		return
	}

	if !isTerminal {
		if rb.Closed {
			// Run already finished; close immediately instead of opening a zombie running step.
			if explicitCallID != "" {
				stepID = "hermes:" + kind + ":" + explicitCallID
			} else {
				b.hermesSeq++
				stepID = "hermes:" + kind + ":" + strconv.FormatUint(b.hermesSeq, 10)
			}
			startedAt = now
			if !rb.ClosedAt.IsZero() && rb.ClosedAt.Before(now) {
				startedAt = rb.ClosedAt
			}
			status = rb.ClosedStatus
			if !isTerminalStepStatus(status) {
				status = "done"
			}
			finishedAt = now.Format(time.RFC3339Nano)
			dur := now.Sub(startedAt).Milliseconds()
			if dur < 0 {
				dur = 0
			}
			durationMs = dur
		} else {
			var existing *openHermesStep
			if explicitCallID != "" {
				for _, s := range b.openHermesSteps[runID] {
					if s.CallID == explicitCallID && s.Kind == kind {
						existing = s
						break
					}
				}
			}
			if existing != nil {
				stepID = existing.StepID
				startedAt = existing.StartedAt
				if name != "" {
					existing.Name = name
					existing.Title = title
				}
				if preview != "" && existing.Preview == "" {
					existing.Preview = preview
				}
			} else {
				callKey := explicitCallID
				if callKey == "" {
					b.hermesSeq++
					callKey = strconv.FormatUint(b.hermesSeq, 10)
				}
				stepID = "hermes:" + kind + ":" + callKey
				startedAt = now
				b.openHermesSteps[runID] = append(b.openHermesSteps[runID], &openHermesStep{
					StepID:    stepID,
					Kind:      kind,
					Name:      name,
					Title:     title,
					CallID:    explicitCallID,
					Preview:   preview,
					StartedAt: startedAt,
				})
			}
		}
	} else {
		matched := b.popMatchingOpenHermesStepLocked(runID, kind, name, explicitCallID)
		if matched != nil {
			stepID = matched.StepID
			startedAt = matched.StartedAt
			inputPreview = matched.Preview
			if name == "" && matched.Title != "" {
				title = matched.Title
			}
			dur := now.Sub(startedAt).Milliseconds()
			if dur < 0 {
				dur = 0
			}
			durationMs = dur
		} else {
			callKey := explicitCallID
			if callKey == "" {
				b.hermesSeq++
				callKey = strconv.FormatUint(b.hermesSeq, 10)
			}
			stepID = "hermes:" + kind + ":" + callKey
			var dur int64
			if sec, ok := asFloat64(decoded["duration"]); ok && sec > 0 {
				dur = int64(math.Round(sec * 1000))
			}
			startedAt = now.Add(-time.Duration(dur) * time.Millisecond)
			durationMs = dur
		}
		finishedAt = now.Format(time.RFC3339Nano)
	}
	b.mu.Unlock()

	obs := map[string]any{
		"action":     eventType,
		"output":     decoded,
		"error_code": "",
	}
	if isTerminal && inputPreview != "" {
		obs["input"] = map[string]any{"preview": inputPreview}
	}

	b.Publish(rb.Scope, "process.step", map[string]any{
		"turn_id":    rb.TurnID,
		"request_id": rb.RequestID,
		"run_id":     runID,
		"channel":    rb.Channel,
		"step": map[string]any{
			"step_id":       stepID,
			"stage":         "hermes.run",
			"title":         title,
			"status":        status,
			"started_at":    startedAt.UTC().Format(time.RFC3339Nano),
			"finished_at":   finishedAt,
			"duration_ms":   durationMs,
			"duration_kind": "boundary_wait",
			"detail":        eventType,
			"observation":   obs,
		},
	})
}

func (b *EventBus) HandleMCPToolEvent(ev mcp.ToolAuditEvent) {
	if !ev.Scope.Valid() {
		return
	}
	channel := "voice"
	if strings.HasPrefix(ev.TurnID, "text-") {
		channel = "text"
	}
	detail := ev.ToolName
	if dev, _ := ev.Arguments["device"].(string); dev != "" {
		if tool, _ := ev.Arguments["tool"].(string); tool != "" {
			detail = dev + "." + tool
		}
	}
	b.Publish(ev.Scope, "process.step", map[string]any{
		"turn_id":    ev.TurnID,
		"request_id": ev.RequestID,
		"task_id":    ev.TaskID,
		"command_id": ev.CommandID,
		"channel":    channel,
		"step": map[string]any{
			"step_id":       ev.CallID,
			"stage":         "mcp.tool",
			"title":         "调用 MCP 工具 " + ev.ToolName,
			"status":        ev.Status,
			"started_at":    ev.StartedAt.UTC().Format(time.RFC3339Nano),
			"finished_at":   formatOptTime(ev.FinishedAt),
			"duration_ms":   ev.DurationMs,
			"duration_kind": "measured_tool",
			"detail":        detail,
			"observation": map[string]any{
				"action":        ev.ToolName,
				"input":         ev.Arguments,
				"output":        ev.Output,
				"error_code":    string(ev.ErrorCode),
				"error_message": ev.ErrorMessage,
			},
		},
	})

	if ev.Status == "done" && ev.IsWrite && ev.TaskID != "" {
		taskPayload := map[string]any{
			"task_id":    ev.TaskID,
			"command_id": ev.CommandID,
			"request_id": ev.RequestID,
			"status":     ev.WriteStatus,
		}
		if dev, _ := ev.Arguments["device"].(string); dev != "" {
			taskPayload["device"] = dev
		}
		if tool, _ := ev.Arguments["tool"].(string); tool != "" {
			taskPayload["tool"] = tool
		}
		if tr, ok := ev.Output.(contract.TaskResult); ok {
			taskPayload["question"] = tr.Question
			taskPayload["missing_fields"] = tr.MissingFields
			if tr.ExpiresAt != nil {
				taskPayload["expires_at"] = tr.ExpiresAt.UTC().Format(time.RFC3339Nano)
			}
		}
		b.Publish(ev.Scope, "task.updated", taskPayload)
	}
}

func (b *EventBus) HandleExecutorStage(ev executor.StageEvent) {
	if !ev.Scope.Valid() {
		return
	}
	title := "车辆指令下发（Outbox Worker）"
	stepID := "executor:dispatch:" + ev.CommandID
	detail := fmt.Sprintf("%s.%s · %s", ev.Operation.Device, ev.Operation.Tool, ev.CommandID)
	obs := map[string]any{
		"action": ev.Operation.Device + "." + ev.Operation.Tool,
		"input":  ev.Params,
		"output": map[string]any{
			"command_id":    ev.CommandID,
			"downstream_id": ev.DownstreamID,
			"status":        ev.CommandStatus,
			"attempts":      ev.Attempts,
		},
		"error_code": "",
	}
	if ev.Stage == "executor.receipt" {
		title = "车辆执行回执确认"
		stepID = "executor:receipt:" + ev.CommandID
		detail = fmt.Sprintf("%s.%s · %s (%s)", ev.Operation.Device, ev.Operation.Tool, ev.CommandStatus, ev.ReconcileMethod)
		obs["output"] = map[string]any{
			"event_id":         ev.EventID,
			"command_id":       ev.CommandID,
			"downstream_id":    ev.DownstreamID,
			"status":           ev.CommandStatus,
			"reconcile_method": ev.ReconcileMethod,
			"reason":           ev.Reason,
		}
	}
	if ev.Reason != "" && ev.Status == "error" {
		obs["error_message"] = ev.Reason
	}
	finished := ev.FinishedAt.UTC()
	dur := ev.DurationMs
	b.Publish(ev.Scope, "process.step", map[string]any{
		"turn_id":    ev.RequestID,
		"request_id": ev.RequestID,
		"task_id":    ev.TaskID,
		"command_id": ev.CommandID,
		"channel":    "executor",
		"step": map[string]any{
			"step_id":       stepID,
			"stage":         ev.Stage,
			"title":         title,
			"status":        ev.Status,
			"started_at":    ev.StartedAt.UTC().Format(time.RFC3339Nano),
			"finished_at":   finished.Format(time.RFC3339Nano),
			"duration_ms":   dur,
			"duration_kind": "boundary_wait",
			"detail":        detail,
			"observation":   obs,
		},
	})

	if ev.TaskID != "" && ev.CommandStatus != "" {
		b.Publish(ev.Scope, "task.updated", map[string]any{
			"task_id":       ev.TaskID,
			"command_id":    ev.CommandID,
			"request_id":    ev.RequestID,
			"device":        ev.Operation.Device,
			"tool":          ev.Operation.Tool,
			"status":        ev.CommandStatus,
			"downstream_id": ev.DownstreamID,
			"reason":        ev.Reason,
		})
	}
}

func (b *EventBus) HandleVehicleResult(ev contract.VehicleResultEvent) {
	if !ev.Scope.Valid() {
		return
	}
	b.Publish(ev.Scope, "vehicle.result", map[string]any{
		"event_id":    ev.EventID,
		"request_id":  ev.RequestID,
		"task_id":     ev.TaskID,
		"command_id":  ev.CommandID,
		"device":      ev.Device,
		"tool":        ev.Tool,
		"status":      ev.Status,
		"occurred_at": ev.OccurredAt.UTC().Format(time.RFC3339Nano),
		"text":        executor.FeedbackText(ev),
	})
}

func (b *EventBus) HandleGatewayEvent(ev gateway.GatewayEvent) {
	if !ev.Scope.Valid() {
		return
	}
	var (
		stepID string
		title  string
		detail string
		obs    = map[string]any{"action": ev.Stage, "error_code": ""}
	)
	switch ev.Stage {
	case "voice.asr":
		stepID = "voice:asr:" + ev.TurnID
		title = "语音转写（Qwen Realtime）"
		detail = fmt.Sprintf("上行音频 %d bytes", ev.AudioBytes)
		obs["input"] = map[string]any{"audio_bytes": ev.AudioBytes}
		obs["output"] = map[string]any{"transcript": ev.Transcript}
	case "voice.route":
		stepID = "voice:route:" + ev.TurnID
		title = "语音分流判定"
		detail = fmt.Sprintf("路由：%s（待办 %d 条）", ev.Route, ev.PendingCount)
		obs["output"] = map[string]any{"route": ev.Route, "pending_count": ev.PendingCount}
	case "voice.playback":
		stepID = "voice:playback:" + ev.PlaybackID
		title = "语音播报输出（TTS / S2S）"
		detail = fmt.Sprintf("来源：%s", ev.PlaybackSource)
		obs["output"] = map[string]any{
			"playback_id":       ev.PlaybackID,
			"source":            ev.PlaybackSource,
			"reason":            ev.PlaybackReason,
			"audio_bytes":       ev.AudioBytes,
			"audio_duration_ms": ev.AudioDurationMs,
		}
	default:
		stepID = "voice:" + ev.Stage + ":" + ev.TurnID
		title = ev.Stage
	}
	if ev.ErrorMessage != "" {
		obs["error_message"] = ev.ErrorMessage
	}
	b.Publish(ev.Scope, "process.step", map[string]any{
		"turn_id":    ev.TurnID,
		"request_id": ev.RequestID,
		"channel":    "voice",
		"step": map[string]any{
			"step_id":       stepID,
			"stage":         ev.Stage,
			"title":         title,
			"status":        ev.Status,
			"started_at":    ev.StartedAt.UTC().Format(time.RFC3339Nano),
			"finished_at":   formatOptTime(ev.FinishedAt),
			"duration_ms":   ev.DurationMs,
			"duration_kind": ev.DurationKind,
			"detail":        detail,
			"observation":   obs,
		},
	})

	if ev.Stage == "voice.asr" && ev.Status == "done" && strings.TrimSpace(ev.Transcript) != "" {
		b.PublishUserMessage(ev.Scope, ev.TurnID, ev.RequestID, "voice", ev.Transcript)
	}
}

func formatOptTime(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// --- Unified Sanitizer ---

var (
	forbiddenKeySubstrings = []string{
		"turn_handle",
		"x-vehicle-turn-handle",
		"authorization",
		"dashscope_api_key",
		"hermes_api_key",
		"vehicle_mcp_service_token",
		"api_key",
		"apikey",
		"service_token",
		"auth_token",
		"access_token",
		"refresh_token",
		"token",
		"cookie",
		"vehicle_demo_session",
		"password",
		"secret",
		"dsn",
	}
	bearerPattern = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]{4,}`)
)

func isForbiddenKey(key string) bool {
	lower := strings.ToLower(strings.TrimSpace(key))
	if lower == "_meta" || lower == "hermes_session_id" || lower == "x-vehicle-hermes-session-id" {
		return true
	}
	for _, sub := range forbiddenKeySubstrings {
		if strings.Contains(lower, sub) {
			return true
		}
	}
	return false
}

func sanitizeMap(in map[string]any, secrets []string) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		if isForbiddenKey(k) {
			continue
		}
		out[k] = sanitizeValue(v, secrets)
	}
	return out
}

func sanitizeValue(v any, secrets []string) any {
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		return sanitizeString(x, secrets)
	case map[string]any:
		return sanitizeMap(x, secrets)
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = sanitizeValue(item, secrets)
		}
		return out
	case []string:
		out := make([]string, len(x))
		for i, item := range x {
			out[i] = sanitizeString(item, secrets)
		}
		return out
	case json.RawMessage:
		var parsed any
		if err := json.Unmarshal(x, &parsed); err == nil {
			return sanitizeValue(parsed, secrets)
		}
		return sanitizeString(string(x), secrets)
	case contract.Scope, contract.Operation, contract.Status, contract.ErrorCode,
		bool, int, int32, int64, uint, uint32, uint64, float32, float64, *int64:
		return x
	default:
		// Round-trip arbitrary structs through JSON so nested forbidden fields are also stripped.
		raw, err := json.Marshal(x)
		if err != nil {
			return sanitizeString(fmt.Sprint(x), secrets)
		}
		var parsed any
		if err := json.Unmarshal(raw, &parsed); err != nil {
			return sanitizeString(string(raw), secrets)
		}
		return sanitizeValue(parsed, secrets)
	}
}

func sanitizeString(s string, secrets []string) string {
	if s == "" {
		return ""
	}
	out := bearerPattern.ReplaceAllString(s, "Bearer [REDACTED]")
	for _, sec := range secrets {
		if sec != "" && strings.Contains(out, sec) {
			out = strings.ReplaceAll(out, sec, "[REDACTED]")
		}
	}
	return out
}
