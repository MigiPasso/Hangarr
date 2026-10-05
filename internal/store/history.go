package store

import (
	"context"
	"database/sql"
	"errors"
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
		  watched = excluded.watched, platform = excluded.platform, player = excluded.player,
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

// FinishSyncRun records the outcome of a source sync.
func (s *Store) FinishSyncRun(ctx context.Context, id int64, now time.Time, items int, runErr error) error {
	status, msg := "success", sql.NullString{}
	if runErr != nil {
		status, msg = "error", sql.NullString{String: runErr.Error(), Valid: true}
	}
	_, err := s.db.ExecContext(ctx, `UPDATE sync_runs SET finished_at = ?, status = ?, items = ?, error = ?
		WHERE id = ?`, now.Unix(), status, items, msg, id)
	return err
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
