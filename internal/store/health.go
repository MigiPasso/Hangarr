package store

import (
	"context"
	"database/sql"
)

// HealthItem is a media listed on the Health page.
type HealthItem struct {
	ID          int64
	Kind        string
	Title       string
	Year        int
	SizeBytes   int64
	MatchMethod string
}

// HealthList is a capped list with the full count.
type HealthList struct {
	Count int
	Items []HealthItem
}

// DataHealth lists catalog inconsistencies.
type DataHealth struct {
	ServerOnly  HealthList // in the media server, not in Sonarr/Radarr
	NoFile      HealthList // in Sonarr/Radarr without any file
	NotInServer HealthList // in Sonarr/Radarr with files but not found in the media server
	Fuzzy       HealthList // matched by title and year only
	Orphans     int        // history sessions whose media disappeared
}

// DataHealth returns each list capped at limit items, by title.
func (s *Store) DataHealth(ctx context.Context, limit int) (*DataHealth, error) {
	d := &DataHealth{}
	lists := []struct {
		dst   *HealthList
		where string
	}{
		{&d.ServerOnly, `match_state = 'server_only'`},
		{&d.NoFile, `arr_id IS NOT NULL AND size_bytes = 0`},
		{&d.NotInServer, `match_state = 'arr_only' AND size_bytes > 0`},
		{&d.Fuzzy, `match_method = 'fuzzy'`},
	}
	for _, l := range lists {
		//nolint:gosec // G202: the WHERE clauses are constants
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM media WHERE `+l.where).Scan(&l.dst.Count); err != nil {
			return nil, err
		}
		//nolint:gosec // G202: the WHERE clauses are constants
		err := s.queryEach(ctx, `SELECT id, kind, title, COALESCE(year, 0), size_bytes, COALESCE(match_method, '')
			FROM media WHERE `+l.where+` ORDER BY title_norm, id LIMIT ?`, func(rows *sql.Rows) error {
			var it HealthItem
			if err := rows.Scan(&it.ID, &it.Kind, &it.Title, &it.Year, &it.SizeBytes, &it.MatchMethod); err != nil {
				return err
			}
			l.dst.Items = append(l.dst.Items, it)
			return nil
		}, limit)
		if err != nil {
			return nil, err
		}
	}
	var err error
	_, d.Orphans, err = s.EventCounts(ctx)
	return d, err
}

// RecentSyncRuns returns the last runs of every source, newest first.
func (s *Store) RecentSyncRuns(ctx context.Context, limit int) ([]SyncRun, error) {
	var out []SyncRun
	err := s.queryEach(ctx, `SELECT id, started_at, finished_at, source, status, COALESCE(items, 0), COALESCE(error, '')
		FROM sync_runs ORDER BY id DESC LIMIT ?`, func(rows *sql.Rows) error {
		var r SyncRun
		var fin sql.NullInt64
		if err := rows.Scan(&r.ID, &r.StartedAt, &fin, &r.Source, &r.Status, &r.Items, &r.Error); err != nil {
			return err
		}
		r.FinishedAt = ptrInt(fin)
		out = append(out, r)
		return nil
	}, limit)
	return out, err
}
