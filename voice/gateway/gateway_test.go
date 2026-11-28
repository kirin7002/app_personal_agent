package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/kirin7002/app_personal_agent/contract"
	"github.com/kirin7002/app_personal_agent/vehicle/tasks"
)

type fakeBackend struct {
	mu      sync.Mutex
	calls   int
	early   bool
	results []contract.VehicleResultEvent
}

func (*fakeBackend) Authenticate(_ context.Context, token, session, car string) (contract.Scope, error) {
	if token != "car-token" || car != "car-1" {
		return contract.Scope{}, errors.New("invalid vehicle")
	}
	return contract.Scope{UserID: "user", SessionID: session, VehicleID: car}, nil
}

func (*fakeBackend) ListPending(context.Context, contract.Scope, time.Time) ([]contract.PendingCandidate, error) {
	return nil, nil
}

func (b *fakeBackend) HandleVehicleTurn(_ context.Context, turn contract.VehicleTurn) (contract.SpeechReply, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	if b.early {
		b.results = []contract.VehicleResultEvent{{
			Scope: turn.Scope, Operation: contract.Operation{Device: "window", Tool: "close"},
			EventID: "C88:completed", RequestID: turn.RequestID, TaskID: "T101", CommandID: "C88",
			Status: contract.Completed, OccurredAt: time.Now(),
		}}
		return contract.SpeechReply{VoiceTurnID: turn.VoiceTurnID, RequestID: turn.RequestID, TaskID: "T101",
			CommandID: "C88", Status: contract.PendingDispatch, Text: "正在处理"}, nil
	}
	return contract.SpeechReply{VoiceTurnID: turn.VoiceTurnID, RequestID: turn.RequestID, TaskID: "T101",
		Status: contract.PendingReply, Text: "要关哪扇车窗？"}, nil
}

func (*fakeBackend) CancelVehicleTurn(context.Context, contract.Scope, string) error { return nil }

func (b *fakeBackend) ListVehicleResults(context.Context, contract.Scope, time.Time) ([]contract.VehicleResultEvent, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]contract.VehicleResultEvent(nil), b.results...), nil
}

func (*fakeBackend) Synthesize(_ context.Context, _ string, emit func([]byte) error) error {
	return emit([]byte{8, 9})
}

type fakeFactory struct {
	opened    atomic.Int64
	cancelled atomic.Int64
}

type fakeMedia struct {
	parent *fakeFactory
	input  byte
	part   int
}

func (f *fakeFactory) Connect(context.Context) (contract.S2SSession, error) {
	f.opened.Add(1)
	return &fakeMedia{parent: f}, nil
}

func (m *fakeMedia) AppendAudio(_ context.Context, pcm []byte) error {
	m.input = pcm[0]
	return nil
}

func (m *fakeMedia) Commit(context.Context) (contract.FinalTranscript, error) {
	m.part++
	text := "把车窗关一下"
	if m.input == 2 {
		text = "你好"
	}
	return contract.FinalTranscript{ProviderSessionID: "q", InputItemID: string(rune('A' + m.part)), Text: text}, nil
}

func (*fakeMedia) CreateResponse(context.Context, string) (contract.S2SResponse, error) {
	return &fakeResponse{}, nil
}

func (m *fakeMedia) CancelResponse(context.Context) error {
	m.parent.cancelled.Add(1)
	return nil
}

func (*fakeMedia) Close() error { return nil }

type fakeResponse struct{ sent bool }

func (*fakeResponse) ID() string { return "chat-response" }

func (f *fakeResponse) Next(ctx context.Context) ([]byte, error) {
	if !f.sent {
		f.sent = true
		return []byte{1, 2}, nil
	}
	<-ctx.Done()
	// Simulate a chunk already in flight while the car interrupts.
	return []byte{3, 4}, io.EOF
}

func fixture(t *testing.T) (*httptest.Server, *fakeBackend, *fakeFactory) {
	t.Helper()
	backend, media := &fakeBackend{}, &fakeFactory{}
	server := httptest.NewServer(&Gateway{
		Auth: backend, Media: media, Turns: backend, Pending: backend, Results: backend, TTS: backend,
	})
	t.Cleanup(server.Close)
	return server, backend, media
}

func dial(t *testing.T, ctx context.Context, server *httptest.Server) (*websocket.Conn, uint64) {
	t.Helper()
	socket, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer car-token"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { socket.CloseNow() })
	format := contract.InputFormat()
	write(t, ctx, socket, contract.CarFrame{Type: contract.SessionOpen, SessionID: "session", VehicleID: "car-1", InputFormat: &format})
	return socket, until(t, ctx, socket, contract.SessionReady).ConnectionGeneration
}

func write(t *testing.T, ctx context.Context, socket *websocket.Conn, frame contract.CarFrame) {
	t.Helper()
	if err := wsjson.Write(ctx, socket, frame); err != nil {
		t.Fatal(err)
	}
}

func until(t *testing.T, ctx context.Context, socket *websocket.Conn, kind string) contract.CarFrame {
	t.Helper()
	for {
		var frame contract.CarFrame
		if err := wsjson.Read(ctx, socket, &frame); err != nil {
			t.Fatalf("awaiting %s: %v", kind, err)
		}
		if frame.Type == kind {
			return frame
		}
		if frame.Type == contract.SessionError {
			t.Fatalf("session error: %s", frame.Reason)
		}
	}
}

func utterance(t *testing.T, ctx context.Context, socket *websocket.Conn, generation uint64, id string, pcm byte) {
	t.Helper()
	frame := contract.CarFrame{Type: contract.SpeechStarted, ConnectionGeneration: generation, VoiceTurnID: id}
	write(t, ctx, socket, frame)
	frame.Type, frame.Sequence, frame.Audio = contract.AudioInput, 1, []byte{pcm, 0}
	write(t, ctx, socket, frame)
	write(t, ctx, socket, frame) // Same sequence is a duplicate.
	frame.Type, frame.Sequence, frame.Audio = contract.SpeechEnded, 0, nil
	write(t, ctx, socket, frame)
}

func TestInterruptNativeAudio(t *testing.T) {
	server, backend, media := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	socket, generation := dial(t, ctx, server)
	utterance(t, ctx, socket, generation, "hello", 2)
	old := until(t, ctx, socket, contract.AudioOutput)
	utterance(t, ctx, socket, generation, "car", 1)
	stop := until(t, ctx, socket, contract.PlaybackStop)
	if stop.Reason != "interrupted" || stop.PlaybackID != old.PlaybackID {
		t.Fatalf("wrong interruption %+v", stop)
	}
	reply := until(t, ctx, socket, contract.SpeechReplyEvent)
	if reply.Reply == nil || reply.Reply.TaskID != "T101" {
		t.Fatalf("unexpected reply %+v", reply)
	}
	next := until(t, ctx, socket, contract.AudioOutput)
	if next.PlaybackID == old.PlaybackID || next.PlaybackGeneration <= old.PlaybackGeneration || next.Audio[0] != 8 {
		t.Fatalf("old playback leaked: %+v", next)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if media.cancelled.Load() != 1 || backend.calls != 1 {
		t.Fatalf("cancellations %d, Hermes calls %d", media.cancelled.Load(), backend.calls)
	}
}

func TestReconnectResultAndRepeatedTurn(t *testing.T) {
	server, backend, _ := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	socket, first := dial(t, ctx, server)
	utterance(t, ctx, socket, first, "turn-1", 1)
	pending := until(t, ctx, socket, contract.SpeechReplyEvent)
	until(t, ctx, socket, contract.PlaybackStop)
	_ = socket.CloseNow()
	backend.mu.Lock()
	event := contract.VehicleResultEvent{Scope: contract.Scope{UserID: "user", SessionID: "session", VehicleID: "car-1"},
		Operation: contract.Operation{Device: "window", Tool: "close"}, EventID: "C88:completed",
		TaskID: "T101", CommandID: "C88", RequestID: pending.Reply.RequestID, Status: contract.Completed, OccurredAt: time.Now()}
	backend.results = []contract.VehicleResultEvent{event, event}
	backend.mu.Unlock()

	socket, second := dial(t, ctx, server)
	if second <= first {
		t.Fatal("connection generation did not advance")
	}
	result := until(t, ctx, socket, contract.VehicleResult)
	if result.Result == nil || result.Result.CommandID != "C88" {
		t.Fatalf("missing disconnected vehicle result: %+v", result)
	}
	played := until(t, ctx, socket, contract.PlaybackStop)
	write(t, ctx, socket, contract.CarFrame{Type: contract.PlaybackAck, PlaybackID: played.PlaybackID, PlaybackGeneration: played.PlaybackGeneration})
	utterance(t, ctx, socket, first, "stale", 1)
	utterance(t, ctx, socket, second, "turn-1", 1)
	utterance(t, ctx, socket, second, "turn-2", 1)
	next := until(t, ctx, socket, contract.SpeechReplyEvent)
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.calls != 2 || next.Reply.VoiceTurnID == pending.Reply.VoiceTurnID {
		t.Fatalf("a duplicate or obsolete frame reached Hermes: calls=%d, reply=%+v", backend.calls, next.Reply)
	}
}

func TestEarlyVehicleConfirmationSupersedesProgress(t *testing.T) {
	server, backend, _ := fixture(t)
	backend.mu.Lock()
	backend.early = true
	backend.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	socket, generation := dial(t, ctx, server)
	utterance(t, ctx, socket, generation, "close", 1)
	reply := until(t, ctx, socket, contract.SpeechReplyEvent)
	if reply.Reply == nil || reply.Reply.Status != contract.Completed || reply.Reply.Text != "" {
		t.Fatalf("stale progress made it to playback: %+v", reply)
	}
	result := until(t, ctx, socket, contract.VehicleResult)
	if result.Result == nil || result.Result.RequestID != reply.Reply.RequestID {
		t.Fatalf("missing result correlation: %+v", result)
	}
	audio := until(t, ctx, socket, contract.AudioOutput)
	if !strings.HasPrefix(audio.PlaybackID, "result-") {
		t.Fatalf("expected the result playback, got %+v", audio)
	}
}

type durableResults struct {
	*fakeBackend
	contract.ResultPlaybackReceipts
}

func TestCompletedPlaybackSurvivesGatewayRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	scope := contract.Scope{UserID: "user", SessionID: "session", VehicleID: "car-1"}
	backend := &fakeBackend{results: []contract.VehicleResultEvent{{
		Scope: scope, Operation: contract.Operation{Device: "window", Tool: "close"},
		EventID: "C88:completed", RequestID: "R2", TaskID: "T101", CommandID: "C88",
		Status: contract.Completed, OccurredAt: time.Now(),
	}}}
	file := filepath.Join(t.TempDir(), "playback.sqlite")
	open := func() (*httptest.Server, *tasks.SQLiteStore, func()) {
		t.Helper()
		db, err := tasks.OpenSQLite(file)
		if err != nil {
			t.Fatal(err)
		}
		store := tasks.NewSQLiteStore(db, nil)
		server := httptest.NewServer(&Gateway{
			Auth: backend, Media: &fakeFactory{}, Turns: backend, Pending: backend, TTS: backend,
			Results: durableResults{fakeBackend: backend, ResultPlaybackReceipts: store},
		})
		return server, store, func() {
			server.Close()
			db.Close()
		}
	}

	first, store, closeFirst := open()
	defer closeFirst()
	car, _ := dial(t, ctx, first)
	completed := until(t, ctx, car, contract.PlaybackStop)
	write(t, ctx, car, contract.CarFrame{
		Type: contract.PlaybackAck, PlaybackID: completed.PlaybackID, PlaybackGeneration: completed.PlaybackGeneration,
	})
	if err := car.Close(websocket.StatusNormalClosure, "reconnect"); err != nil {
		t.Fatal(err)
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		saved, err := store.ResultPlaybackSettled(ctx, scope, completed.PlaybackID, time.Now().Add(-time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if saved {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("playback acknowledgment was lost on disconnect")
		case <-ticker.C:
		}
	}
	closeFirst()

	second, _, closeSecond := open()
	defer closeSecond()
	car, generation := dial(t, ctx, second)
	if result := until(t, ctx, car, contract.VehicleResult); result.Result.EventID != "C88:completed" {
		t.Fatalf("vehicle result was lost after restart: %+v", result)
	}
	utterance(t, ctx, car, generation, "new-turn", 1)
	next := until(t, ctx, car, contract.PlaybackStart)
	if next.PlaybackID == completed.PlaybackID || !strings.HasPrefix(next.PlaybackID, "reply-") {
		t.Fatalf("acknowledged result replayed after restart: %+v", next)
	}
}

func TestGatewayOnEventASRRoutePlayback(t *testing.T) {
	backend := &fakeBackend{}
	factory := &fakeFactory{}
	var (
		mu     sync.Mutex
		events []GatewayEvent
	)
	gw := &Gateway{
		Auth: backend, Media: factory, Turns: backend, Pending: backend, Results: backend, TTS: backend,
		OnEvent: func(ev GatewayEvent) {
			mu.Lock()
			events = append(events, ev)
			mu.Unlock()
		},
	}
	srv := httptest.NewServer(gw)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	car, gen := dial(t, ctx, srv)
	utterance(t, ctx, car, gen, "obs-turn-1", 1)
	stopFrame := until(t, ctx, car, contract.PlaybackStop)
	write(t, ctx, car, contract.CarFrame{
		Type: contract.PlaybackAck, PlaybackID: stopFrame.PlaybackID, PlaybackGeneration: stopFrame.PlaybackGeneration,
	})
	_ = car.Close(websocket.StatusNormalClosure, "done")

	mu.Lock()
	got := append([]GatewayEvent(nil), events...)
	mu.Unlock()

	var hasASR, hasRoute, hasPlayStart, hasPlayDone bool
	for _, ev := range got {
		switch ev.Stage {
		case "voice.asr":
			if ev.Status == "done" && ev.Transcript == "把车窗关一下" && ev.AudioBytes == 2 {
				hasASR = true
			}
		case "voice.route":
			if ev.Status == "done" && ev.Route == "vehicle_hermes" {
				hasRoute = true
			}
		case "voice.playback":
			if ev.Status == "running" && ev.PlaybackSource == "reply" {
				hasPlayStart = true
			}
			if ev.Status == "done" && ev.PlaybackSource == "reply" && ev.AudioBytes == 2 && ev.PlaybackReason == "completed" {
				hasPlayDone = true
			}
		}
	}
	if !hasASR || !hasRoute || !hasPlayStart || !hasPlayDone {
		t.Fatalf("missing expected gateway observation events (asr=%v route=%v playStart=%v playDone=%v): %+v",
			hasASR, hasRoute, hasPlayStart, hasPlayDone, got)
	}
}
