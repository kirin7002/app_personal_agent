package tasks

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/kirin7002/app_personal_agent/contract"
)

func (s *MySQLStore) ResultPlaybackSettled(ctx context.Context, scope contract.Scope, playbackID string, since time.Time) (bool, error) {
	return playbackReceiptExists(ctx, s.DB, scope, playbackID, since.UTC())
}

func (s *MySQLStore) SettleResultPlayback(ctx context.Context, scope contract.Scope, playbackID string, at time.Time) error {
	if _, err := s.DB.ExecContext(ctx, "DELETE FROM vehicle_playback_receipts WHERE settled_at < ?", at.Add(-contract.ResultRetention).UTC()); err != nil {
		return err
	}
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO vehicle_playback_receipts (scope_key, playback_id, settled_at) VALUES (?, ?, ?)
		ON DUPLICATE KEY UPDATE settled_at = settled_at`, scopeKey(scope), playbackID, at.UTC())
	return err
}

func (s *SQLiteStore) ResultPlaybackSettled(ctx context.Context, scope contract.Scope, playbackID string, since time.Time) (bool, error) {
	return playbackReceiptExists(ctx, s.DB, scope, playbackID, formatSQLiteTime(since))
}

func (s *SQLiteStore) SettleResultPlayback(ctx context.Context, scope contract.Scope, playbackID string, at time.Time) error {
	if _, err := s.DB.ExecContext(ctx, "DELETE FROM vehicle_playback_receipts WHERE settled_at < ?", formatSQLiteTime(at.Add(-contract.ResultRetention))); err != nil {
		return err
	}
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO vehicle_playback_receipts (scope_key, playback_id, settled_at) VALUES (?, ?, ?)
		ON CONFLICT (scope_key, playback_id) DO NOTHING`, scopeKey(scope), playbackID, formatSQLiteTime(at))
	return err
}

func playbackReceiptExists(ctx context.Context, db *sql.DB, scope contract.Scope, playbackID string, since any) (bool, error) {
	var hit int
	err := db.QueryRowContext(ctx, `SELECT 1 FROM vehicle_playback_receipts
		WHERE scope_key = ? AND playback_id = ? AND settled_at >= ?`, scopeKey(scope), playbackID, since).Scan(&hit)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

var (
	_ contract.ResultPlaybackReceipts = (*MySQLStore)(nil)
	_ contract.ResultPlaybackReceipts = (*SQLiteStore)(nil)
)
