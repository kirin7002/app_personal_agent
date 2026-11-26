package integration

import (
	"context"
	"crypto/rand"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/kirin7002/app_personal_agent/contract"
	"github.com/kirin7002/app_personal_agent/vehicle/catalog"
	"github.com/kirin7002/app_personal_agent/vehicle/executor"
	"github.com/kirin7002/app_personal_agent/vehicle/mcp"
	"github.com/kirin7002/app_personal_agent/vehicle/tasks"
	"github.com/kirin7002/app_personal_agent/vehicle/turns"
)

// TestSQLiteScenarioA runs the full two-turn H08+H02+H04+H03+H06+H05 voice workflow
// backed by a real WAL SQLite file database (zero external MySQL required).
func TestSQLiteScenarioA(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dbPath := filepath.Join(t.TempDir(), "h07-scenario-a.sqlite")
	db, err := tasks.OpenSQLite(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	scope := contract.Scope{UserID: "h07-sqlite-user", SessionID: rand.Text(), VehicleID: "h07-car"}
	telemetry := catalog.NewMemoryTelemetry()
	cat := catalog.NewService(telemetry)
	validator := tasks.NewCatalogValidator(cat)
	turnStore := turns.NewSQLiteStore(db)
	taskStore := tasks.NewSQLiteStore(db, validator)
	mcpToken := rand.Text()

	// mcp.NewServer auto-detects turnStore's SQLite LockWritableTurn without extra wiring!
	service := mcp.NewServer(mcp.Config{
		ServiceToken: mcpToken,
		RequireMeta:  true,
		Turns:        turnStore,
		Tasks:        taskStore,
		Catalog:      cat,
	})
	var audit callAudit
	mcpServer := httptest.NewServer(audit.wrap(service))
	t.Cleanup(mcpServer.Close)
	endpoint := mcpServer.URL + "/mcp"

	turnService := &turns.Service{
		Store:          turnStore,
		Pending:        taskStore,
		Capabilities:   cat,
		Progress:       taskStore,
		Runs:           fixtureHermes(t, endpoint, mcpToken),
		RetryAdmission: true,
	}

	channel := executor.NewSimulatedChannel(telemetry)
	channel.Mode = executor.SimAcceptedOnly
	worker := executor.NewWorker("h07-sqlite-"+rand.Text(), taskStore, cat, channel)
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

	evidence := runScenarioA(t, ctx, turnService, taskStore, fixtureMedia{}, fixtureTTS{}, scope, false, func(commandID string) {
		injectVehicleReceipt(t, ctx, taskStore, worker, channel, scope, commandID)
	})
	audit.assertScenario(t, evidence.taskID)

	command, err := taskStore.GetCommand(ctx, evidence.commandID)
	if err != nil || command.Scope != scope || command.Params["position"] != "rear_left" || command.Status != contract.Completed ||
		command.Attempts != 1 || channel.SendCount(evidence.commandID) != 1 || command.TaskID != evidence.taskID {
		t.Fatalf("wrong SQLite confirmed command: %+v, %v", command, err)
	}
	results, err := taskStore.ListVehicleResults(ctx, scope, time.Now().Add(-time.Minute))
	if err != nil || len(results) != 1 || results[0].EventID != evidence.result.EventID {
		t.Fatalf("expected 1 SQLite result event: %+v, %v", results, err)
	}
}
