package integration

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/kirin7002/app_personal_agent/contract"
	"github.com/kirin7002/app_personal_agent/vehicle/catalog"
	"github.com/kirin7002/app_personal_agent/vehicle/tasks"
	"github.com/kirin7002/app_personal_agent/vehicle/turns"
)

func integrationDB(t *testing.T, ctx context.Context) *sql.DB {
	t.Helper()
	dsn := os.Getenv("H07_MYSQL_DSN")
	cfg, err := mysql.ParseDSN(dsn)
	if dsn == "" || err != nil || cfg.DBName == "" {
		t.Fatal("set H07_MYSQL_DSN to a disposable MySQL database")
	}
	cfg.ParseTime = true
	cfg.Loc = time.UTC
	cfg.Timeout = 3 * time.Second
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		t.Fatal("invalid MySQL connector configuration")
	}
	db := sql.OpenDB(connector)
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(8)
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("MySQL unavailable: %v", err)
	}
	statements := append([]string{turns.MySQLSchema}, tasks.EnsureSQLSchemaStatements()...)
	for i, migration := range statements {
		if _, err := db.ExecContext(ctx, migration); err != nil {
			t.Fatalf("schema migration %d: %v", i, err)
		}
	}
	return db
}

func TestMySQLTurnCancelFence(t *testing.T) {
	if os.Getenv("H07_MYSQL_DSN") == "" {
		t.Skip("set H07_MYSQL_DSN to run MySQL integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db := integrationDB(t, ctx)
	store := &turns.MySQLStore{DB: db}
	scope := contract.Scope{UserID: "h07-user", SessionID: rand.Text(), VehicleID: "h07-car"}
	metadata := contract.TurnMetadata{TurnHandle: rand.Text(), HermesSessionID: rand.Text()}
	if err := store.Register(ctx, turns.Record{Context: contract.TurnContext{
		VehicleTurn: contract.VehicleTurn{Scope: scope, VoiceTurnID: "turn-1", RequestID: "request-1"},
		TurnHandle:  metadata.TurnHandle, HermesSessionID: metadata.HermesSessionID, ExpiresAt: time.Now().Add(time.Minute),
	}}); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	writable, err := turns.LockWritableTurn(ctx, tx, metadata, time.Now())
	if err != nil || writable.Scope != scope {
		t.Fatalf("pre-registered context missing: %+v, %v", writable.Scope, err)
	}
	finished := make(chan error, 1)
	go func() { finished <- store.Cancel(ctx, scope, "turn-1") }()
	timer := time.NewTimer(120 * time.Millisecond)
	defer timer.Stop()
	select {
	case err := <-finished:
		t.Fatalf("cancel bypassed transaction fence: %v", err)
	case <-timer.C:
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	_, err = store.ResolveTurn(ctx, metadata, time.Now())
	var invalid *contract.BusinessError
	if !errors.As(err, &invalid) || invalid.Code != contract.TurnContextInvalid {
		t.Fatalf("cancelled handle remained writable: %v", err)
	}
}

func TestMySQLPendingCandidateBoundary(t *testing.T) {
	if os.Getenv("H07_MYSQL_DSN") == "" {
		t.Skip("set H07_MYSQL_DSN to run MySQL integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db := integrationDB(t, ctx)
	store := &turns.MySQLStore{DB: db}
	taskStore := &tasks.MySQLStore{DB: db, Validator: tasks.NewCatalogValidator(catalog.NewService(nil))}
	scope := contract.Scope{UserID: "h07-user", SessionID: rand.Text(), VehicleID: "h07-car"}
	createdAt := time.Now().UTC().Truncate(time.Second)
	operation := contract.Operation{Device: "window", Tool: "close"}
	var taskIDs []string
	for i := range 4 {
		metadata := contract.TurnMetadata{TurnHandle: rand.Text(), HermesSessionID: scope.SessionID}
		now := createdAt.Add(time.Duration(i) * time.Second)
		turn := contract.TurnContext{
			VehicleTurn: contract.VehicleTurn{Scope: scope, VoiceTurnID: rand.Text(), RequestID: rand.Text()},
			TurnHandle:  metadata.TurnHandle, HermesSessionID: metadata.HermesSessionID, ExpiresAt: now.Add(time.Minute),
		}
		if err := store.Register(ctx, turns.Record{Context: turn}); err != nil {
			t.Fatal(err)
		}
		req := tasks.TaskWriteRequest{
			Metadata: metadata, Mutation: contract.TaskMutation{Operation: operation, Params: map[string]any{}}, Now: now,
		}
		result, err := taskStore.CreateOrUpdateTask(ctx, req)
		if err != nil || result.ExpiresAt == nil || !result.ExpiresAt.Equal(now.Add(contract.PendingTTL)) {
			t.Fatalf("invalid pending task: %+v %v", result, err)
		}
		replay, err := taskStore.CreateOrUpdateTask(ctx, req)
		if err != nil || result.TaskID != replay.TaskID {
			t.Fatalf("identical write replay created another task: %+v %v", replay, err)
		}
		taskIDs = append(taskIDs, result.TaskID)
	}
	candidates, err := taskStore.ListPending(ctx, scope, createdAt.Add(4*time.Second))
	if err != nil || len(candidates) != contract.PendingLimit {
		t.Fatalf("pending candidate limit: %d %v", len(candidates), err)
	}
	for i, candidate := range candidates {
		if candidate.TaskID != taskIDs[3-i] || candidate.Version == 0 {
			t.Fatalf("candidate order or version mismatch: %+v", candidate)
		}
	}
	deadline := createdAt.Add(contract.PendingTTL)
	if _, err := taskStore.ExpirePending(ctx, deadline); err != nil {
		t.Fatal(err)
	}
	expired, err := taskStore.GetTask(ctx, scope, taskIDs[0])
	if err != nil || expired.Status != contract.Expired {
		t.Fatalf("task remained active at its deadline: %+v %v", expired, err)
	}
	pending, err := taskStore.GetTask(ctx, scope, taskIDs[1])
	if err != nil || pending.Status != contract.PendingReply {
		t.Fatalf("later task expired before its deadline: %+v %v", pending, err)
	}
}
