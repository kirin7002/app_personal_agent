package main

import (
	"context"
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/kirin7002/app_personal_agent/contract"
)

//go:embed web
var embeddedWebDir embed.FS

var defaultDemoSuggestions = []string{
	"把车窗关一下",
	"打开空调调到24度",
	"左后窗关了吗",
	"儿童锁怎么用",
}

func (a *demoApp) webFS() fs.FS {
	if strings.TrimSpace(a.cfg.WebDir) != "" {
		return os.DirFS(a.cfg.WebDir)
	}
	sub, err := fs.Sub(embeddedWebDir, "web")
	if err != nil {
		return embeddedWebDir
	}
	return sub
}

func (a *demoApp) probeHermesHealth(ctx context.Context) bool {
	base := strings.TrimRight(strings.TrimSpace(a.cfg.HermesURL), "/")
	if base == "" {
		return false
	}
	probeCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, base+"/health", nil)
	if err != nil {
		return false
	}
	client := &http.Client{
		Timeout: 1500 * time.Millisecond,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// handleAPIConfig serves GET /api/config.
func (a *demoApp) handleAPIConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	if _, ok := a.authorizeRequest(r); !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	profile := a.catalog.Profile(a.cfg.DemoVehicleID)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"storage":       a.storageKind,
		"hermes_url":    a.cfg.HermesURL,
		"hermes_ready":  a.probeHermesHealth(r.Context()),
		"voice_enabled": a.voiceEnabled,
		"tts_backend":   a.ttsBackend,
		"sim_mode":      a.channel.Mode,
		"debug_enabled": a.cfg.DebugEndpoints,
		"default_vehicle": map[string]any{
			"vehicle_id":     profile.VehicleID,
			"model":          profile.Model,
			"manual_version": profile.ManualVersion,
			"min_temp_c":     profile.MinTempC,
			"max_temp_c":     profile.MaxTempC,
		},
		"suggestions": defaultDemoSuggestions,
	})
}

// handleAPICapabilities serves GET /api/capabilities?vehicle_id=...
func (a *demoApp) handleAPICapabilities(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	scope, ok := a.authorizeRequest(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	vehicleID := strings.TrimSpace(r.URL.Query().Get("vehicle_id"))
	if vehicleID == "" {
		vehicleID = scope.VehicleID
	}
	if vehicleID == "" {
		vehicleID = a.cfg.DemoVehicleID
	}
	profile := a.catalog.Profile(vehicleID)
	caps := a.catalog.ListCapabilities(vehicleID)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"vehicle_id":     profile.VehicleID,
		"model":          profile.Model,
		"manual_version": profile.ManualVersion,
		"capabilities":   caps,
	})
}

type activeCommandView struct {
	CommandID    string          `json:"command_id"`
	TaskID       string          `json:"task_id"`
	Device       string          `json:"device"`
	Tool         string          `json:"tool"`
	Params       map[string]any  `json:"params"`
	Status       contract.Status `json:"status"`
	DownstreamID string          `json:"downstream_id,omitempty"`
}

// handleAPIState serves GET /api/state?session_id=...&vehicle_id=...
func (a *demoApp) handleAPIState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	scope, ok := a.authorizeRequest(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	now := time.Now().UTC()
	signals, _ := a.telemetry.ReadSignals(r.Context(), scope.VehicleID, demoSignalNames)
	pending, err := a.taskStore.ListPending(r.Context(), scope, now)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if pending == nil {
		pending = []contract.PendingCandidate{}
	}

	results, err := a.taskStore.ListVehicleResults(r.Context(), scope, now.Add(-contract.ResultRetention))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if results == nil {
		results = []contract.VehicleResultEvent{}
	}

	activeCommands := []activeCommandView{}
	if activeTasks, err := a.taskStore.ListTasks(r.Context(), scope, contract.ListTasksInput{
		Status: []contract.Status{contract.PendingDispatch, contract.Executing},
	}, now); err == nil {
		for _, t := range activeTasks.Tasks {
			if t.CommandID == "" {
				continue
			}
			cmd, err := a.taskStore.GetCommand(r.Context(), t.CommandID)
			if err != nil || cmd == nil {
				continue
			}
			params := cmd.Params
			if params == nil {
				params = map[string]any{}
			}
			activeCommands = append(activeCommands, activeCommandView{
				CommandID:    cmd.CommandID,
				TaskID:       cmd.TaskID,
				Device:       cmd.Device,
				Tool:         cmd.Tool,
				Params:       params,
				Status:       cmd.Status,
				DownstreamID: cmd.DownstreamID,
			})
		}
	}

	cursor, messages, processSteps := a.events.Snapshot(scope)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"cursor":          cursor,
		"scope":           scope,
		"telemetry":       signals,
		"pending_tasks":   pending,
		"active_commands": activeCommands,
		"vehicle_results": results,
		"messages":        messages,
		"process_steps":   processSteps,
	})
}

// handleStatic serves GET / and GET /static/* from either -web-dir or embedded web/.
func (a *demoApp) handleStatic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	reqPath := path.Clean("/" + r.URL.Path)
	// Never fall through API/voice/debug prefixes to static files.
	if strings.HasPrefix(reqPath, "/api/") || strings.HasPrefix(reqPath, "/voice/") || strings.HasPrefix(reqPath, "/debug/") {
		http.NotFound(w, r)
		return
	}

	var relPath string
	switch {
	case reqPath == "/":
		relPath = "index.html"
	case strings.HasPrefix(reqPath, "/static/"):
		relPath = strings.TrimPrefix(reqPath, "/static/")
	default:
		relPath = strings.TrimPrefix(reqPath, "/")
	}

	if relPath == "" || relPath == ".gitkeep" || strings.HasPrefix(relPath, ".") || strings.Contains(relPath, "/.") {
		http.NotFound(w, r)
		return
	}

	wfs := a.webFS()
	info, err := fs.Stat(wfs, relPath)
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFileFS(w, r, wfs, relPath)
}
