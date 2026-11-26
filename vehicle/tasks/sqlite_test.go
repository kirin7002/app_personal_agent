package tasks

import (
	"context"
	"crypto/rand"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kirin7002/app_personal_agent/contract"
	"github.com/kirin7002/app_personal_agent/vehicle/catalog"
	"github.com/kirin7002/app_personal_agent/vehicle/turns"
)

func openTestSQLite(t *testing.T) (string, *turns.SQLiteStore, *SQLiteStore) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "vehicle.sqlite")
	db, err := OpenSQLite(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cat := catalog.NewService(nil)
	return dbPath, turns.NewSQLiteStore(db), NewSQLiteStore(db, NewCatalogValidator(cat))
}

func TestSQLiteTurnCancelFence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, turnStore, _ := openTestSQLite(t)

	scope := contract.Scope{UserID: "u-sqlite", SessionID: rand.Text(), VehicleID: "car-1"}
	meta := contract.TurnMetadata{TurnHandle: rand.Text(), HermesSessionID: rand.Text()}
	if err := turnStore.Register(ctx, turns.Record{Context: contract.TurnContext{
		VehicleTurn:     contract.VehicleTurn{Scope: scope, VoiceTurnID: "turn-1", RequestID: "req-1"},
		TurnHandle:      meta.TurnHandle,
		HermesSessionID: meta.HermesSessionID,
		ExpiresAt:       time.Now().UTC().Add(time.Minute),
	}}); err != nil {
		t.Fatal(err)
	}

	tx, err := turnStore.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	writable, err := turnStore.LockWritableTurn(ctx, tx, meta, time.Now().UTC())
	if err != nil || writable.Scope != scope {
		t.Fatalf("LockWritableTurn failed: %+v, %v", writable, err)
	}

	// Cancel MUST block while the BEGIN IMMEDIATE write transaction is open.
	finished := make(chan error, 1)
	go func() { finished <- turnStore.Cancel(ctx, scope, "turn-1") }()

	timer := time.NewTimer(120 * time.Millisecond)
	defer timer.Stop()
	select {
	case err := <-finished:
		t.Fatalf("cancel bypassed SQLite write transaction fence: %v", err)
	case <-timer.C:
	}

	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}

	_, err = turnStore.ResolveTurn(ctx, meta, time.Now().UTC())
	var invalid *contract.BusinessError
	if !errors.As(err, &invalid) || invalid.Code != contract.TurnContextInvalid {
		t.Fatalf("cancelled handle remained writable: %v", err)
	}
}

func TestSQLitePendingCandidateBoundaryAndExpiry(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, turnStore, taskStore := openTestSQLite(t)

	scope := contract.Scope{UserID: "u-sqlite", SessionID: rand.Text(), VehicleID: "car-1"}
	createdAt := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	op := contract.Operation{Device: "window", Tool: "close"}

	// Unwritten turn -> ErrTurnHasNoWrite.
	if _, err := taskStore.ReadTurnProgress(ctx, scope, "no-such-req"); !errors.Is(err, contract.ErrTurnHasNoWrite) {
		t.Fatalf("expected ErrTurnHasNoWrite, got %v", err)
	}

	var taskIDs []string
	var firstReqID string
	for i := range 4 {
		meta := contract.TurnMetadata{TurnHandle: rand.Text(), HermesSessionID: "hs-" + scope.SessionID}
		now := createdAt.Add(time.Duration(i) * time.Second)
		reqID := rand.Text()
		if i == 0 {
			firstReqID = reqID
		}
		turn := contract.TurnContext{
			VehicleTurn:     contract.VehicleTurn{Scope: scope, VoiceTurnID: rand.Text(), RequestID: reqID},
			TurnHandle:      meta.TurnHandle,
			HermesSessionID: meta.HermesSessionID,
			ExpiresAt:       now.Add(5 * time.Minute),
		}
		if err := turnStore.Register(ctx, turns.Record{Context: turn}); err != nil {
			t.Fatal(err)
		}
		req := TaskWriteRequest{
			Metadata: meta,
			Mutation: contract.TaskMutation{Operation: op, Params: map[string]any{}},
			Now:      now,
		}
		res, err := taskStore.CreateOrUpdateTask(ctx, req)
		if err != nil || res.ExpiresAt == nil || !res.ExpiresAt.Equal(now.Add(contract.PendingTTL)) {
			t.Fatalf("create pending task failed: %+v, %v", res, err)
		}
		// Exact replay returns original TaskID.
		replay, err := taskStore.CreateOrUpdateTask(ctx, req)
		if err != nil || replay.TaskID != res.TaskID {
			t.Fatalf("replay created duplicate task: first=%+v replay=%+v err=%v", res, replay, err)
		}
		// Different payload in same turn returns IDEMPOTENCY_CONFLICT.
		conflictReq := req
		conflictReq.Mutation = contract.TaskMutation{
			Operation: contract.Operation{Device: "hvac", Tool: "set_climate"},
			Params:    map[string]any{"power": "on"},
		}
		_, err = taskStore.CreateOrUpdateTask(ctx, conflictReq)
		var be *contract.BusinessError
		if !errors.As(err, &be) || be.Code != contract.IdempotencyConflict {
			t.Fatalf("expected IDEMPOTENCY_CONFLICT, got %v", err)
		}
		taskIDs = append(taskIDs, res.TaskID)
	}

	// 4 tasks -> ListPending returns top 3 ordered created_at DESC, task_id DESC.
	candidates, err := taskStore.ListPending(ctx, scope, createdAt.Add(4*time.Second))
	if err != nil || len(candidates) != contract.PendingLimit {
		t.Fatalf("expected %d candidates, got %d (%v)", contract.PendingLimit, len(candidates), err)
	}
	for i, c := range candidates {
		if c.TaskID != taskIDs[3-i] || c.Version != 1 || len(c.ParamsSchema) == 0 {
			t.Fatalf("candidate[%d] mismatch: %+v", i, c)
		}
	}

	// Exact 10m boundary WITHOUT running scanner (scan lag): dispatch must still reject TASK_EXPIRED.
	deadline := createdAt.Add(contract.PendingTTL)
	expMeta := contract.TurnMetadata{TurnHandle: rand.Text(), HermesSessionID: "hs-" + scope.SessionID}
	if err := turnStore.Register(ctx, turns.Record{Context: contract.TurnContext{
		VehicleTurn:     contract.VehicleTurn{Scope: scope, VoiceTurnID: rand.Text(), RequestID: rand.Text()},
		TurnHandle:      expMeta.TurnHandle,
		HermesSessionID: expMeta.HermesSessionID,
		ExpiresAt:       deadline.Add(time.Minute),
	}}); err != nil {
		t.Fatal(err)
	}
	_, err = taskStore.DispatchCommand(ctx, TaskWriteRequest{
		Metadata:        expMeta,
		Mutation:        contract.TaskMutation{Operation: op, TaskID: taskIDs[0], Params: map[string]any{"position": "rear_left"}},
		ExpectedVersion: 1,
		Now:             deadline,
	})
	var be *contract.BusinessError
	if !errors.As(err, &be) || be.Code != contract.TaskExpired {
		t.Fatalf("expected TASK_EXPIRED at exact 10m boundary, got %v", err)
	}

	// Expiry scanner marks task 0 expired while leaving task 1 pending_reply.
	n, err := taskStore.ExpirePending(ctx, deadline)
	if err != nil || n != 1 {
		t.Fatalf("expected 1 expired task, got %d (%v)", n, err)
	}
	expired, err := taskStore.GetTask(ctx, scope, taskIDs[0])
	if err != nil || expired.Status != contract.Expired {
		t.Fatalf("expected task 0 expired, got %+v (%v)", expired, err)
	}
	prog, err := taskStore.ReadTurnProgress(ctx, scope, firstReqID)
	if err != nil || prog.TaskID != taskIDs[0] || prog.Status != contract.Expired {
		t.Fatalf("expected ProgressReader to reflect expired status, got %+v (%v)", prog, err)
	}
}

func TestSQLitePartialCompletionAndConcurrentResumeRace(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, turnStore, taskStore := openTestSQLite(t)

	scope := contract.Scope{UserID: "u-race", SessionID: rand.Text(), VehicleID: "car-1"}
	t0 := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)

	register := func(now time.Time) contract.TurnMetadata {
		meta := contract.TurnMetadata{TurnHandle: rand.Text(), HermesSessionID: "hs-" + scope.SessionID}
		if err := turnStore.Register(ctx, turns.Record{Context: contract.TurnContext{
			VehicleTurn:     contract.VehicleTurn{Scope: scope, VoiceTurnID: rand.Text(), RequestID: rand.Text()},
			TurnHandle:      meta.TurnHandle,
			HermesSessionID: meta.HermesSessionID,
			ExpiresAt:       now.Add(5 * time.Minute),
		}}); err != nil {
			t.Fatal(err)
		}
		return meta
	}

	// Create hvac.set_climate missing both power & temperature_c, then partial fill power=on.
	c1, err := taskStore.CreateOrUpdateTask(ctx, TaskWriteRequest{
		Metadata: register(t0),
		Mutation: contract.TaskMutation{Operation: contract.Operation{Device: "hvac", Tool: "set_climate"}, Params: map[string]any{}},
		Now:      t0,
	})
	if err != nil || len(c1.MissingFields) != 2 {
		t.Fatalf("create failed: %+v (%v)", c1, err)
	}
	c2, err := taskStore.CreateOrUpdateTask(ctx, TaskWriteRequest{
		Metadata:        register(t0.Add(time.Minute)),
		Mutation:        contract.TaskMutation{Operation: contract.Operation{Device: "hvac", Tool: "set_climate"}, TaskID: c1.TaskID, Params: map[string]any{"power": "on"}},
		ExpectedVersion: 1,
		Now:             t0.Add(time.Minute),
	})
	if err != nil || !c2.ExpiresAt.Equal(*c1.ExpiresAt) {
		t.Fatalf("partial fill must preserve expires_at: %+v (%v)", c2, err)
	}

	// Two concurrent goroutines race to dispatch temperature_c=25 -> at most 1 command!
	metaA, metaB := register(t0.Add(2*time.Minute)), register(t0.Add(2*time.Minute))
	var wg sync.WaitGroup
	var mu sync.Mutex
	var succeeded []contract.TaskResult
	for _, meta := range []contract.TurnMetadata{metaA, metaB} {
		wg.Add(1)
		go func(m contract.TurnMetadata) {
			defer wg.Done()
			res, err := taskStore.DispatchCommand(ctx, TaskWriteRequest{
				Metadata:        m,
				Mutation:        contract.TaskMutation{Operation: contract.Operation{Device: "hvac", Tool: "set_climate"}, TaskID: c1.TaskID, Params: map[string]any{"temperature_c": 25}},
				ExpectedVersion: 2,
				Now:             t0.Add(2 * time.Minute),
			})
			if err == nil {
				mu.Lock()
				succeeded = append(succeeded, res)
				mu.Unlock()
			}
		}(meta)
	}
	wg.Wait()
	if len(succeeded) != 1 {
		t.Fatalf("expected exactly 1 winner in concurrent SQLite resume race, got %+v", succeeded)
	}
	cmd, err := taskStore.GetCommand(ctx, succeeded[0].CommandID)
	if err != nil || cmd.Params["power"] != "on" || cmd.Params["temperature_c"] != float64(25) {
		t.Fatalf("unexpected merged command params: %+v (%v)", cmd, err)
	}
}

func TestSQLiteRestartPersistenceAndOutboxFlow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dbPath := filepath.Join(t.TempDir(), "persist.sqlite")
	scope := contract.Scope{UserID: "u-restart", SessionID: "S1", VehicleID: "VCL1"}
	t0 := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)

	// Process 1: register turn and dispatch a direct command, then close DB (simulate restart).
	db1, err := OpenSQLite(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	cat := catalog.NewService(nil)
	turns1 := turns.NewSQLiteStore(db1)
	tasks1 := NewSQLiteStore(db1, NewCatalogValidator(cat))
	meta := contract.TurnMetadata{TurnHandle: "TH-PERSIST", HermesSessionID: "HS-S1"}
	if err := turns1.Register(ctx, turns.Record{Context: contract.TurnContext{
		VehicleTurn:     contract.VehicleTurn{Scope: scope, VoiceTurnID: "V1", RequestID: "R1"},
		TurnHandle:      meta.TurnHandle,
		HermesSessionID: meta.HermesSessionID,
		ExpiresAt:       t0.Add(5 * time.Minute),
	}}); err != nil {
		t.Fatal(err)
	}
	disp, err := tasks1.DispatchCommand(ctx, TaskWriteRequest{
		Metadata: meta,
		Mutation: contract.TaskMutation{
			Operation: contract.Operation{Device: "window", Tool: "close"},
			Params:    map[string]any{"position": "rear_left"},
		},
		Now: t0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db1.Close(); err != nil {
		t.Fatal(err)
	}

	// Process 2: reopen the SAME SQLite file, claim outbox command, complete it, and verify deduplication!
	db2, err := OpenSQLite(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db2.Close() }()
	tasks2 := NewSQLiteStore(db2, NewCatalogValidator(cat))
	tasks2.Now = func() time.Time { return t0.Add(time.Minute) }

	claimed, err := tasks2.ClaimNextCommand(ctx, "worker-2", 15*time.Second, t0.Add(5*time.Second))
	if err != nil || claimed == nil || claimed.CommandID != disp.CommandID {
		t.Fatalf("expected claimed command %s after restart, got %+v (%v)", disp.CommandID, claimed, err)
	}
	if err := tasks2.MarkCommandExecuting(ctx, claimed.CommandID, "worker-2", t0.Add(6*time.Second), "VEH-99"); err != nil {
		t.Fatal(err)
	}
	applied1, ev1, err := tasks2.TransitionCommandOutcome(ctx, claimed.CommandID, contract.Completed, "ok", t0.Add(8*time.Second))
	if err != nil || !applied1 || ev1 == nil || ev1.EventID != claimed.CommandID+":completed" {
		t.Fatalf("first completion transition failed: applied=%v ev=%+v err=%v", applied1, ev1, err)
	}
	// Duplicate completion is ignored monotonically.
	applied2, _, err := tasks2.TransitionCommandOutcome(ctx, claimed.CommandID, contract.Completed, "dup", t0.Add(9*time.Second))
	if err != nil || applied2 {
		t.Fatalf("duplicate completion must not re-apply: applied=%v err=%v", applied2, err)
	}
	events, err := tasks2.ListVehicleResults(ctx, scope, t0)
	if err != nil || len(events) != 1 || events[0].EventID != claimed.CommandID+":completed" {
		t.Fatalf("expected 1 retained result event after restart, got %+v (%v)", events, err)
	}
}
