package integration

import (
	"context"
	"crypto/rand"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kirin7002/app_personal_agent/contract"
	"github.com/kirin7002/app_personal_agent/realtime/qwen"
	"github.com/kirin7002/app_personal_agent/vehicle/catalog"
	"github.com/kirin7002/app_personal_agent/vehicle/executor"
	"github.com/kirin7002/app_personal_agent/vehicle/mcp"
	"github.com/kirin7002/app_personal_agent/vehicle/tasks"
	"github.com/kirin7002/app_personal_agent/vehicle/turns"
)

func TestSimulatedScenarioA(t *testing.T) {
	testScenarioA(t, false)
}

func TestLiveScenarioA(t *testing.T) {
	if os.Getenv("H07_LIVE") != "1" {
		t.Skip("set H07_LIVE=1 to run live voice integration")
	}
	testScenarioA(t, true)
}

func testScenarioA(t *testing.T, live bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	scope := contract.Scope{UserID: "h07-user", SessionID: rand.Text(), VehicleID: "h07-car"}
	telemetry := catalog.NewMemoryTelemetry()
	cat := catalog.NewService(telemetry)
	validator := tasks.NewCatalogValidator(cat)
	var turnStore turns.Store = turns.NewMemoryStore()
	var taskStore tasks.Store = tasks.NewMemoryStore(validator)
	mcpToken := rand.Text()
	if live {
		if os.Getenv("H07_MYSQL_DSN") != "" {
			db := integrationDB(t, ctx)
			turnStore = &turns.MySQLStore{DB: db}
			taskStore = &tasks.MySQLStore{DB: db, Validator: validator}
		} else {
			sqlitePath := os.Getenv("H07_SQLITE_PATH")
			if sqlitePath == "" {
				sqlitePath = filepath.Join(t.TempDir(), "h07-live.sqlite")
			}
			db, err := tasks.OpenSQLite(sqlitePath)
			if err != nil {
				t.Fatalf("open SQLite fallback db: %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })
			turnStore = turns.NewSQLiteStore(db)
			taskStore = tasks.NewSQLiteStore(db, validator)
		}
	}

	service := mcp.NewServer(mcp.Config{ServiceToken: mcpToken, RequireMeta: true, Turns: turnStore, Tasks: taskStore, Catalog: cat})
	var audit callAudit
	mcpServer := httptest.NewServer(audit.wrap(service))
	t.Cleanup(mcpServer.Close)
	endpoint := mcpServer.URL + "/mcp"
	var media contract.S2SFactory = fixtureMedia{}
	var tts contract.SpeechSynthesizer = fixtureTTS{}
	turnService := &turns.Service{
		Store: turnStore, Pending: taskStore, Capabilities: cat, Progress: taskStore, RetryAdmission: true,
	}
	if live {
		factory, err := qwen.New(qwen.Config{
			APIKey: os.Getenv("DASHSCOPE_API_KEY"), WorkspaceID: os.Getenv("DASHSCOPE_WORKSPACE_ID"), Region: os.Getenv("DASHSCOPE_REGION"),
		})
		if err != nil {
			t.Fatal(err)
		}
		media, tts = factory, sampleTTS{}
		prompt, err := os.ReadFile("../hermes/prompts/vehicle_system_prompt.md")
		if err != nil {
			t.Fatal(err)
		}
		turnService.Instructions = string(prompt)
		turnService.Runs = startHermes(t, ctx, endpoint, mcpToken)
	} else {
		turnService.Runs = fixtureHermes(t, endpoint, mcpToken)
	}

	channel := executor.NewSimulatedChannel(telemetry)
	channel.Mode = executor.SimAcceptedOnly
	worker := executor.NewWorker("h07-"+rand.Text(), taskStore, cat, channel)
	workerCtx, stopWorker := context.WithCancel(ctx)
	defer stopWorker()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		_ = worker.Run(workerCtx)
	}()
	t.Cleanup(func() {
		stopWorker()
		<-stopped
	})
	evidence := runScenarioA(t, ctx, turnService, taskStore, media, tts, scope, live, func(commandID string) {
		injectVehicleReceipt(t, ctx, taskStore, worker, channel, scope, commandID)
	})
	audit.assertScenario(t, evidence.taskID)
	command, err := taskStore.GetCommand(ctx, evidence.commandID)
	if err != nil || command.Scope != scope || command.Params["position"] != "rear_left" || command.Status != contract.Completed ||
		command.Attempts != 1 || channel.SendCount(evidence.commandID) != 1 || command.TaskID != evidence.taskID {
		t.Fatalf("wrong confirmed command: %+v, %v", command, err)
	}
	result, err := taskStore.ListVehicleResults(ctx, scope, time.Now().Add(-time.Minute))
	if err != nil || len(result) != 1 || result[0].EventID != evidence.result.EventID {
		t.Fatalf("expected a single confirmed result: %+v, %v", result, err)
	}
}

func injectVehicleReceipt(t *testing.T, ctx context.Context, store tasks.Store, worker *executor.Worker,
	channel *executor.SimulatedChannel, scope contract.Scope, commandID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	wait := time.NewTicker(50 * time.Millisecond)
	defer wait.Stop()
	var command *tasks.OutboxCommand
	for {
		var err error
		command, err = store.GetCommand(ctx, commandID)
		if err != nil {
			t.Fatal(err)
		}
		if command.Status == contract.Executing {
			break
		}
		if command.Status != contract.PendingDispatch {
			t.Fatalf("vehicle acceptance unexpectedly became terminal: %s", command.Status)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-wait.C:
		}
	}
	before, err := store.ListVehicleResults(ctx, scope, time.Now().Add(-time.Minute))
	if err != nil || len(before) != 0 || command.DownstreamID == "" || command.Scope != scope {
		t.Fatalf("acceptance did not remain executing without result: %+v %v", before, err)
	}
	t.Logf("simulated vehicle accepted command_id=%s downstream_id=%s; status=executing events=0", commandID, command.DownstreamID)
	now := time.Now().UTC()
	channel.ApplyTelemetry(command.VehicleCommand, now)
	receipt := executor.VehicleReceipt{CommandID: commandID, DownstreamID: command.DownstreamID,
		Status: executor.ReceiptCompleted, OccurredAt: now}
	for range 2 {
		if err := worker.HandleReceipt(ctx, receipt); err != nil {
			t.Fatal(err)
		}
	}
}
