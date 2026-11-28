package turns

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kirin7002/app_personal_agent/contract"
)

// StepSubscription supplies pre-serialized JSON payloads for process.step events
// belonging to a single HTTP utterance turn (allowing cross-module steps such as
// Go MCP tool calls to stream on POST /voice/utterances in real time).
type StepSubscription func(ctx context.Context, scope contract.Scope, turnID, requestID string) (<-chan json.RawMessage, func())

// UtteranceOptions configures optional observability hooks for POST /voice/utterances.
type UtteranceOptions struct {
	// OnUtteranceAccepted is invoked right after authentication and validation,
	// before HandleVehicleTurn begins (e.g., to publish the user's message.upsert).
	OnUtteranceAccepted func(scope contract.Scope, turnID, requestID, text string)
	// SubscribeSteps optionally provides a stream of process.step JSON payloads
	// for this turn. When nil, UtteranceHandler streams TurnEvent steps directly
	// via WithTurnEventSink.
	SubscribeSteps StepSubscription
}

// UtteranceHandler serves POST /voice/utterances with streaming process.step SSE
// events (unless ?include_steps=0) followed by the authoritative speech.reply frame.
func UtteranceHandler(service contract.TurnHandler, auth contract.Authenticator) http.Handler {
	return UtteranceHandlerWithOptions(service, auth, UtteranceOptions{})
}

// UtteranceHandlerWithOptions serves POST /voice/utterances with optional EventBus integration.
func UtteranceHandlerWithOptions(service contract.TurnHandler, auth contract.Authenticator, opts UtteranceOptions) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		scheme, bearer, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(bearer) == "" {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		var input struct {
			SessionID string `json:"session_id"`
			VehicleID string `json:"vehicle_id"`
			RequestID string `json:"request_id"`
			Text      string `json:"text"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil || decoder.Decode(new(any)) != io.EOF ||
			input.SessionID == "" || input.RequestID == "" || len(input.RequestID) > 128 || strings.TrimSpace(input.Text) == "" {
			http.Error(w, "invalid utterance", http.StatusBadRequest)
			return
		}
		scope, err := auth.Authenticate(r.Context(), bearer, input.SessionID, input.VehicleID)
		if err != nil || !scope.Valid() || scope.SessionID != input.SessionID ||
			(input.VehicleID != "" && scope.VehicleID != input.VehicleID) {
			http.Error(w, "session or vehicle unavailable", http.StatusForbidden)
			return
		}

		turnID := "text-" + stableID(scopeKey(scope), input.RequestID)
		if opts.OnUtteranceAccepted != nil {
			opts.OnUtteranceAccepted(scope, turnID, input.RequestID, input.Text)
		}

		includeSteps := parseIncludeSteps(r.URL.Query().Get("include_steps"))
		turn := contract.VehicleTurn{
			Scope:           scope,
			VoiceTurnID:     turnID,
			RequestID:       input.RequestID,
			FinalTranscript: contract.FinalTranscript{ProviderSessionID: "text", InputItemID: input.RequestID, Text: input.Text},
		}

		if !includeSteps {
			reply, err := service.HandleVehicleTurn(r.Context(), turn)
			if err != nil {
				writePreStreamError(w, err)
				return
			}
			body, err := json.Marshal(reply)
			if err != nil {
				http.Error(w, "cannot encode reply", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Accel-Buffering", "no")
			_, _ = fmt.Fprintf(w, "id: %s\nevent: speech.reply\ndata: %s\n\n", stableID(scopeKey(scope), input.RequestID), body)
			flushResponse(w)
			return
		}

		// Streaming SSE mode: send headers immediately and stream process.step frames as they arrive.
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		flushResponse(w)

		var (
			stepCh   <-chan json.RawMessage
			stopSub  func()
			localCh  chan json.RawMessage
			closeOne sync.Once
			turnCtx  = r.Context()
		)
		if opts.SubscribeSteps != nil {
			stepCh, stopSub = opts.SubscribeSteps(r.Context(), scope, turnID, input.RequestID)
		} else {
			localCh = make(chan json.RawMessage, 64)
			stepCh = localCh
			stopSub = func() {
				closeOne.Do(func() { close(localCh) })
			}
			turnCtx = WithTurnEventSink(turnCtx, func(ev TurnEvent) {
				raw := marshalTurnEventStep(ev)
				if len(raw) == 0 {
					return
				}
				select {
				case localCh <- raw:
				default:
				}
			})
		}

		var writerWG sync.WaitGroup
		writerWG.Add(1)
		go func() {
			defer writerWG.Done()
			var stepSeq int
			for raw := range stepCh {
				stepSeq++
				frameID := input.RequestID + ":step:" + strconv.Itoa(stepSeq)
				_, _ = fmt.Fprintf(w, "id: %s\nevent: process.step\ndata: %s\n\n", frameID, raw)
				flushResponse(w)
			}
		}()

		reply, err := service.HandleVehicleTurn(turnCtx, turn)
		if stopSub != nil {
			stopSub()
		}
		writerWG.Wait()

		if err != nil {
			errPayload := buildTurnErrorPayload(err)
			body, _ := json.Marshal(errPayload)
			_, _ = fmt.Fprintf(w, "id: %s:error\nevent: turn.error\ndata: %s\n\n", stableID(scopeKey(scope), input.RequestID), body)
			flushResponse(w)
			return
		}

		body, err := json.Marshal(reply)
		if err != nil {
			return
		}
		_, _ = fmt.Fprintf(w, "id: %s\nevent: speech.reply\ndata: %s\n\n", stableID(scopeKey(scope), input.RequestID), body)
		flushResponse(w)
	})
}

func parseIncludeSteps(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

func flushResponse(w http.ResponseWriter) {
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func writePreStreamError(w http.ResponseWriter, err error) {
	var business *contract.BusinessError
	if errors.As(err, &business) {
		w.Header().Set("Content-Type", "application/json")
		status := http.StatusConflict
		if business.Code == contract.InvalidArgument {
			status = http.StatusBadRequest
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(business)
		return
	}
	http.Error(w, "vehicle turn unavailable", http.StatusBadGateway)
}

func buildTurnErrorPayload(err error) map[string]any {
	var business *contract.BusinessError
	if errors.As(err, &business) {
		return map[string]any{
			"code":      business.Code,
			"message":   business.Message,
			"retryable": business.Retryable,
		}
	}
	if errors.Is(err, context.Canceled) {
		return map[string]any{
			"code":      "TURN_CANCELLED",
			"message":   "vehicle turn cancelled",
			"retryable": false,
		}
	}
	return map[string]any{
		"code":      "TURN_UNAVAILABLE",
		"message":   "vehicle turn unavailable",
		"retryable": true,
	}
}

func marshalTurnEventStep(ev TurnEvent) json.RawMessage {
	obs := map[string]any{
		"action":     ev.Stage,
		"error_code": ev.ErrorCode,
	}
	switch ev.Stage {
	case "catalog.recall":
		obs["input"] = map[string]any{
			"pending_count":    ev.PendingCount,
			"pending_task_ids": ev.PendingTaskIDs,
		}
		obs["output"] = map[string]any{
			"candidate_tools": ev.CandidateTools,
		}
	case "hermes.run":
		obs["output"] = map[string]any{
			"run_id": ev.RunID,
			"status": ev.RunStatus,
		}
	case "turn.reply":
		obs["output"] = map[string]any{
			"status":     ev.ReplyStatus,
			"task_id":    ev.TaskID,
			"command_id": ev.CommandID,
			"text":       ev.ReplyText,
		}
	}
	if ev.ErrorMessage != "" {
		obs["error_message"] = ev.ErrorMessage
	}
	payload := map[string]any{
		"ts":         time.Now().UTC().Format(time.RFC3339Nano),
		"scope":      ev.Scope,
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
			"finished_at":   formatTimePtr(ev.FinishedAt),
			"duration_ms":   ev.DurationMs,
			"duration_kind": ev.DurationKind,
			"detail":        ev.Detail,
			"observation":   obs,
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	return raw
}

func formatTimePtr(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}
