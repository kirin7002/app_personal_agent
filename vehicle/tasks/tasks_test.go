package tasks

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kirin7002/app_personal_agent/contract"
	"github.com/kirin7002/app_personal_agent/vehicle/catalog"
)

func testTurn(scope contract.Scope, reqID string, candidates ...contract.CandidateVersion) contract.TurnContext {
	return contract.TurnContext{
		VehicleTurn: contract.VehicleTurn{
			Scope:       scope,
			VoiceTurnID: "V-" + reqID,
			RequestID:   reqID,
		},
		HermesSessionID: "HS-" + scope.SessionID,
		TurnHandle:      "TH-" + reqID,
		Candidates:      candidates,
		ExpiresAt:       time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC),
	}
}

func TestExactTenMinuteExpiryAndScanLagGuard(t *testing.T) {
	ctx := context.Background()
	cat := catalog.NewService(nil)
	store := NewMemoryStore(NewCatalogValidator(cat))
	scope := contract.Scope{UserID: "U1", SessionID: "S1", VehicleID: "VCL1"}
	t0 := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)

	created, err := store.CreateOrUpdateTask(ctx, TaskWriteRequest{
		Turn:     testTurn(scope, "R1"),
		Mutation: contract.TaskMutation{Operation: contract.Operation{Device: "window", Tool: "close"}, Params: map[string]any{}},
		Now:      t0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.ExpiresAt == nil || !created.ExpiresAt.Equal(t0.Add(10*time.Minute)) {
		t.Fatalf("expected expires_at = created_at + 10m, got %+v", created)
	}

	// At 9m59s: still pending and eligible.
	at9m59 := t0.Add(10*time.Minute - time.Second)
	pending, err := store.ListPending(ctx, scope, at9m59)
	if err != nil || len(pending) != 1 {
		t.Fatalf("expected 1 pending at 9m59s, got %+v, %v", pending, err)
	}

	// At exact 10m00s WITHOUT running expiry scanner (simulating scan lag):
	// ListPending must exclude it AND DispatchCommand must reject with TASK_EXPIRED!
	at10m := t0.Add(10 * time.Minute)
	pendingAt10m, err := store.ListPending(ctx, scope, at10m)
	if err != nil || len(pendingAt10m) != 0 {
		t.Fatalf("exact 10m boundary must exclude pending task, got %+v, %v", pendingAt10m, err)
	}
	_, err = store.DispatchCommand(ctx, TaskWriteRequest{
		Turn: testTurn(scope, "R2", contract.CandidateVersion{TaskID: created.TaskID, Version: 1}),
		Mutation: contract.TaskMutation{
			Operation: contract.Operation{Device: "window", Tool: "close"},
			TaskID:    created.TaskID,
			Params:    map[string]any{"position": "rear_left"},
		},
		ExpectedVersion: 1,
		Now:             at10m,
	})
	var be *contract.BusinessError
	if !errors.As(err, &be) || be.Code != contract.TaskExpired {
		t.Fatalf("expected TASK_EXPIRED at exact 10m boundary, got %v", err)
	}
}

func TestFourTasksLimitThreeAndStableOrdering(t *testing.T) {
	ctx := context.Background()
	cat := catalog.NewService(nil)
	store := NewMemoryStore(NewCatalogValidator(cat))
	scope := contract.Scope{UserID: "U1", SessionID: "S1", VehicleID: "VCL1"}
	base := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)

	// Create 4 tasks; tasks 3 & 4 share the exact same timestamp to test task_id DESC tie-breaker.
	times := []time.Time{base, base.Add(time.Minute), base.Add(2 * time.Minute), base.Add(2 * time.Minute)}
	var ids []string
	for i, ts := range times {
		res, err := store.CreateOrUpdateTask(ctx, TaskWriteRequest{
			Turn:     testTurn(scope, "R"+string(rune('1'+i))),
			Mutation: contract.TaskMutation{Operation: contract.Operation{Device: "window", Tool: "close"}, Params: map[string]any{}},
			Now:      ts,
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, res.TaskID)
	}

	got, err := store.ListPending(ctx, scope, base.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != contract.PendingLimit {
		t.Fatalf("expected %d candidates, got %d", contract.PendingLimit, len(got))
	}
	// Expected order: ids[3], ids[2], ids[1] (ids[0] excluded).
	want := []string{ids[3], ids[2], ids[1]}
	for i := range want {
		if got[i].TaskID != want[i] {
			t.Fatalf("candidate[%d] got %s, want %s (all=%+v)", i, got[i].TaskID, want[i], got)
		}
		if len(got[i].ParamsSchema) == 0 {
			t.Fatalf("expected ParamsSchema attached to candidate %s", got[i].TaskID)
		}
	}
}

func TestConcurrentResumeRaceAndExpiryRaceAtMostOneCommand(t *testing.T) {
	ctx := context.Background()
	cat := catalog.NewService(nil)
	scope := contract.Scope{UserID: "U1", SessionID: "S1", VehicleID: "VCL1"}
	t0 := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)

	// 1. Two concurrent replies racing for the same pending_reply task -> at most 1 command.
	store := NewMemoryStore(NewCatalogValidator(cat))
	created, err := store.CreateOrUpdateTask(ctx, TaskWriteRequest{
		Turn:     testTurn(scope, "R1"),
		Mutation: contract.TaskMutation{Operation: contract.Operation{Device: "window", Tool: "close"}, Params: map[string]any{}},
		Now:      t0,
	})
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	successes := 0
	var cmdIDs []string
	for _, reqID := range []string{"R2", "R3"} {
		wg.Add(1)
		go func(rid string) {
			defer wg.Done()
			res, err := store.DispatchCommand(ctx, TaskWriteRequest{
				Turn: testTurn(scope, rid, contract.CandidateVersion{TaskID: created.TaskID, Version: 1}),
				Mutation: contract.TaskMutation{
					Operation: contract.Operation{Device: "window", Tool: "close"},
					TaskID:    created.TaskID,
					Params:    map[string]any{"position": "rear_left"},
				},
				ExpectedVersion: 1,
				Now:             t0.Add(time.Minute),
			})
			if err == nil {
				mu.Lock()
				successes++
				cmdIDs = append(cmdIDs, res.CommandID)
				mu.Unlock()
			}
		}(reqID)
	}
	wg.Wait()
	if successes != 1 || len(cmdIDs) != 1 || len(store.commands) != 1 {
		t.Fatalf("expected exactly 1 command from concurrent resume race, got successes=%d cmds=%d", successes, len(store.commands))
	}

	// 2. Expiry scan racing with dispatch -> task ends either expired (0 cmds) or pending_dispatch (1 cmd), never inconsistent.
	store2 := NewMemoryStore(NewCatalogValidator(cat))
	c2, err := store2.CreateOrUpdateTask(ctx, TaskWriteRequest{
		Turn:     testTurn(scope, "E1"),
		Mutation: contract.TaskMutation{Operation: contract.Operation{Device: "window", Tool: "close"}, Params: map[string]any{}},
		Now:      t0,
	})
	if err != nil {
		t.Fatal(err)
	}
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = store2.ExpirePending(ctx, t0.Add(10*time.Minute))
	}()
	go func() {
		defer wg.Done()
		_, _ = store2.DispatchCommand(ctx, TaskWriteRequest{
			Turn: testTurn(scope, "E2", contract.CandidateVersion{TaskID: c2.TaskID, Version: 1}),
			Mutation: contract.TaskMutation{
				Operation: contract.Operation{Device: "window", Tool: "close"},
				TaskID:    c2.TaskID,
				Params:    map[string]any{"position": "rear_left"},
			},
			ExpectedVersion: 1,
			Now:             t0.Add(9*time.Minute + 59*time.Second),
		})
	}()
	wg.Wait()
	taskFinal, err := store2.GetTask(ctx, scope, c2.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if taskFinal.Status == contract.Expired && len(store2.commands) != 0 {
		t.Fatalf("expired task must have 0 commands, got %d", len(store2.commands))
	}
	if taskFinal.Status == contract.PendingDispatch && len(store2.commands) != 1 {
		t.Fatalf("dispatched task must have 1 command, got %d", len(store2.commands))
	}
}

func TestPartialCompletionPreservesDeadlineAndRejectsConflicts(t *testing.T) {
	ctx := context.Background()
	cat := catalog.NewService(nil)
	store := NewMemoryStore(NewCatalogValidator(cat))
	scope := contract.Scope{UserID: "U1", SessionID: "S1", VehicleID: "VCL1"}
	t0 := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)

	// Step 1: hvac.set_climate missing BOTH power and temperature_c.
	c1, err := store.CreateOrUpdateTask(ctx, TaskWriteRequest{
		Turn:     testTurn(scope, "P1"),
		Mutation: contract.TaskMutation{Operation: contract.Operation{Device: "hvac", Tool: "set_climate"}, Params: map[string]any{}},
		Now:      t0,
	})
	if err != nil || len(c1.MissingFields) != 2 {
		t.Fatalf("expected 2 missing fields, got %+v, %v", c1, err)
	}

	// Step 2: supply power=on at t0+2m -> stays pending_reply, preserves t0+10m expires_at, bumps version to 2.
	c2, err := store.CreateOrUpdateTask(ctx, TaskWriteRequest{
		Turn: testTurn(scope, "P2", contract.CandidateVersion{TaskID: c1.TaskID, Version: 1}),
		Mutation: contract.TaskMutation{
			Operation: contract.Operation{Device: "hvac", Tool: "set_climate"},
			TaskID:    c1.TaskID,
			Params:    map[string]any{"power": "on"},
		},
		ExpectedVersion: 1,
		Now:             t0.Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !c2.ExpiresAt.Equal(*c1.ExpiresAt) || len(c2.MissingFields) != 1 || c2.MissingFields[0] != "temperature_c" {
		t.Fatalf("expected preserved deadline & missing=[temperature_c], got %+v", c2)
	}

	// Stale version snapshot (version 1) rejected with TASK_CONFLICT.
	_, err = store.DispatchCommand(ctx, TaskWriteRequest{
		Turn: testTurn(scope, "P3_stale", contract.CandidateVersion{TaskID: c1.TaskID, Version: 1}),
		Mutation: contract.TaskMutation{
			Operation: contract.Operation{Device: "hvac", Tool: "set_climate"},
			TaskID:    c1.TaskID,
			Params:    map[string]any{"temperature_c": 25},
		},
		ExpectedVersion: 1,
		Now:             t0.Add(3 * time.Minute),
	})
	var be *contract.BusinessError
	if !errors.As(err, &be) || be.Code != contract.TaskConflict {
		t.Fatalf("expected TASK_CONFLICT for stale version, got %v", err)
	}

	// Known-field conflict (power=off vs known power=on) rejected with TASK_CONFLICT.
	_, err = store.DispatchCommand(ctx, TaskWriteRequest{
		Turn: testTurn(scope, "P3_conflict", contract.CandidateVersion{TaskID: c1.TaskID, Version: 2}),
		Mutation: contract.TaskMutation{
			Operation: contract.Operation{Device: "hvac", Tool: "set_climate"},
			TaskID:    c1.TaskID,
			Params:    map[string]any{"power": "off", "temperature_c": 25},
		},
		ExpectedVersion: 2,
		Now:             t0.Add(3 * time.Minute),
	})
	if !errors.As(err, &be) || be.Code != contract.TaskConflict {
		t.Fatalf("expected TASK_CONFLICT for known-param overwrite, got %v", err)
	}

	// Valid completion with version=2 and temperature_c=25 inherits known power=on and dispatches!
	disp, err := store.DispatchCommand(ctx, TaskWriteRequest{
		Turn: testTurn(scope, "P3_ok", contract.CandidateVersion{TaskID: c1.TaskID, Version: 2}),
		Mutation: contract.TaskMutation{
			Operation: contract.Operation{Device: "hvac", Tool: "set_climate"},
			TaskID:    c1.TaskID,
			Params:    map[string]any{"temperature_c": 25},
		},
		ExpectedVersion: 2,
		Now:             t0.Add(4 * time.Minute),
	})
	if err != nil || disp.TaskID != c1.TaskID || disp.Status != contract.PendingDispatch {
		t.Fatalf("expected dispatch success, got %+v, %v", disp, err)
	}
	cmd, err := store.GetCommand(ctx, disp.CommandID)
	if err != nil || cmd.Params["power"] != "on" || cmd.Params["temperature_c"] != float64(25) {
		t.Fatalf("expected merged params {power:on, temperature_c:25}, got %+v, %v", cmd, err)
	}
}

func TestReplayIdempotencyRollbackAtomicityAndProgressReader(t *testing.T) {
	ctx := context.Background()
	cat := catalog.NewService(nil)
	store := NewMemoryStore(NewCatalogValidator(cat))
	scope := contract.Scope{UserID: "U1", SessionID: "S1", VehicleID: "VCL1"}
	t0 := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)

	// Unwritten turn -> ProgressReader returns ErrTurnHasNoWrite.
	if _, err := store.ReadTurnProgress(ctx, scope, "R_NONE"); !errors.Is(err, contract.ErrTurnHasNoWrite) {
		t.Fatalf("expected ErrTurnHasNoWrite, got %v", err)
	}

	// Simulated mid-tx failure leaves ZERO orphan tasks or commands.
	store.FailOutboxInsertOnce = true
	_, err := store.DispatchCommand(ctx, TaskWriteRequest{
		Turn: testTurn(scope, "R_FAIL"),
		Mutation: contract.TaskMutation{
			Operation: contract.Operation{Device: "window", Tool: "close"},
			Params:    map[string]any{"position": "rear_left"},
		},
		Now: t0,
	})
	if err == nil || len(store.tasks) != 0 || len(store.commands) != 0 {
		t.Fatalf("failed tx must leave zero orphans, err=%v tasks=%d cmds=%d", err, len(store.tasks), len(store.commands))
	}

	// Exact replay returns original task_id and command_id; differing payload raises IDEMPOTENCY_CONFLICT.
	req := TaskWriteRequest{
		Turn: testTurn(scope, "R_DIRECT"),
		Mutation: contract.TaskMutation{
			Operation: contract.Operation{Device: "window", Tool: "close"},
			Params:    map[string]any{"position": "rear_left"},
		},
		Now: t0,
	}
	first, err := store.DispatchCommand(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := store.DispatchCommand(ctx, req)
	if err != nil || replay.TaskID != first.TaskID || replay.CommandID != first.CommandID {
		t.Fatalf("expected identical replay result, got first=%+v replay=%+v err=%v", first, replay, err)
	}
	conflictReq := req
	conflictReq.Mutation.Params = map[string]any{"position": "front_left"}
	_, err = store.DispatchCommand(ctx, conflictReq)
	var be *contract.BusinessError
	if !errors.As(err, &be) || be.Code != contract.IdempotencyConflict {
		t.Fatalf("expected IDEMPOTENCY_CONFLICT, got %v", err)
	}

	// ProgressReader reflects current persisted state for R_DIRECT.
	prog, err := store.ReadTurnProgress(ctx, scope, "R_DIRECT")
	if err != nil || prog.TaskID != first.TaskID || prog.CommandID != first.CommandID || prog.Status != contract.PendingDispatch {
		t.Fatalf("unexpected progress: %+v, %v", prog, err)
	}
}

// --- Lightweight SQL driver double to verify MySQLStore transactional rollback & LockWritableTurn wiring ---

type mockSQLDriver struct {
	mu          sync.Mutex
	failOnQuery string
	queries     []string
	committed   int
	rolledBack  int
}

func (d *mockSQLDriver) Open(string) (driver.Conn, error) { return &mockSQLConn{drv: d}, nil }

type mockSQLConn struct{ drv *mockSQLDriver }

func (c *mockSQLConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unused") }
func (c *mockSQLConn) Close() error                        { return nil }
func (c *mockSQLConn) Begin() (driver.Tx, error)           { return &mockSQLTx{drv: c.drv}, nil }
func (c *mockSQLConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return &mockSQLTx{drv: c.drv}, nil
}

func (c *mockSQLConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	c.drv.mu.Lock()
	defer c.drv.mu.Unlock()
	c.drv.queries = append(c.drv.queries, strings.TrimSpace(query))
	if c.drv.failOnQuery != "" && strings.Contains(query, c.drv.failOnQuery) {
		return nil, errors.New("injected sql failure")
	}
	return driver.RowsAffected(1), nil
}

func (c *mockSQLConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	c.drv.mu.Lock()
	defer c.drv.mu.Unlock()
	c.drv.queries = append(c.drv.queries, strings.TrimSpace(query))
	return &emptyRows{}, nil
}

type mockSQLTx struct{ drv *mockSQLDriver }

func (t *mockSQLTx) Commit() error {
	t.drv.mu.Lock()
	t.drv.committed++
	t.drv.mu.Unlock()
	return nil
}
func (t *mockSQLTx) Rollback() error {
	t.drv.mu.Lock()
	t.drv.rolledBack++
	t.drv.mu.Unlock()
	return nil
}

type emptyRows struct{}

func (e *emptyRows) Columns() []string              { return []string{"a", "b"} }
func (e *emptyRows) Close() error                   { return nil }
func (e *emptyRows) Next([]driver.Value) error      { return io.EOF }

func TestMySQLStoreRollbackAndTurnLockInSingleTx(t *testing.T) {
	// Ensure embedded SQL DDL parses cleanly into 4 CREATE TABLE statements.
	stmts := EnsureSQLSchemaStatements()
	if len(stmts) != 4 {
		t.Fatalf("expected 4 DDL statements in tasks.sql, got %d", len(stmts))
	}

	drv := &mockSQLDriver{failOnQuery: "INSERT INTO vehicle_command_outbox"}
	sql.Register("h03_mock_mysql", drv)
	db, err := sql.Open("h03_mock_mysql", "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	cat := catalog.NewService(nil)
	store := &MySQLStore{DB: db, Validator: NewCatalogValidator(cat)}
	scope := contract.Scope{UserID: "U1", SessionID: "S1", VehicleID: "VCL1"}
	lockCalled := false

	_, err = store.DispatchCommand(context.Background(), TaskWriteRequest{
		Turn: testTurn(scope, "SQL_R1"),
		Mutation: contract.TaskMutation{
			Operation: contract.Operation{Device: "window", Tool: "close"},
			Params:    map[string]any{"position": "rear_left"},
		},
		Now: time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC),
		TxLock: func(_ context.Context, tx *sql.Tx, _ time.Time) (contract.TurnContext, error) {
			if tx == nil {
				t.Error("TxLock must receive active *sql.Tx")
			}
			lockCalled = true
			return testTurn(scope, "SQL_R1"), nil
		},
	})
	if err == nil {
		t.Fatal("expected injected outbox insert error")
	}
	if !lockCalled {
		t.Fatal("expected TxLock to execute inside the MySQL write transaction")
	}
	if drv.committed != 0 || drv.rolledBack != 1 {
		t.Fatalf("expected 0 commits and 1 rollback on outbox failure, got committed=%d rolledBack=%d", drv.committed, drv.rolledBack)
	}
}
