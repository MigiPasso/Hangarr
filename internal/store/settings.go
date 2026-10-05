package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Setting keys.
const SettingSyncTime = "sync_time" // "HH:MM", local time (TZ)

// Setting returns a setting value, or def when unset.
func (s *Store) Setting(ctx context.Context, key, def string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM app_settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return def, nil
	}
	return v, err
}

// SetSetting stores a setting value.
func (s *Store) SetSetting(ctx context.Context, key, value string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO app_settings(key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, key, value, now.Unix())
	return err
}

// Cleanup score settings (Réglages page).
const (
	SettingCleanupThreshold  = "cleanup_threshold"
	SettingScoreNeverWatched = "score_never_watched"
	SettingScoreContinuing   = "score_active_continuing"
	SettingScoreActiveWindow = "score_active_window_days"
)
