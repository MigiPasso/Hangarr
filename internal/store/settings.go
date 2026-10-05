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

// SettingCleanupDays is the cleanup rule (Réglages page): items nobody
// watched for that many days are candidates. Settings of the former cleanup
// score (cleanup_threshold, score_*) may remain in old databases, unused.
const SettingCleanupDays = "cleanup_inactive_days"

// SettingsWithPrefix returns every setting whose key starts with prefix,
// keyed without the prefix.
func (s *Store) SettingsWithPrefix(ctx context.Context, prefix string) (map[string]string, error) {
	all, err := settingsWithPrefix(ctx, s.db, prefix)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(all))
	for k, v := range all {
		out[k[len(prefix):]] = v
	}
	return out, nil
}

// settingsWithPrefix returns the matching settings keyed by full key.
func settingsWithPrefix(ctx context.Context, q interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}, prefix string) (map[string]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT key, value FROM app_settings WHERE substr(key, 1, ?) = ?`, len(prefix), prefix)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// ApplySettings sets and deletes settings and records the security event in
// one transaction.
func (s *Store) ApplySettings(ctx context.Context, set map[string]string, del []string, now time.Time, ev AuthEvent) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for k, v := range set {
		if _, err := tx.ExecContext(ctx, `INSERT INTO app_settings(key, value, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, k, v, now.Unix()); err != nil {
			return err
		}
	}
	for _, k := range del {
		if _, err := tx.ExecContext(ctx, `DELETE FROM app_settings WHERE key = ?`, k); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO auth_events(at, user_id, username, event, ip, user_agent)
		VALUES (?, ?, ?, ?, ?, ?)`, ev.At, nullInt(ev.UserID), ev.Username, ev.Event, ev.IP, ev.UserAgent); err != nil {
		return err
	}
	return tx.Commit()
}

// RewriteSettings rewrites, in one transaction, the settings whose key starts
// with prefix and for which fn returns ok (keys are passed without the
// prefix). It returns how many settings changed.
func (s *Store) RewriteSettings(ctx context.Context, prefix string, now time.Time,
	fn func(key, value string) (string, bool, error)) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	current, err := settingsWithPrefix(ctx, tx, prefix)
	if err != nil {
		return 0, err
	}
	n := 0
	for k, v := range current {
		nv, ok, err := fn(k[len(prefix):], v)
		if err != nil {
			return 0, err
		}
		if !ok {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE app_settings SET value = ?, updated_at = ? WHERE key = ?`,
			nv, now.Unix(), k); err != nil {
			return 0, err
		}
		n++
	}
	return n, tx.Commit()
}
