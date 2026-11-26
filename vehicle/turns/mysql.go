package turns

import (
	"context"
	"database/sql"
	"database/sql/driver"
	_ "embed"
	"encoding/json"
	"errors"
	"time"

	"github.com/kirin7002/app_personal_agent/contract"
)

//go:embed turns.sql
var MySQLSchema string

type MySQLStore struct {
	DB *sql.DB
}

// LockScope holds a MySQL advisory lock across the HTTP turn on a dedicated
// connection. MCP callbacks can still read and lock the turn row on another connection.
func (s *MySQLStore) LockScope(ctx context.Context, scope contract.Scope) (func(), error) {
	conn, err := s.DB.Conn(ctx)
	if err != nil {
		return nil, err
	}
	name := scopeKey(scope)
	var acquired int
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, 60)", name).Scan(&acquired); err != nil || acquired != 1 {
		conn.Close()
		if err != nil {
			return nil, err
		}
		return nil, errors.New("vehicle scope lock timed out")
	}
	return func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		var released int
		if err := conn.QueryRowContext(releaseCtx, "SELECT RELEASE_LOCK(?)", name).Scan(&released); err != nil || released != 1 {
			// Returning an unreleased advisory lock to the pool can block other turns.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
		_ = conn.Close()
	}, nil
}

type scanner interface {
	Scan(...any) error
}

func scanRecord(row scanner) (Record, error) {
	var record Record
	var body []byte
	var closed, cancelled bool
	if err := row.Scan(&body, &closed, &cancelled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return record, ErrNotFound
		}
		return record, err
	}
	if err := json.Unmarshal(body, &record); err != nil {
		return Record{}, err
	}
	record.Closed = closed
	record.Cancelled = cancelled
	return record, nil
}

func (s *MySQLStore) Get(ctx context.Context, scope contract.Scope, id string) (Record, error) {
	return scanRecord(s.DB.QueryRowContext(ctx,
		"SELECT record_json, closed, cancelled FROM voice_turns WHERE scope_key=? AND voice_turn_id=?",
		scopeKey(scope), id))
}

func (s *MySQLStore) Register(ctx context.Context, record Record) error {
	body, err := json.Marshal(record)
	if err != nil {
		return err
	}
	binding := record.Context
	result, err := s.DB.ExecContext(ctx, `INSERT INTO voice_turns
		(scope_key, voice_turn_id, request_id, turn_handle, hermes_session_id, record_json)
		VALUES (?, ?, ?, ?, ?, ?) ON DUPLICATE KEY UPDATE voice_turn_id=voice_turn_id`,
		scopeKey(binding.Scope), binding.VoiceTurnID, binding.RequestID, binding.TurnHandle, binding.HermesSessionID, body)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return conflict()
	}
	// clientFoundRows may report an unchanged duplicate as one affected row.
	found, err := s.Get(ctx, binding.Scope, binding.VoiceTurnID)
	if err != nil || found.Context.TurnHandle != binding.TurnHandle || found.Cancelled {
		return conflict()
	}
	return nil
}

func (s *MySQLStore) Save(ctx context.Context, record Record) error {
	body, err := json.Marshal(record)
	if err != nil {
		return err
	}
	binding := record.Context
	_, err = s.DB.ExecContext(ctx, `UPDATE voice_turns SET record_json=?, closed=(closed OR ?)
		WHERE scope_key=? AND voice_turn_id=?`, body, record.Closed, scopeKey(binding.Scope), binding.VoiceTurnID)
	return err
}

func (s *MySQLStore) Cancel(ctx context.Context, scope contract.Scope, id string) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO voice_turns (scope_key, voice_turn_id, record_json, cancelled, closed)
		VALUES (?, ?, '{}', TRUE, TRUE) ON DUPLICATE KEY UPDATE cancelled=TRUE, closed=TRUE`,
		scopeKey(scope), id)
	return err
}

func (s *MySQLStore) ResolveTurn(ctx context.Context, meta contract.TurnMetadata, now time.Time) (contract.TurnContext, error) {
	record, err := scanRecord(s.DB.QueryRowContext(ctx, bindingQuery, meta.TurnHandle, meta.HermesSessionID))
	return resolveRecord(record, err, meta, now)
}

const bindingQuery = "SELECT record_json, closed, cancelled FROM voice_turns WHERE turn_handle=? AND hermes_session_id=?"

// LockWritableTurn locks the same row mutated by Cancel. The caller must make
// its one-write check, task mutation and outbox mutation before committing tx.
func LockWritableTurn(ctx context.Context, tx *sql.Tx, meta contract.TurnMetadata, now time.Time) (contract.TurnContext, error) {
	record, err := scanRecord(tx.QueryRowContext(ctx, bindingQuery+" FOR UPDATE", meta.TurnHandle, meta.HermesSessionID))
	return resolveRecord(record, err, meta, now)
}

func resolveRecord(record Record, err error, meta contract.TurnMetadata, now time.Time) (contract.TurnContext, error) {
	if errors.Is(err, ErrNotFound) {
		return contract.TurnContext{}, invalidTurn()
	}
	if err != nil {
		return contract.TurnContext{}, err
	}
	return resolve(record, meta, now)
}
