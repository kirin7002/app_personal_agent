// Command server runs the voice gateway, text utterance endpoint, internal Go MCP
// server, task expiry scanner, and simulated vehicle command worker for local demos.
package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/kirin7002/app_personal_agent/contract"
	"github.com/kirin7002/app_personal_agent/realtime/qwen"
	"github.com/kirin7002/app_personal_agent/vehicle/catalog"
	"github.com/kirin7002/app_personal_agent/vehicle/executor"
	"github.com/kirin7002/app_personal_agent/vehicle/mcp"
	"github.com/kirin7002/app_personal_agent/vehicle/runs"
	"github.com/kirin7002/app_personal_agent/vehicle/tasks"
	"github.com/kirin7002/app_personal_agent/vehicle/turns"
	"github.com/kirin7002/app_personal_agent/voice/gateway"
)

type config struct {
	ListenAddr       string
	MCPListenAddr    string
	SQLitePath       string
	MySQLDSN         string
	HermesURL        string
	HermesAPIKey     string
	MCPServiceToken  string
	DemoAuthToken    string
	DemoUserID       string
	DemoVehicleID    string
	PromptFile       string
	SimMode          string
	TTSMode          string
	DashScopeAPIKey  string
	DashScopeRegion  string
	DashScopeSpaceID string
	DashScopeModel   string
	DashScopeVoice   string
	DebugEndpoints   bool
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func parseConfig() config {
	hermesKeyDefault := envOr("HERMES_API_KEY", os.Getenv("API_SERVER_KEY"))
	cfg := config{}
	flag.StringVar(&cfg.ListenAddr, "listen", envOr("LISTEN_ADDR", "127.0.0.1:8080"), "Public HTTP/WS gateway listen address")
	flag.StringVar(&cfg.MCPListenAddr, "mcp-listen", envOr("MCP_LISTEN_ADDR", "127.0.0.1:18080"), "Internal Go MCP HTTP listen address")
	flag.StringVar(&cfg.SQLitePath, "sqlite", envOr("SQLITE_PATH", "data/vehicle-demo.sqlite"), "SQLite database path (used when MYSQL_DSN is empty)")
	flag.StringVar(&cfg.MySQLDSN, "mysql-dsn", strings.TrimSpace(os.Getenv("MYSQL_DSN")), "Optional MySQL DSN (overrides SQLite when set)")
	flag.StringVar(&cfg.HermesURL, "hermes-url", envOr("HERMES_BASE_URL", "http://127.0.0.1:8642"), "Hermes Runs API base URL")
	flag.StringVar(&cfg.HermesAPIKey, "hermes-key", hermesKeyDefault, "Hermes Runs API Bearer key (HERMES_API_KEY or API_SERVER_KEY)")
	flag.StringVar(&cfg.MCPServiceToken, "mcp-token", strings.TrimSpace(os.Getenv("VEHICLE_MCP_SERVICE_TOKEN")), "Bearer token required on POST /mcp")
	flag.StringVar(&cfg.DemoAuthToken, "demo-token", envOr("DEMO_AUTH_TOKEN", envOr("CAR_SIM_TOKEN", "demo-token")), "Demo Bearer token for /voice/* and /debug/*")
	flag.StringVar(&cfg.DemoUserID, "demo-user", envOr("DEMO_USER_ID", "demo-user"), "Demo user_id bound to DEMO_AUTH_TOKEN")
	flag.StringVar(&cfg.DemoVehicleID, "demo-vehicle", envOr("DEMO_VEHICLE_ID", "sim-car"), "Default vehicle_id bound to DEMO_AUTH_TOKEN")
	flag.StringVar(&cfg.PromptFile, "prompt", envOr("PROMPT_FILE", "hermes/prompts/vehicle_system_prompt.md"), "Path to Hermes vehicle system prompt file")
	flag.StringVar(&cfg.SimMode, "sim-mode", envOr("SIM_MODE", string(executor.SimAutoComplete)), "Simulated vehicle channel mode: auto_complete|accepted_only|retryable_once|rejected")
	flag.StringVar(&cfg.TTSMode, "tts", envOr("TTS_MODE", "auto"), "Demo TTS backend: auto|say|qwen|tone")
	flag.StringVar(&cfg.DashScopeAPIKey, "dashscope-key", strings.TrimSpace(os.Getenv("DASHSCOPE_API_KEY")), "Bailian DashScope API key (enables /voice/audio)")
	flag.StringVar(&cfg.DashScopeRegion, "dashscope-region", envOr("DASHSCOPE_REGION", "cn-beijing"), "Bailian DashScope region")
	flag.StringVar(&cfg.DashScopeSpaceID, "dashscope-workspace", strings.TrimSpace(os.Getenv("DASHSCOPE_WORKSPACE_ID")), "Optional Bailian workspace ID")
	flag.StringVar(&cfg.DashScopeModel, "dashscope-model", strings.TrimSpace(os.Getenv("DASHSCOPE_MODEL")), "Optional Qwen Realtime model override")
	flag.StringVar(&cfg.DashScopeVoice, "dashscope-voice", strings.TrimSpace(os.Getenv("DASHSCOPE_VOICE")), "Optional Qwen Realtime voice override")
	flag.BoolVar(&cfg.DebugEndpoints, "debug", envBool("DEBUG_ENDPOINTS", true), "Expose /debug/state and /debug/receipt behind DEMO_AUTH_TOKEN")
	flag.Parse()
	return cfg
}

type staticAuthenticator struct {
	token          string
	userID         string
	defaultVehicle string
}

func (a staticAuthenticator) Authenticate(_ context.Context, bearer, sessionID, vehicleID string) (contract.Scope, error) {
	if subtle.ConstantTimeCompare([]byte(bearer), []byte(a.token)) != 1 {
		return contract.Scope{}, errors.New("unauthorized token")
	}
	if strings.TrimSpace(sessionID) == "" {
		return contract.Scope{}, errors.New("session_id is required")
	}
	vid := strings.TrimSpace(vehicleID)
	if vid == "" {
		vid = a.defaultVehicle
	}
	return contract.Scope{
		UserID:    a.userID,
		SessionID: sessionID,
		VehicleID: vid,
	}, nil
}

type demoApp struct {
	cfg           config
	logger        *slog.Logger
	db            *sql.DB
	storageKind   string
	telemetry     *catalog.MemoryTelemetry
	catalog       *catalog.Service
	turnStore     turns.Store
	taskStore     tasks.Store
	channel       *executor.SimulatedChannel
	worker        *executor.Worker
	expiryWorker  *tasks.ExpiryWorker
	events        *EventBus
	publicHandler http.Handler
	mcpHandler    http.Handler
	voiceEnabled  bool
	ttsBackend    string
}

func buildApp(ctx context.Context, cfg config, logger *slog.Logger) (*demoApp, error) {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}
	if strings.TrimSpace(cfg.HermesAPIKey) == "" {
		return nil, errors.New("HERMES_API_KEY (or API_SERVER_KEY) is required")
	}
	if strings.TrimSpace(cfg.MCPServiceToken) == "" {
		return nil, errors.New("VEHICLE_MCP_SERVICE_TOKEN is required")
	}
	if strings.TrimSpace(cfg.DemoAuthToken) == "" {
		return nil, errors.New("DEMO_AUTH_TOKEN must not be empty")
	}

	simMode, err := parseSimMode(cfg.SimMode)
	if err != nil {
		return nil, err
	}

	promptBytes, err := os.ReadFile(cfg.PromptFile)
	if err != nil {
		return nil, fmt.Errorf("read prompt file %q: %w", cfg.PromptFile, err)
	}

	telemetry := catalog.NewMemoryTelemetry()
	seedDemoTelemetry(telemetry, cfg.DemoVehicleID, time.Now().UTC())
	cat := catalog.NewService(telemetry)
	validator := tasks.NewCatalogValidator(cat)

	var (
		db          *sql.DB
		storageKind string
		turnStore   turns.Store
		taskStore   tasks.Store
	)
	if cfg.MySQLDSN != "" {
		db, err = sql.Open("mysql", cfg.MySQLDSN)
		if err != nil {
			return nil, fmt.Errorf("open mysql: %w", err)
		}
		pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = db.PingContext(pingCtx)
		cancel()
		if err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("ping mysql: %w", err)
		}
		storageKind = "mysql"
		turnStore = &turns.MySQLStore{DB: db}
		taskStore = &tasks.MySQLStore{DB: db, Validator: validator}
	} else {
		if dir := filepath.Dir(cfg.SQLitePath); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0700); err != nil {
				return nil, fmt.Errorf("create sqlite dir %q: %w", dir, err)
			}
		}
		db, err = tasks.OpenSQLite(cfg.SQLitePath)
		if err != nil {
			return nil, fmt.Errorf("open sqlite %q: %w", cfg.SQLitePath, err)
		}
		storageKind = "sqlite"
		turnStore = turns.NewSQLiteStore(db)
		taskStore = tasks.NewSQLiteStore(db, validator)
	}

	eventBus := NewEventBus(defaultEventRingCapacity)
	eventBus.AddSecrets(cfg.HermesAPIKey, cfg.MCPServiceToken, cfg.DemoAuthToken, cfg.DashScopeAPIKey, cfg.MySQLDSN)

	hermesClient, err := runs.New(cfg.HermesURL, cfg.HermesAPIKey, nil)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init hermes runs client: %w", err)
	}
	hermesClient.RetryAdmission = true
	hermesClient.OnStreamEvent = eventBus.HandleHermesStreamEvent

	turnService := &turns.Service{
		Store:          turnStore,
		Pending:        taskStore,
		Capabilities:   cat,
		Progress:       taskStore,
		Runs:           hermesClient,
		Instructions:   string(promptBytes),
		RetryAdmission: true,
		OnEvent:        eventBus.HandleTurnEvent,
	}

	mcpServer := mcp.NewServer(mcp.Config{
		ServiceToken: cfg.MCPServiceToken,
		RequireMeta:  true,
		Turns:        turnStore,
		Tasks:        taskStore,
		Catalog:      cat,
		OnToolCall:   eventBus.HandleMCPToolEvent,
	})

	channel := executor.NewSimulatedChannel(telemetry)
	channel.Mode = simMode

	publishTelemetrySnapshot := func(primaryScope contract.Scope, vehicleID string) {
		signals, err := telemetry.ReadSignals(context.Background(), vehicleID, demoSignalNames)
		if err != nil {
			return
		}
		payload := make(map[string]any, len(signals))
		for name, pt := range signals {
			payload[name] = map[string]any{
				"Value":     pt.Value,
				"SampledAt": pt.SampledAt,
				"Available": pt.Available,
			}
		}
		eventBus.PublishVehicleTelemetry(primaryScope, vehicleID, payload)
	}

	worker := executor.NewWorker("demo-"+rand.Text()[:8], taskStore, cat, channel)
	worker.OnStage = eventBus.HandleExecutorStage
	worker.OnEvent = func(ev contract.VehicleResultEvent) {
		logger.Info("vehicle result emitted",
			"event_id", ev.EventID,
			"task_id", ev.TaskID,
			"command_id", ev.CommandID,
			"status", ev.Status,
			"vehicle_id", ev.Scope.VehicleID,
			"device", ev.Device,
			"tool", ev.Tool,
		)
		eventBus.HandleVehicleResult(ev)
		if ev.Status == contract.Completed {
			publishTelemetrySnapshot(ev.Scope, ev.Scope.VehicleID)
		}
	}

	expiryWorker := &tasks.ExpiryWorker{
		Store:    taskStore,
		Interval: 5 * time.Second,
		OnError: func(err error) {
			logger.Warn("expiry scanner error", "error", err)
		},
	}

	auth := staticAuthenticator{
		token:          cfg.DemoAuthToken,
		userID:         cfg.DemoUserID,
		defaultVehicle: cfg.DemoVehicleID,
	}

	var (
		qwenFactory  *qwen.Factory
		voiceEnabled bool
	)
	if cfg.DashScopeAPIKey != "" {
		qwenFactory, err = qwen.New(qwen.Config{
			APIKey:      cfg.DashScopeAPIKey,
			WorkspaceID: cfg.DashScopeSpaceID,
			Region:      cfg.DashScopeRegion,
			Model:       cfg.DashScopeModel,
			Voice:       cfg.DashScopeVoice,
		})
		if err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("init qwen realtime: %w", err)
		}
		voiceEnabled = true
	}

	tts, ttsBackend, err := selectDemoTTS(cfg.TTSMode, qwenFactory)
	if err != nil {
		_ = db.Close()
		return nil, err
	}

	app := &demoApp{
		cfg:          cfg,
		logger:       logger,
		db:           db,
		storageKind:  storageKind,
		telemetry:    telemetry,
		catalog:      cat,
		turnStore:    turnStore,
		taskStore:    taskStore,
		channel:      channel,
		worker:       worker,
		expiryWorker: expiryWorker,
		events:       eventBus,
		voiceEnabled: voiceEnabled,
		ttsBackend:   ttsBackend,
	}

	pubMux := http.NewServeMux()
	pubMux.HandleFunc("/healthz", app.handleHealth)
	pubMux.HandleFunc("/api/events/stream", app.handleEventsStream)
	pubMux.Handle("/voice/utterances", turns.UtteranceHandlerWithOptions(turnService, auth, turns.UtteranceOptions{
		OnUtteranceAccepted: func(scope contract.Scope, turnID, requestID, text string) {
			eventBus.PublishUserMessage(scope, turnID, requestID, "text", text)
		},
		SubscribeSteps: eventBus.SubscribeRequestSteps,
	}))
	if voiceEnabled {
		gw := &gateway.Gateway{
			Auth:    auth,
			Media:   qwenFactory,
			Turns:   turnService,
			Pending: taskStore,
			Results: taskStore,
			TTS:     tts,
			Logger:  logger,
			OnEvent: eventBus.HandleGatewayEvent,
		}
		pubMux.Handle("/voice/audio", gw)
	} else {
		pubMux.HandleFunc("/voice/audio", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "voice gateway disabled: set DASHSCOPE_API_KEY to enable /voice/audio (text /voice/utterances is active)", http.StatusServiceUnavailable)
		})
	}
	if cfg.DebugEndpoints {
		pubMux.HandleFunc("/debug/state", app.handleDebugState)
		pubMux.HandleFunc("/debug/receipt", app.handleDebugReceipt)
	}
	app.publicHandler = pubMux

	mcpMux := http.NewServeMux()
	mcpMux.Handle("/mcp", mcpServer)
	mcpMux.HandleFunc("/healthz", app.handleHealth)
	app.mcpHandler = mcpMux

	return app, nil
}

func (a *demoApp) Close() error {
	if a.db != nil {
		return a.db.Close()
	}
	return nil
}

func (a *demoApp) startBackground(ctx context.Context) func() {
	bgCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		_ = a.worker.Run(bgCtx)
	}()
	go func() {
		defer wg.Done()
		_ = a.expiryWorker.Run(bgCtx)
	}()
	go func() {
		defer wg.Done()
		refreshTelemetryLoop(bgCtx, a.telemetry, a.cfg.DemoVehicleID, 10*time.Second)
	}()
	return func() {
		cancel()
		wg.Wait()
	}
}

func (a *demoApp) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":        "ok",
		"storage":       a.storageKind,
		"hermes_url":    a.cfg.HermesURL,
		"voice_enabled": a.voiceEnabled,
		"tts_backend":   a.ttsBackend,
		"sim_mode":      a.channel.Mode,
		"demo_vehicle":  a.cfg.DemoVehicleID,
	})
}

func (a *demoApp) authorizeDebug(r *http.Request) bool {
	scheme, bearer, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(bearer), []byte(a.cfg.DemoAuthToken)) == 1
}

var demoSignalNames = []string{
	"window.front_left",
	"window.front_right",
	"window.rear_left",
	"window.rear_right",
	"hvac.power",
	"hvac.temperature_c",
}

func (a *demoApp) handleEventsStream(w http.ResponseWriter, r *http.Request) {
	if !a.authorizeDebug(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	sessionID := strings.TrimSpace(r.URL.Query().Get("session_id"))
	if sessionID == "" {
		sessionID = "web-demo-01"
	}
	vehicleID := strings.TrimSpace(r.URL.Query().Get("vehicle_id"))
	if vehicleID == "" {
		vehicleID = a.cfg.DemoVehicleID
	}
	scope := contract.Scope{
		UserID:    a.cfg.DemoUserID,
		SessionID: sessionID,
		VehicleID: vehicleID,
	}
	a.events.ServeSSE(w, r, scope)
}

func (a *demoApp) handleDebugState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	if !a.authorizeDebug(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	sessionID := strings.TrimSpace(r.URL.Query().Get("session_id"))
	if sessionID == "" {
		sessionID = "car-test"
	}
	vehicleID := strings.TrimSpace(r.URL.Query().Get("vehicle_id"))
	if vehicleID == "" {
		vehicleID = a.cfg.DemoVehicleID
	}
	scope := contract.Scope{UserID: a.cfg.DemoUserID, SessionID: sessionID, VehicleID: vehicleID}
	now := time.Now().UTC()

	signals, _ := a.telemetry.ReadSignals(r.Context(), vehicleID, demoSignalNames)
	pending, err := a.taskStore.ListPending(r.Context(), scope, now)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	results, err := a.taskStore.ListVehicleResults(r.Context(), scope, now.Add(-time.Hour))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"scope":           scope,
		"telemetry":       signals,
		"pending_tasks":   pending,
		"vehicle_results": results,
	})
}

func (a *demoApp) handleDebugReceipt(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if !a.authorizeDebug(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req struct {
		CommandID string `json:"command_id"`
		Status    string `json:"status"`
		Reason    string `json:"reason"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || strings.TrimSpace(req.CommandID) == "" {
		http.Error(w, "invalid receipt payload: command_id is required", http.StatusBadRequest)
		return
	}
	status := executor.ReceiptCompleted
	switch strings.ToLower(strings.TrimSpace(req.Status)) {
	case "", "completed":
		status = executor.ReceiptCompleted
	case "failed":
		status = executor.ReceiptFailed
	default:
		http.Error(w, "status must be completed or failed", http.StatusBadRequest)
		return
	}

	cmd, err := a.taskStore.GetCommand(r.Context(), req.CommandID)
	if err != nil {
		http.Error(w, fmt.Sprintf("lookup command: %v", err), http.StatusNotFound)
		return
	}
	now := time.Now().UTC()
	if status == executor.ReceiptCompleted {
		a.channel.ApplyTelemetry(cmd.VehicleCommand, now)
	}
	err = a.worker.HandleReceipt(r.Context(), executor.VehicleReceipt{
		CommandID:    cmd.CommandID,
		DownstreamID: cmd.DownstreamID,
		Status:       status,
		Reason:       req.Reason,
		OccurredAt:   now,
	})
	if err != nil {
		http.Error(w, fmt.Sprintf("handle receipt: %v", err), http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":         true,
		"command_id": cmd.CommandID,
		"status":     status,
	})
}

func parseSimMode(raw string) (executor.SimMode, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", string(executor.SimAutoComplete):
		return executor.SimAutoComplete, nil
	case string(executor.SimAcceptedOnly):
		return executor.SimAcceptedOnly, nil
	case string(executor.SimRetryableOnce):
		return executor.SimRetryableOnce, nil
	case string(executor.SimRejected):
		return executor.SimRejected, nil
	default:
		return "", fmt.Errorf("unsupported SIM_MODE %q (use auto_complete, accepted_only, retryable_once, or rejected)", raw)
	}
}

func seedDemoTelemetry(tel *catalog.MemoryTelemetry, vehicleID string, now time.Time) {
	tel.Set(vehicleID, "window.front_left", "closed", now)
	tel.Set(vehicleID, "window.front_right", "closed", now)
	tel.Set(vehicleID, "window.rear_left", "open", now)
	tel.Set(vehicleID, "window.rear_right", "closed", now)
	tel.Set(vehicleID, "hvac.power", "off", now)
	tel.Set(vehicleID, "hvac.temperature_c", 24.0, now)
}

func refreshTelemetryLoop(ctx context.Context, tel *catalog.MemoryTelemetry, vehicleID string, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now().UTC()
			pts, err := tel.ReadSignals(ctx, vehicleID, demoSignalNames)
			if err != nil {
				continue
			}
			for name, pt := range pts {
				if pt.Available {
					tel.Set(vehicleID, name, pt.Value, now)
				}
			}
		}
	}
}

func main() {
	cfg := parseConfig()
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	app, err := buildApp(ctx, cfg, logger)
	if err != nil {
		logger.Error("failed to initialize demo server", "error", err)
		os.Exit(1)
	}
	defer app.Close()

	stopWorkers := app.startBackground(ctx)
	defer stopWorkers()

	mcpListener, err := net.Listen("tcp", cfg.MCPListenAddr)
	if err != nil {
		logger.Error("listen MCP address failed", "addr", cfg.MCPListenAddr, "error", err)
		os.Exit(1)
	}
	pubListener, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		_ = mcpListener.Close()
		logger.Error("listen public address failed", "addr", cfg.ListenAddr, "error", err)
		os.Exit(1)
	}

	mcpSrv := &http.Server{Handler: app.mcpHandler, ReadHeaderTimeout: 10 * time.Second}
	pubSrv := &http.Server{Handler: app.publicHandler, ReadHeaderTimeout: 10 * time.Second}

	errCh := make(chan error, 2)
	go func() {
		if err := mcpSrv.Serve(mcpListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("mcp server: %w", err)
		}
	}()
	go func() {
		if err := pubSrv.Serve(pubListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("public server: %w", err)
		}
	}()

	logger.Info("demo vehicle agent server started",
		"public_addr", pubListener.Addr().String(),
		"mcp_addr", mcpListener.Addr().String(),
		"storage", app.storageKind,
		"hermes_url", cfg.HermesURL,
		"voice_enabled", app.voiceEnabled,
		"tts_backend", app.ttsBackend,
		"sim_mode", app.channel.Mode,
		"demo_vehicle", cfg.DemoVehicleID,
	)

	select {
	case <-ctx.Done():
		logger.Info("shutting down demo server...")
	case serveErr := <-errCh:
		logger.Error("server exited unexpectedly", "error", serveErr)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = pubSrv.Shutdown(shutdownCtx)
	_ = mcpSrv.Shutdown(shutdownCtx)
}
