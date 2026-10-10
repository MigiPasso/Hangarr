package store

import (
	"context"
	"database/sql"
	"sort"
)

// LinkWatchEventsByKey resolves media_id from the keys of the server each
// event comes from (Plex rating keys for Tautulli and the Plex history,
// Jellyfin ids for the Jellyfin history): a movie row by rating_key, an
// episode by its show (grandparent_rating_key). Arr-managed rows win over
// server_only ones. Every event is re-evaluated, since keys can move between
// syncs.
func (s *Store) LinkWatchEventsByKey(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, l := range []struct{ col, sources string }{
		{"plex_rating_key", `watch_events.source <> 'jellyfin_history'`},
		{"jellyfin_id", `watch_events.source = 'jellyfin_history'`},
	} {
		//nolint:gosec // G202: constant fragments only
		if _, err := tx.ExecContext(ctx, `UPDATE watch_events SET media_id = (
			SELECT m.id FROM media m
			WHERE m.`+l.col+` IS NOT NULL
			  AND ((watch_events.media_type = 'movie' AND m.kind = 'movie' AND m.`+l.col+` = watch_events.rating_key)
			    OR (watch_events.media_type = 'episode' AND m.kind = 'series'
			        AND m.`+l.col+` = watch_events.grandparent_rating_key))
			ORDER BY m.arr_id IS NULL, m.id LIMIT 1)
			WHERE `+l.sources); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// UnlinkedTitle is a distinct (type, title, year) of events without media.
type UnlinkedTitle struct {
	MediaType string // movie | episode
	Title     string // movie title, or show title for episodes
	Year      int
}

// UnlinkedEventTitles lists titles of movie/episode events without media_id.
func (s *Store) UnlinkedEventTitles(ctx context.Context) ([]UnlinkedTitle, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT media_type,
		CASE media_type WHEN 'episode' THEN COALESCE(grandparent_title, '') ELSE COALESCE(title, '') END,
		COALESCE(year, 0)
		FROM watch_events WHERE media_id IS NULL AND media_type IN ('movie', 'episode')`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []UnlinkedTitle
	for rows.Next() {
		var u UnlinkedTitle
		if err := rows.Scan(&u.MediaType, &u.Title, &u.Year); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// TitleLink assigns media to the unlinked events of one title.
type TitleLink struct {
	UnlinkedTitle
	MediaID int64
}

// LinkWatchEventsByTitle applies title-based fallback links.
func (s *Store) LinkWatchEventsByTitle(ctx context.Context, links []TitleLink) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, l := range links {
		if _, err := tx.ExecContext(ctx, `UPDATE watch_events SET media_id = ? WHERE media_id IS NULL
			AND media_type = ? AND COALESCE(year, 0) = ?
			AND (CASE media_type WHEN 'episode' THEN COALESCE(grandparent_title, '') ELSE COALESCE(title, '') END) = ?`,
			l.MediaID, l.MediaType, l.Year, l.Title); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// MediaTitle is a media row exposed for title fallback.
type MediaTitle struct {
	ID    int64
	Kind  string
	Title string
	Year  int
}

// ListMediaTitles returns every media title.
func (s *Store) ListMediaTitles(ctx context.Context) ([]MediaTitle, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, kind, title, COALESCE(year, 0) FROM media`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []MediaTitle
	for rows.Next() {
		var m MediaTitle
		if err := rows.Scan(&m.ID, &m.Kind, &m.Title, &m.Year); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// EventCounts reports linked and orphan (no media) movie/episode events.
func (s *Store) EventCounts(ctx context.Context) (linked, orphans int, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT
		COUNT(CASE WHEN media_id IS NOT NULL THEN 1 END), COUNT(CASE WHEN media_id IS NULL THEN 1 END)
		FROM watch_events WHERE media_type IN ('movie', 'episode')`).Scan(&linked, &orphans)
	return linked, orphans, err
}

// SeasonStat is per-season progress.
type SeasonStat struct {
	Number       int
	Downloaded   int
	Watched      int
	LastViewedAt *int64
}

// MediaStats gathers what is needed to recompute derived columns.
type MediaStats struct {
	MediaID        int64
	Kind           string
	Status         string
	Managed        bool // arr_id IS NOT NULL
	SizeBytes      int64
	AddedAt        *int64
	Plays          int
	CompletedPlays int
	WatchSeconds   int64
	Viewers        int
	LastViewedAt   *int64
	LastViewedBy   *int64
	Seasons        []SeasonStat
}

// play groups sessions: Tautulli resumes share a reference_id; rows without
// one (Plex history) count individually.
const playKey = `COALESCE('r' || reference_id, 'e' || id)`

// queryEach runs a query and calls fn for every row.
func (s *Store) queryEach(ctx context.Context, query string, fn func(*sql.Rows) error, args ...any) error {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// LoadMediaStats aggregates watch_events per media, all users combined.
func (s *Store) LoadMediaStats(ctx context.Context) ([]*MediaStats, error) {
	byID := map[int64]*MediaStats{}
	var order []int64
	err := s.queryEach(ctx, `SELECT id, kind, COALESCE(status, ''), arr_id IS NOT NULL, size_bytes, added_at
		FROM media ORDER BY id`, func(rows *sql.Rows) error {
		m := &MediaStats{}
		var added sql.NullInt64
		if err := rows.Scan(&m.MediaID, &m.Kind, &m.Status, &m.Managed, &m.SizeBytes, &added); err != nil {
			return err
		}
		m.AddedAt = ptrInt(added)
		byID[m.MediaID] = m
		order = append(order, m.MediaID)
		return nil
	})
	if err != nil {
		return nil, err
	}

	err = s.queryEach(ctx, `SELECT media_id,
		COUNT(DISTINCT `+playKey+`),
		COUNT(DISTINCT CASE WHEN watched = 1 THEN `+playKey+` END),
		COALESCE(SUM(play_seconds), 0), COUNT(DISTINCT user_id), MAX(stopped_at)
		FROM watch_events WHERE media_id IS NOT NULL GROUP BY media_id`, func(rows *sql.Rows) error {
		var id int64
		var last sql.NullInt64
		var plays, completed, viewers int
		var secs int64
		if err := rows.Scan(&id, &plays, &completed, &secs, &viewers, &last); err != nil {
			return err
		}
		if m := byID[id]; m != nil {
			m.Plays, m.CompletedPlays, m.WatchSeconds, m.Viewers, m.LastViewedAt = plays, completed, secs, viewers, ptrInt(last)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	err = s.queryEach(ctx, `SELECT media_id, user_id FROM (
		SELECT media_id, user_id, ROW_NUMBER() OVER (PARTITION BY media_id ORDER BY stopped_at DESC, id DESC) AS rn
		FROM watch_events WHERE media_id IS NOT NULL) WHERE rn = 1`, func(rows *sql.Rows) error {
		var id, user int64
		if err := rows.Scan(&id, &user); err != nil {
			return err
		}
		if m := byID[id]; m != nil {
			m.LastViewedBy = &user
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Seasons: Sonarr seasons (downloaded counts) merged with seasons seen in
	// history (a watched season may no longer exist in Sonarr).
	seasons := map[int64]map[int]*SeasonStat{}
	season := func(id int64, n int) *SeasonStat {
		if seasons[id] == nil {
			seasons[id] = map[int]*SeasonStat{}
		}
		if seasons[id][n] == nil {
			seasons[id][n] = &SeasonStat{Number: n}
		}
		return seasons[id][n]
	}
	err = s.queryEach(ctx, `SELECT media_id, season_number, episode_file_count FROM seasons`, func(rows *sql.Rows) error {
		var id int64
		var n, files int
		if err := rows.Scan(&id, &n, &files); err != nil {
			return err
		}
		season(id, n).Downloaded = files
		return nil
	})
	if err != nil {
		return nil, err
	}
	err = s.queryEach(ctx, `SELECT media_id, season_number,
		COUNT(DISTINCT CASE WHEN watched = 1 THEN episode_number END), MAX(stopped_at)
		FROM watch_events WHERE media_type = 'episode' AND media_id IS NOT NULL AND season_number IS NOT NULL
		GROUP BY media_id, season_number`, func(rows *sql.Rows) error {
		var id int64
		var n, watched int
		var last sql.NullInt64
		if err := rows.Scan(&id, &n, &watched, &last); err != nil {
			return err
		}
		st := season(id, n)
		st.Watched, st.LastViewedAt = watched, ptrInt(last)
		return nil
	})
	if err != nil {
		return nil, err
	}

	out := make([]*MediaStats, 0, len(order))
	for _, id := range order {
		m := byID[id]
		for _, st := range seasons[id] {
			m.Seasons = append(m.Seasons, *st)
		}
		sort.Slice(m.Seasons, func(i, j int) bool { return m.Seasons[i].Number < m.Seasons[j].Number })
		out = append(out, m)
	}
	return out, nil
}

// Derived is the recomputed state of one media row.
type Derived struct {
	*MediaStats
	WatchedEpisodes *int
	WatchState      string
}

// SaveDerived writes aggregates, watch_state and per-season
// progress in one transaction.
func (s *Store) SaveDerived(ctx context.Context, items []Derived) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	media, err := tx.PrepareContext(ctx, `UPDATE media SET play_count = ?, completed_play_count = ?,
		watch_seconds = ?, distinct_viewers = ?, last_viewed_at = ?, last_viewed_by = ?, watched_episodes = ?,
		watch_state = ?, cleanup_score = NULL WHERE id = ?`)
	if err != nil {
		return err
	}
	defer func() { _ = media.Close() }()
	seasons, err := tx.PrepareContext(ctx, `UPDATE seasons SET watched_episodes = ?, last_viewed_at = ?
		WHERE media_id = ? AND season_number = ?`)
	if err != nil {
		return err
	}
	defer func() { _ = seasons.Close() }()
	for _, d := range items {
		if _, err := media.ExecContext(ctx, d.Plays, d.CompletedPlays, d.WatchSeconds, d.Viewers,
			nullInt(d.LastViewedAt), nullInt(d.LastViewedBy), nullIntPtr(d.WatchedEpisodes), d.WatchState,
			d.MediaID); err != nil {
			return err
		}
		for _, st := range d.Seasons {
			if _, err := seasons.ExecContext(ctx, st.Watched, nullInt(st.LastViewedAt), d.MediaID, st.Number); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}
