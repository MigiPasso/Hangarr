package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// WatchEvent is one raw session (Tautulli) or history entry (Plex).
type WatchEvent struct {
	Source               string
	SourceID             int64
	ReferenceID          *int64
	MediaType            string
	RatingKey            string
	ParentRatingKey      string
	GrandparentRatingKey string
	SeasonNumber         *int
	EpisodeNumber        *int
	UserID               int64
	StartedAt            int64
	StoppedAt            int64
	PlaySeconds          int64
	PercentComplete      int
	Watched              bool
	Platform             string
	Player               string
	TranscodeDecision    string
	Title                string
	GrandparentTitle     string
	Year                 int
}

// UpsertWatchEvents inserts or refreshes events, keyed by (source, source_id).
// media_id is resolved separately by LinkWatchEvents.
func (s *Store) UpsertWatchEvents(ctx context.Context, events []WatchEvent) error {
	if len(events) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO watch_events(source, source_id, reference_id, media_type,
		rating_key, parent_rating_key, grandparent_rating_key, season_number, episode_number, user_id,
		started_at, stopped_at, play_seconds, percent_complete, watched, platform, player, transcode_decision,
		title, grandparent_title, year)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(source, source_id) DO UPDATE SET reference_id = excluded.reference_id,
		  media_type = excluded.media_type, rating_key = excluded.rating_key,
		  parent_rating_key = excluded.parent_rating_key, grandparent_rating_key = excluded.grandparent_rating_key,
		  season_number = excluded.season_number, episode_number = excluded.episode_number,
		  user_id = excluded.user_id, started_at = excluded.started_at, stopped_at = excluded.stopped_at,
		  play_seconds = excluded.play_seconds, percent_complete = excluded.percent_complete,
		  -- never "unwatch": the merge may have completed a session from the Plex history
		  watched = MAX(watch_events.watched, excluded.watched), platform = excluded.platform, player = excluded.player,
		  transcode_decision = excluded.transcode_decision, title = excluded.title,
		  grandparent_title = excluded.grandparent_title, year = excluded.year`)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()
	for _, e := range events {
		if _, err := stmt.ExecContext(ctx, e.Source, e.SourceID, nullInt(e.ReferenceID), e.MediaType,
			nullString(e.RatingKey), nullString(e.ParentRatingKey), nullString(e.GrandparentRatingKey),
			nullIntPtr(e.SeasonNumber), nullIntPtr(e.EpisodeNumber), e.UserID, e.StartedAt, e.StoppedAt,
			e.PlaySeconds, e.PercentComplete, e.Watched, nullString(e.Platform), nullString(e.Player),
			nullString(e.TranscodeDecision), nullString(e.Title), nullString(e.GrandparentTitle),
			nullPositive(int64(e.Year))); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Cursor returns the stored sync cursor of a source, "" if none.
func (s *Store) Cursor(ctx context.Context, source string) (string, error) {
	var c sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT cursor FROM sync_cursors WHERE source = ?`, source).Scan(&c)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return c.String, err
}

// SetCursor stores a sync cursor.
func (s *Store) SetCursor(ctx context.Context, source, cursor string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO sync_cursors(source, cursor, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(source) DO UPDATE SET cursor = excluded.cursor, updated_at = excluded.updated_at`,
		source, cursor, now.Unix())
	return err
}

// PlexAccount is a Plex user (from Tautulli get_users or Plex /accounts).
type PlexAccount struct {
	AccountID    int64
	Username     string
	FriendlyName string
	IsActive     bool
	ThumbURL     string
}

// UpsertPlexAccounts refreshes accounts. Accounts that disappeared are kept so
// that historical sessions still show a name.
func (s *Store) UpsertPlexAccounts(ctx context.Context, accounts []PlexAccount) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, a := range accounts {
		if _, err := tx.ExecContext(ctx, `INSERT INTO plex_accounts(account_id, username, friendly_name, is_active,
			thumb_url) VALUES (?, ?, ?, ?, ?) ON CONFLICT(account_id) DO UPDATE SET username = excluded.username,
			friendly_name = excluded.friendly_name, is_active = excluded.is_active, thumb_url = excluded.thumb_url`,
			a.AccountID, a.Username, nullString(a.FriendlyName), a.IsActive, nullString(a.ThumbURL)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// StartSyncRun records the start of a source sync and returns its id.
func (s *Store) StartSyncRun(ctx context.Context, source string, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO sync_runs(started_at, source, status) VALUES (?, ?, 'running')`,
		now.Unix(), source)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ErrSyncInterrupted marks a source sync stopped before its end (shutdown
// during the sync, or the process killed): status "interrupted".
var ErrSyncInterrupted = errors.New("interrupted")

// FinishSyncRun records the outcome of a source sync. A canceled context
// (shutdown) records the run as interrupted, not failed.
func (s *Store) FinishSyncRun(ctx context.Context, id int64, now time.Time, items int, runErr error) error {
	status, msg := "success", sql.NullString{}
	switch {
	case errors.Is(runErr, ErrSyncInterrupted), errors.Is(runErr, context.Canceled):
		status, msg = "interrupted", sql.NullString{String: ErrSyncInterrupted.Error(), Valid: true}
	case runErr != nil:
		status, msg = "error", sql.NullString{String: runErr.Error(), Valid: true}
	}
	_, err := s.db.ExecContext(ctx, `UPDATE sync_runs SET finished_at = ?, status = ?, items = ?, error = ?
		WHERE id = ?`, now.Unix(), status, items, msg, id)
	return err
}

// InterruptUnfinishedSyncRuns closes the runs left "running" by a previous
// process (killed during a sync): they would show as running forever. Call
// it at startup, before any sync.
func (s *Store) InterruptUnfinishedSyncRuns(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE sync_runs SET finished_at = ?, status = 'interrupted', error = ?
		WHERE finished_at IS NULL`, now.Unix(), ErrSyncInterrupted.Error())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SyncRunRetention is how long sync_runs rows are kept: each sync adds one
// per source, and the Tautulli webhook one or two per play.
const SyncRunRetention = 90 * 24 * time.Hour

// PruneSyncRuns deletes the finished runs started before before, except the
// latest run of each source (Health page, last sync).
func (s *Store) PruneSyncRuns(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sync_runs WHERE started_at < ? AND finished_at IS NOT NULL
		AND id NOT IN (SELECT MAX(id) FROM sync_runs GROUP BY source)`, before.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SyncRun is a row of sync_runs.
type SyncRun struct {
	ID         int64
	StartedAt  int64
	FinishedAt *int64
	Source     string
	Status     string
	Items      int
	Error      string
}

// LatestSyncRuns returns the most recent run of each source.
func (s *Store) LatestSyncRuns(ctx context.Context) ([]SyncRun, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, started_at, finished_at, source, status, COALESCE(items, 0),
		COALESCE(error, '') FROM sync_runs WHERE id IN (SELECT MAX(id) FROM sync_runs GROUP BY source)
		ORDER BY source`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []SyncRun
	for rows.Next() {
		var r SyncRun
		var fin sql.NullInt64
		if err := rows.Scan(&r.ID, &r.StartedAt, &fin, &r.Source, &r.Status, &r.Items, &r.Error); err != nil {
			return nil, err
		}
		r.FinishedAt = ptrInt(fin)
		out = append(out, r)
	}
	return out, rows.Err()
}

// DropOtherHistory keeps a single history source: it deletes the watch
// events of every source but keep, the given cursors of those sources (so
// that switching back re-imports their whole history), and the accounts
// left without any event. Plex and Tautulli number the server owner
// differently (1 vs the plex.tv id): stale accounts would show the same
// person twice. The caller upserts the users of the kept source right after.
// It returns the number of events deleted.
func (s *Store) DropOtherHistory(ctx context.Context, keep string, otherCursors ...string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `DELETE FROM watch_events WHERE source <> ?`, keep)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	for _, c := range otherCursors {
		if _, err := tx.ExecContext(ctx, `DELETE FROM sync_cursors WHERE source = ?`, c); err != nil {
			return 0, err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM plex_accounts WHERE account_id NOT IN
		(SELECT DISTINCT user_id FROM watch_events WHERE user_id IS NOT NULL)`); err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// RemapHistoryUsers moves the events of source from one account id to
// another (Plex numbers the server owner 1, Tautulli uses the plex.tv id)
// and deletes the accounts that were moved away from.
func (s *Store) RemapHistoryUsers(ctx context.Context, source string, ids map[int64]int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for from, to := range ids {
		if from == to {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE watch_events SET user_id = ? WHERE source = ? AND user_id = ?`,
			to, source, from); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM plex_accounts WHERE account_id = ?`, from); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// KeepViewers deletes the accounts not in keep and their plays (merged
// mode: users Tautulli remembers but who no longer have access to the Plex
// server). Events without a user are kept. It returns the deleted events.
func (s *Store) KeepViewers(ctx context.Context, keep []int64) (int64, error) {
	if len(keep) == 0 {
		return 0, nil // never empty the history on an empty account list
	}
	in := strings.TrimSuffix(strings.Repeat("?,", len(keep)), ",")
	args := make([]any, len(keep))
	for i, id := range keep {
		args[i] = id
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `DELETE FROM watch_events WHERE user_id IS NOT NULL AND user_id NOT IN (`+in+`)`, args...) //nolint:gosec // placeholders only
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM plex_accounts WHERE account_id NOT IN (`+in+`)`, args...); err != nil { //nolint:gosec // placeholders only
		return 0, err
	}
	return n, tx.Commit()
}

// MergeWindow bounds how far a Plex history entry (viewedAt, recorded near
// the end of playback) may be from a Tautulli session to be the same play:
// from 30 min before it started to 3 h after it stopped.
const (
	mergeBefore = 30 * 60
	mergeAfter  = 3 * 60 * 60
)

// mergeMatch is true when the Tautulli session t and the Plex history entry
// p are the same play: same user, same item (same episode for a series, or
// same rating key when unlinked) and a compatible time.
const mergeMatch = `t.source = 'tautulli' AND p.source = 'plex_history' AND t.user_id = p.user_id
	AND ((p.media_id IS NOT NULL AND t.media_id = p.media_id
	      AND COALESCE(t.season_number, -1) = COALESCE(p.season_number, -1)
	      AND COALESCE(t.episode_number, -1) = COALESCE(p.episode_number, -1))
	  OR (p.media_id IS NULL AND t.rating_key = p.rating_key))
	AND p.stopped_at BETWEEN t.started_at - ? AND t.stopped_at + ?`

// MergeStats describes a merge of the Plex history into Tautulli's.
type MergeStats struct {
	Completed int64 // Tautulli sessions marked watched because Plex recorded them
	Merged    int64 // Plex entries dropped: Tautulli has the same play
	Estimated int64 // Plex-only plays given the runtime of the item as duration
}

// MergePlexHistory merges the Plex history (the reference of what was
// watched) into the Tautulli sessions (durations, devices, partial plays).
// It runs after the events are linked to media. Plex entries matching a
// Tautulli session are dropped (and complete it); the others stay, with the
// runtime from Sonarr/Radarr as estimated duration.
func (s *Store) MergePlexHistory(ctx context.Context) (MergeStats, error) {
	var st MergeStats
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return st, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `UPDATE watch_events AS t SET watched = 1
		WHERE t.source = 'tautulli' AND t.watched = 0
		  AND EXISTS (SELECT 1 FROM watch_events AS p WHERE `+mergeMatch+`)`, mergeBefore, mergeAfter)
	if err != nil {
		return st, err
	}
	if st.Completed, err = res.RowsAffected(); err != nil {
		return st, err
	}
	res, err = tx.ExecContext(ctx, `DELETE FROM watch_events AS p
		WHERE p.source = 'plex_history'
		  AND EXISTS (SELECT 1 FROM watch_events AS t WHERE `+mergeMatch+`)`, mergeBefore, mergeAfter)
	if err != nil {
		return st, err
	}
	if st.Merged, err = res.RowsAffected(); err != nil {
		return st, err
	}
	res, err = tx.ExecContext(ctx, `UPDATE watch_events AS p
		SET play_seconds = (SELECT m.runtime_minutes * 60 FROM media m WHERE m.id = p.media_id)
		WHERE p.source = 'plex_history' AND COALESCE(p.play_seconds, 0) = 0
		  AND (SELECT COALESCE(m.runtime_minutes, 0) FROM media m WHERE m.id = p.media_id) > 0`)
	if err != nil {
		return st, err
	}
	if st.Estimated, err = res.RowsAffected(); err != nil {
		return st, err
	}
	return st, tx.Commit()
}
