package turns

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/kirin7002/app_personal_agent/contract"
	_ "modernc.org/sqlite"
)

//go:embed sqlite.sql
var SQLiteSchema string

// OpenSQLite opens a persistent SQLite database configured with WAL mode,
// 5-second busy_timeout, foreign keys, and BEGIN IMMEDIATE transactions so
// turn cancellation and MCP task writes serialize cleanly.
func OpenSQLite(path string) (*sql.DB, error) {
	dsn := buildSQLiteDSN(path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// Limit concurrent open connections modestly while allowing concurrent readers
	// under WAL; writers serialize cleanly via _txlock=immediate + busy_timeout.
	db.SetMaxOpenConns(4)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := db.Exec(SQLiteSchema); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func buildSQLiteDSN(path string) string {
	if strings.HasPrefix(path, "file:") {
		if strings.Contains(path, "?") {
			return path
		}
		return path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)&_txlock=immediate"
	}
	u := &url.URL{Scheme: "file", Opaque: path}
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(ON)")
	q.Set("_txlock", "immediate")
	return u.String() + "?" + q.Encode()
}

// SQLiteStore persists voice turns in SQLite as a zero-dependency drop-in
// alternative to MySQLStore.
type SQLiteStore struct {
	DB    *sql.DB
	mu    sync.Mutex
	locks map[contract.Scope]*scopeLock
}

func NewSQLiteStore(db *sql.DB) *SQLiteStore {
	return &SQLiteStore{
		DB:    db,
		locks: make(map[contract.Scope]*scopeLock),
	}
}

func (s *SQLiteStore) LockScope(ctx context.Context, scope contract.Scope) (func(), error) {
	s.mu.Lock()
	if s.locks == nil {
		s.locks = make(map[contract.Scope]*scopeLock)
	}
	lock := s.locks[scope]
	if lock == nil {
		lock = &scopeLock{token: make(chan struct{}, 1)}
		s.locks[scope] = lock
	}
	lock.waiters++
	s.mu.Unlock()

	release := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		lock.waiters--
		if lock.waiters == 0 {
			delete(s.locks, scope)
		}
	}
	select {
	case lock.token <- struct{}{}:
		return func() { <-lock.token; release() }, nil
	case <-ctx.Done():
		release()
		return nil, ctx.Err()
	}
}

func scanSQLiteRecord(row scanner) (Record, error) {
	var record Record
	var body string
	var closed, cancelled int
	if err := row.Scan(&body, &closed, &cancelled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return record, ErrNotFound
		}
		return record, err
	}
	if err := json.Unmarshal([]byte(body), &record); err != nil {
		return Record{}, err
	}
	record.Closed = closed != 0
	record.Cancelled = cancelled != 0
	return record, nil
}

func (s *SQLiteStore) Get(ctx context.Context, scope contract.Scope, id string) (Record, error) {
	return scanSQLiteRecord(s.DB.QueryRowContext(ctx,
		"SELECT record_json, closed, cancelled FROM voice_turns WHERE scope_key=? AND voice_turn_id=?",
		scopeKey(scope), id))
}

func (s *SQLiteStore) Register(ctx context.Context, record Record) error {
	body, err := json.Marshal(record)
	if err != nil {
		return err
	}
	binding := record.Context
	res, err := s.DB.ExecContext(ctx, `INSERT OR IGNORE INTO voice_turns
		(scope_key, voice_turn_id, request_id, turn_handle, hermes_session_id, record_json)
		VALUES (?, ?, ?, ?, ?, ?)`,
		scopeKey(binding.Scope), binding.VoiceTurnID, binding.RequestID, binding.TurnHandle, binding.HermesSessionID, string(body))
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return conflict()
	}
	return nil
}

func (s *SQLiteStore) Save(ctx context.Context, record Record) error {
	body, err := json.Marshal(record)
	if err != nil {
		return err
	}
	binding := record.Context
	closedInt := 0
	if record.Closed {
		closedInt = 1
	}
	_, err = s.DB.ExecContext(ctx, `UPDATE voice_turns
		SET record_json=?, closed=(CASE WHEN closed=1 OR ?=1 THEN 1 ELSE 0 END)
		WHERE scope_key=? AND voice_turn_id=?`,
		string(body), closedInt, scopeKey(binding.Scope), binding.VoiceTurnID)
	return err
}

func (s *SQLiteStore) Cancel(ctx context.Context, scope contract.Scope, id string) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO voice_turns (scope_key, voice_turn_id, record_json, cancelled, closed)
		VALUES (?, ?, '{}', 1, 1)
		ON CONFLICT(scope_key, voice_turn_id) DO UPDATE SET cancelled=1, closed=1`,
		scopeKey(scope), id)
	return err
}

func (s *SQLiteStore) ResolveTurn(ctx context.Context, meta contract.TurnMetadata, now time.Time) (contract.TurnContext, error) {
	record, err := scanSQLiteRecord(s.DB.QueryRowContext(ctx, bindingQuery, meta.TurnHandle, meta.HermesSessionID))
	return resolveRecord(record, err, meta, now)
}

// LockWritableTurn verifies the turn inside an active SQLite BEGIN IMMEDIATE
// transaction, serializing with Cancel without MySQL-only FOR UPDATE syntax.
func (s *SQLiteStore) LockWritableTurn(ctx context.Context, tx *sql.Tx, meta contract.TurnMetadata, now time.Time) (contract.TurnContext, error) {
	return LockWritableTurnSQLite(ctx, tx, meta, now)
}

// LockWritableTurnSQLite reads and validates the voice_turns row inside an active
// SQLite write transaction (opened with _txlock=immediate).
func LockWritableTurnSQLite(ctx context.Context, tx *sql.Tx, meta contract.TurnMetadata, now time.Time) (contract.TurnContext, error) {
	record, err := scanSQLiteRecord(tx.QueryRowContext(ctx, bindingQuery, meta.TurnHandle, meta.HermesSessionID))
	return resolveRecord(record, err, meta, now)
}

var _ Store = (*SQLiteStore)(nil)
