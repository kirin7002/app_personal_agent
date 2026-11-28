package turns

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kirin7002/app_personal_agent/contract"
	"github.com/kirin7002/app_personal_agent/vehicle/runs"
)

type testAuth struct{}

func (testAuth) Authenticate(_ context.Context, bearer, sessionID, vehicleID string) (contract.Scope, error) {
	if bearer != "good-token" {
		return contract.Scope{}, errors.New("unauthorized")
	}
	if vehicleID == "" {
		vehicleID = "car-1"
	}
	return contract.Scope{UserID: "U", SessionID: sessionID, VehicleID: vehicleID}, nil
}

type sseFrame struct {
	ID    string
	Event string
	Data  string
}

func parseSSEFrames(raw string) []sseFrame {
	var frames []sseFrame
	var cur sseFrame
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			if cur.Event != "" || cur.Data != "" || cur.ID != "" {
				frames = append(frames, cur)
				cur = sseFrame{}
			}
			continue
		}
		switch {
		case strings.HasPrefix(line, "id: "):
			cur.ID = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "event: "):
			cur.Event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			if cur.Data != "" {
				cur.Data += "\n"
			}
			cur.Data += strings.TrimPrefix(line, "data: ")
		}
	}
	if cur.Event != "" || cur.Data != "" || cur.ID != "" {
		frames = append(frames, cur)
	}
	return frames
}

func TestUtteranceHandlerStreamingStepsAndCompat(t *testing.T) {
	now := time.Now().UTC()
	store := NewMemoryStore()
	data := fakeData{now: now}
	svc := &Service{
		Store: store, Pending: data, Capabilities: data, Progress: data, Now: func() time.Time { return now },
		Runs: fakeRuns{
			start: func(context.Context, runs.StartRequest, contract.TurnMetadata, string) (runs.Run, error) {
				return runs.Run{ID: "run_http_1", Status: "queued"}, nil
			},
			wait: func(context.Context, string) (runs.Run, error) {
				return runs.Run{ID: "run_http_1", Status: "completed", Output: "ok"}, nil
			},
		},
	}
	handler := UtteranceHandler(svc, testAuth{})

	// 1. Default mode streams process.step frames followed by speech.reply.
	body, _ := json.Marshal(map[string]string{
		"session_id": "S", "vehicle_id": "car-1", "request_id": "req-stream-1", "text": "左后窗",
	})
	req := httptest.NewRequest(http.MethodPost, "/voice/utterances", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer good-token")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("unexpected status %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.HasPrefix(rr.Header().Get("Content-Type"), "text/event-stream") || rr.Header().Get("X-Accel-Buffering") != "no" {
		t.Fatalf("missing SSE headers: %v", rr.Header())
	}
	frames := parseSSEFrames(rr.Body.String())
	if len(frames) < 4 {
		t.Fatalf("expected process.step frames + speech.reply, got %d frames: %s", len(frames), rr.Body.String())
	}
	last := frames[len(frames)-1]
	if last.Event != "speech.reply" {
		t.Fatalf("expected final frame speech.reply, got %+v", last)
	}
	var reply contract.SpeechReply
	if err := json.Unmarshal([]byte(last.Data), &reply); err != nil {
		t.Fatalf("decode speech.reply: %v", err)
	}
	if reply.CommandID != "C88" || reply.TaskID != "T101" {
		t.Fatalf("unexpected reply: %+v", reply)
	}
	for _, f := range frames[:len(frames)-1] {
		if f.Event != "process.step" {
			t.Fatalf("expected process.step before speech.reply, got %+v", f)
		}
	}

	// 2. ?include_steps=0 omits intermediate steps and returns only speech.reply.
	body2, _ := json.Marshal(map[string]string{
		"session_id": "S", "vehicle_id": "car-1", "request_id": "req-compat-2", "text": "左后窗",
	})
	req2 := httptest.NewRequest(http.MethodPost, "/voice/utterances?include_steps=0", bytes.NewReader(body2))
	req2.Header.Set("Authorization", "Bearer good-token")
	req2.Header.Set("Content-Type", "application/json")
	rr2 := httptest.NewRecorder()
	handler.ServeHTTP(rr2, req2)
	frames2 := parseSSEFrames(rr2.Body.String())
	if len(frames2) != 1 || frames2[0].Event != "speech.reply" {
		t.Fatalf("expected single speech.reply frame with include_steps=0, got %+v", frames2)
	}
}

func TestUtteranceHandlerStreamError(t *testing.T) {
	now := time.Now().UTC()
	store := NewMemoryStore()
	data := fakeData{now: now}
	svc := &Service{
		Store: store, Pending: data, Capabilities: data, Progress: data, Now: func() time.Time { return now },
		Runs: fakeRuns{
			start: func(context.Context, runs.StartRequest, contract.TurnMetadata, string) (runs.Run, error) {
				return runs.Run{}, &contract.BusinessError{Code: contract.CapabilityUnavailable, Message: "hermes offline", Retryable: true}
			},
			wait: func(context.Context, string) (runs.Run, error) { return runs.Run{}, nil },
		},
	}
	handler := UtteranceHandler(svc, testAuth{})

	// Pre-flight 401 remains standard HTTP error.
	req401 := httptest.NewRequest(http.MethodPost, "/voice/utterances", strings.NewReader(`{}`))
	rr401 := httptest.NewRecorder()
	handler.ServeHTTP(rr401, req401)
	if rr401.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr401.Code)
	}

	// Mid-stream failure emits event: turn.error.
	body, _ := json.Marshal(map[string]string{
		"session_id": "S", "vehicle_id": "car-2", "request_id": "req-err-1", "text": "查状态",
	})
	req := httptest.NewRequest(http.MethodPost, "/voice/utterances", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer good-token")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	frames := parseSSEFrames(rr.Body.String())
	if len(frames) == 0 || frames[len(frames)-1].Event != "turn.error" {
		t.Fatalf("expected terminal turn.error frame, got %+v", frames)
	}
	if !strings.Contains(frames[len(frames)-1].Data, string(contract.CapabilityUnavailable)) {
		t.Fatalf("expected business error code in turn.error, got %s", frames[len(frames)-1].Data)
	}
}
