// Package gateway serves authenticated car audio WebSockets.
package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/kirin7002/app_personal_agent/contract"
)

// GatewayEvent is an optional read-only observation of voice ASR, routing, and playback stages.
type GatewayEvent struct {
	Scope           contract.Scope
	TurnID          string
	RequestID       string
	Stage           string // "voice.asr", "voice.route", "voice.playback"
	Status          string // "running", "done", "error", "interrupted"
	StartedAt       time.Time
	FinishedAt      *time.Time
	DurationMs      *int64
	DurationKind    string // "boundary_wait", "measured_local"
	Transcript      string
	AudioBytes      int
	Route           string // "native_s2s" or "vehicle_hermes"
	PendingCount    int
	PlaybackID      string
	PlaybackSource  string // "native", "reply", "result"
	PlaybackReason  string
	AudioDurationMs int64
	ErrorMessage    string
}

type Gateway struct {
	Auth    contract.Authenticator
	Media   contract.S2SFactory
	Turns   contract.TurnHandler
	Pending contract.PendingReader
	Results contract.ResultReader
	TTS     contract.SpeechSynthesizer
	Logger  *slog.Logger
	OnEvent func(GatewayEvent)

	mu             sync.Mutex
	sessions       map[contract.Scope]*sessionState
	nextGeneration atomic.Uint64
	nextPlayback   atomic.Uint64
}

func (g *Gateway) emitEvent(ev GatewayEvent) {
	if g.OnEvent != nil {
		g.OnEvent(ev)
	}
}

type sessionState struct {
	mu         sync.Mutex
	listener   *connection
	usedTurns  map[string]time.Time
	deliveries map[string]delivery
	lastSeen   time.Time
}

type delivery struct {
	generation uint64
	at         time.Time
	settled    bool
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if g.Auth == nil || g.Media == nil || g.Turns == nil || g.Pending == nil || g.Results == nil || g.TTS == nil {
		http.Error(w, "voice gateway unavailable", http.StatusServiceUnavailable)
		return
	}
	scheme, bearer, found := strings.Cut(r.Header.Get("Authorization"), " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(bearer) == "" {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	socket, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer socket.CloseNow()
	socket.SetReadLimit(128 << 10)
	openCtx, stop := context.WithTimeout(r.Context(), 5*time.Second)
	var request contract.CarFrame
	if err := wsjson.Read(openCtx, socket, &request); err != nil {
		stop()
		return
	}
	if request.Type != contract.SessionOpen || request.SessionID == "" || request.InputFormat == nil ||
		*request.InputFormat != contract.InputFormat() {
		stop()
		_ = socket.Close(websocket.StatusPolicyViolation, "invalid session.open")
		return
	}
	scope, err := g.Auth.Authenticate(openCtx, bearer, request.SessionID, request.VehicleID)
	stop()
	if err != nil || !scope.Valid() || scope.SessionID != request.SessionID ||
		(request.VehicleID != "" && scope.VehicleID != request.VehicleID) {
		_ = socket.Close(websocket.StatusPolicyViolation, "session or vehicle unavailable")
		return
	}
	receipts, _ := g.Results.(contract.ResultPlaybackReceipts)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	c := &connection{
		gateway: g, receipts: receipts, ctx: ctx, cancel: cancel, socket: socket, scope: scope,
		frames: make(chan contract.CarFrame, 8), work: make(chan jobEvent, 8),
		signalled: make(map[string]bool), attempted: make(map[string]bool),
	}
	g.attach(c)
	defer g.detach(c)
	c.run()
}

func (g *Gateway) attach(c *connection) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.sessions == nil {
		g.sessions = make(map[contract.Scope]*sessionState)
	}
	now := time.Now()
	for scope, state := range g.sessions {
		if state.listener == nil && now.Sub(state.lastSeen) >= contract.ResultRetention {
			delete(g.sessions, scope)
		}
	}
	state := g.sessions[c.scope]
	if state == nil {
		state = &sessionState{usedTurns: make(map[string]time.Time), deliveries: make(map[string]delivery)}
		g.sessions[c.scope] = state
	}
	if state.listener != nil {
		state.listener.cancel()
	}
	state.listener = c
	c.state = state
}

func (g *Gateway) detach(c *connection) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if c.state.listener == c {
		c.state.listener = nil
		c.state.lastSeen = time.Now()
	}
}

func digest(parts ...string) string {
	encoded, _ := json.Marshal(parts)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func (s *sessionState) used(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	at, ok := s.usedTurns[id]
	return ok && time.Since(at) < contract.ResultRetention
}

func (s *sessionState) take(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for key, timeSeen := range s.usedTurns {
		if now.Sub(timeSeen) >= contract.ResultRetention {
			delete(s.usedTurns, key)
		}
	}
	if _, exists := s.usedTurns[id]; exists {
		return false
	}
	s.usedTurns[id] = now
	return true
}

func (s *sessionState) wasSettled(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deliveries[id].settled
}

func (s *sessionState) issued(id string, generation uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for key, previous := range s.deliveries {
		if now.Sub(previous.at) >= contract.ResultRetention {
			delete(s.deliveries, key)
		}
	}
	s.deliveries[id] = delivery{generation: generation, at: now}
}

func (s *sessionState) canSettle(id string, generation uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.deliveries[id]
	return ok && current.generation == generation && !current.settled
}

func (s *sessionState) settle(id string, generation uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if current, ok := s.deliveries[id]; ok && current.generation == generation {
		current.settled = true
		s.deliveries[id] = current
	}
}

func (g *Gateway) timing(stage string, start time.Time, args ...any) {
	if g.Logger != nil {
		data := append([]any{"stage", stage, "elapsed_ms", time.Since(start).Milliseconds()}, args...)
		g.Logger.Info("voice latency", data...)
	}
}

var errSequence = errors.New("audio.input sequence or length invalid")
