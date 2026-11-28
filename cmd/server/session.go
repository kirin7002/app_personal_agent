package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/kirin7002/app_personal_agent/contract"
)

const (
	sessionCookieName = "vehicle_demo_session"
	defaultWebSession = "web-demo-01"
	sessionMaxAge     = 24 * time.Hour
)

type webSession struct {
	ID        string
	Scope     contract.Scope
	CreatedAt time.Time
	LastSeen  time.Time
}

type sessionStore struct {
	mu       sync.RWMutex
	sessions map[string]webSession
}

func newSessionStore() *sessionStore {
	return &sessionStore{
		sessions: make(map[string]webSession),
	}
}

func (s *sessionStore) issue(scope contract.Scope) webSession {
	now := time.Now().UTC()
	id := rand.Text()
	ws := webSession{
		ID:        id,
		Scope:     scope,
		CreatedAt: now,
		LastSeen:  now,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Prune expired sessions opportunistically.
	cutoff := now.Add(-sessionMaxAge)
	for k, existing := range s.sessions {
		if existing.LastSeen.Before(cutoff) {
			delete(s.sessions, k)
		}
	}
	s.sessions[id] = ws
	return ws
}

func (s *sessionStore) get(id string) (webSession, bool) {
	if strings.TrimSpace(id) == "" {
		return webSession{}, false
	}
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	ws, ok := s.sessions[id]
	if !ok {
		return webSession{}, false
	}
	if now.Sub(ws.LastSeen) > sessionMaxAge {
		delete(s.sessions, id)
		return webSession{}, false
	}
	ws.LastSeen = now
	s.sessions[id] = ws
	return ws, true
}

func (s *sessionStore) updateScope(id string, scope contract.Scope) (webSession, bool) {
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	ws, ok := s.sessions[id]
	if !ok || now.Sub(ws.LastSeen) > sessionMaxAge {
		return webSession{}, false
	}
	ws.Scope = scope
	ws.LastSeen = now
	s.sessions[id] = ws
	return ws, true
}

type webSessionCtxKey struct{}

func webSessionFromContext(ctx context.Context) (webSession, bool) {
	ws, ok := ctx.Value(webSessionCtxKey{}).(webSession)
	return ws, ok
}

func (a *demoApp) validBearer(r *http.Request) (present bool, valid bool) {
	hdr := strings.TrimSpace(r.Header.Get("Authorization"))
	if hdr == "" {
		return false, false
	}
	scheme, bearer, ok := strings.Cut(hdr, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return true, false
	}
	return true, subtle.ConstantTimeCompare([]byte(strings.TrimSpace(bearer)), []byte(a.cfg.DemoAuthToken)) == 1
}

func (a *demoApp) lookupSessionCookie(r *http.Request) (webSession, bool) {
	if a.sessions == nil {
		return webSession{}, false
	}
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || strings.TrimSpace(cookie.Value) == "" {
		return webSession{}, false
	}
	return a.sessions.get(cookie.Value)
}

// withSessionAuth adapts a valid vehicle_demo_session Cookie to an internal
// Authorization: Bearer header for downstream handlers (/voice/utterances and
// /voice/audio) without altering contract.Authenticator or underlying packages.
// Explicit Authorization headers always take priority over Cookie auth.
func (a *demoApp) withSessionAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.TrimSpace(r.Header.Get("Authorization")) != "" {
			next.ServeHTTP(w, r)
			return
		}
		if ws, ok := a.lookupSessionCookie(r); ok {
			r2 := r.Clone(context.WithValue(r.Context(), webSessionCtxKey{}, ws))
			r2.Header.Set("Authorization", "Bearer "+a.cfg.DemoAuthToken)
			next.ServeHTTP(w, r2)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// authorizeRequest authenticates using priority: Authorization: Bearer, then Cookie.
func (a *demoApp) authorizeRequest(r *http.Request) (contract.Scope, bool) {
	bearerPresent, bearerOK := a.validBearer(r)
	if bearerPresent {
		if !bearerOK {
			return contract.Scope{}, false
		}
		ws, hasCookie := a.lookupSessionCookie(r)
		sessionID := strings.TrimSpace(r.URL.Query().Get("session_id"))
		if sessionID == "" {
			if hasCookie {
				sessionID = ws.Scope.SessionID
			} else {
				sessionID = defaultWebSession
			}
		}
		vehicleID := strings.TrimSpace(r.URL.Query().Get("vehicle_id"))
		if vehicleID == "" {
			if hasCookie {
				vehicleID = ws.Scope.VehicleID
			} else {
				vehicleID = a.cfg.DemoVehicleID
			}
		}
		return contract.Scope{
			UserID:    a.cfg.DemoUserID,
			SessionID: sessionID,
			VehicleID: vehicleID,
		}, true
	}

	ws, ok := webSessionFromContext(r.Context())
	if !ok {
		ws, ok = a.lookupSessionCookie(r)
	}
	if !ok {
		return contract.Scope{}, false
	}
	scope := ws.Scope
	if qSess := strings.TrimSpace(r.URL.Query().Get("session_id")); qSess != "" {
		scope.SessionID = qSess
	}
	if qVeh := strings.TrimSpace(r.URL.Query().Get("vehicle_id")); qVeh != "" {
		scope.VehicleID = qVeh
	}
	return scope, scope.Valid()
}

// handleSession serves POST /api/session to establish or renew a browser Cookie session.
func (a *demoApp) handleSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		SessionID string `json:"session_id"`
		VehicleID string `json:"vehicle_id"`
		Token     string `json:"token"`
	}
	if r.Body != nil {
		raw, err := io.ReadAll(io.LimitReader(r.Body, 16<<10))
		if err != nil {
			http.Error(w, "failed to read body", http.StatusBadRequest)
			return
		}
		if len(strings.TrimSpace(string(raw))) > 0 {
			dec := json.NewDecoder(strings.NewReader(string(raw)))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
				http.Error(w, "invalid session payload", http.StatusBadRequest)
				return
			}
		}
	}

	bearerPresent, bearerOK := a.validBearer(r)
	if bearerPresent && !bearerOK {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	existing, hasCookie := a.lookupSessionCookie(r)

	if strings.TrimSpace(req.Token) != "" {
		if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(req.Token)), []byte(a.cfg.DemoAuthToken)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	} else if a.cfg.RequireSessionToken && !bearerOK && !hasCookie {
		http.Error(w, "token required", http.StatusUnauthorized)
		return
	}

	sessionID := strings.TrimSpace(req.SessionID)
	if sessionID == "" {
		if hasCookie && existing.Scope.SessionID != "" {
			sessionID = existing.Scope.SessionID
		} else {
			sessionID = defaultWebSession
		}
	}
	vehicleID := strings.TrimSpace(req.VehicleID)
	if vehicleID == "" {
		if hasCookie && existing.Scope.VehicleID != "" {
			vehicleID = existing.Scope.VehicleID
		} else {
			vehicleID = a.cfg.DemoVehicleID
		}
	}

	scope := contract.Scope{
		UserID:    a.cfg.DemoUserID,
		SessionID: sessionID,
		VehicleID: vehicleID,
	}

	var ws webSession
	if hasCookie {
		if updated, ok := a.sessions.updateScope(existing.ID, scope); ok {
			ws = updated
		} else {
			ws = a.sessions.issue(scope)
		}
	} else {
		ws = a.sessions.issue(scope)
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    ws.ID,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   r.TLS != nil,
		MaxAge:   int(sessionMaxAge.Seconds()),
	})

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"user_id":    scope.UserID,
		"session_id": scope.SessionID,
		"vehicle_id": scope.VehicleID,
		"cursor":     a.events.Cursor(scope),
	})
}
