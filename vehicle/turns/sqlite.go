package turns

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
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
	DB *sql.DB
}

func NewSQLiteStore(db *sql.DB) *SQLiteStore {
	return &SQLiteStore{DB: db}
}

// A ticket lives longer than the bounded turn, including time spent waiting
// for admission. Stale tickets become eligible for removal after a process exits.
const sqliteScopeTicketLifetime = 2 * time.Minute

func (s *SQLiteStore) LockScope(ctx context.Context, scope contract.Scope) (func(), error) {
	now := time.Now()
	deadline := now.Add(sqliteScopeTicketLifetime)
	db := s.DB
	if _, err := db.ExecContext(ctx, "DELETE FROM voice_scope_queue WHERE expires_at <= ?", now.UnixMilli()); err != nil {
		return nil, err
	}
	key := scopeKey(scope)
	result, err := db.ExecContext(ctx, "INSERT INTO voice_scope_queue (scope_key, expires_at) VALUES (?, ?)", key, deadline.UnixMilli())
	if err != nil {
		return nil, err
	}
	ticket, err := result.LastInsertId()
	if err != nil {
		return nil, err
	}
	release := func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = db.ExecContext(cleanup, "DELETE FROM voice_scope_queue WHERE ticket_id = ?", ticket)
	}
	acquired := false
	defer func() {
		if !acquired {
			release()
		}
	}()

	wait, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		now := time.Now()
		var first int64
		err := db.QueryRowContext(wait, `SELECT ticket_id FROM voice_scope_queue
			WHERE scope_key = ? AND expires_at > ? ORDER BY ticket_id LIMIT 1`,
			key, now.UnixMilli()).Scan(&first)
		if err != nil {
			return nil, err
		}
		if err := wait.Err(); err != nil {
			return nil, err
		}
		if first == ticket {
			acquired = true
			return release, nil
		}
		select {
		case <-wait.Done():
			return nil, wait.Err()
		case <-ticker.C:
		}
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
