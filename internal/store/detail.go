package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// Media is the full row shown on the detail page.
type Media struct {
	ID                 int64
	Kind               string
	ArrID              int64
	PlexRatingKey      string
	Title              string
	TitleSlug          string
	Year               int
	Overview           string
	TvdbID             int64
	TmdbID             int64
	ImdbID             string
	Status             string
	Genres             []string
	NetworkOrStudio    string
	Rating             float64
	SizeBytes          int64
	AddedAt            *int64
	Tags               []string
	QualityProfile     string
	Monitored          bool
	Path               string
	HasFile            bool
	TotalEpisodes      int
	DownloadedEpisodes int
	WatchedEpisodes    int
	RuntimeMinutes     int
	MatchState         string
	MatchMethod        string
	Plays              int
	CompletedPlays     int
	WatchSeconds       int64
	Viewers            int
	LastViewedAt       *int64
	LastViewedBy       string
	WatchState         string
	ProtectedLocal     bool // protected in Cleanarr only (plex_only items)
}

// MediaByID returns a media row; ErrNotFound if it does not exist.
func (s *Store) MediaByID(ctx context.Context, id int64) (*Media, error) {
	var m Media
	var genres, tags string
	var added, last sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT m.id, m.kind, COALESCE(m.arr_id, 0), COALESCE(m.plex_rating_key, ''),
		m.title, COALESCE(m.title_slug, ''), COALESCE(m.year, 0), COALESCE(m.overview, ''), COALESCE(m.tvdb_id, 0),
		COALESCE(m.tmdb_id, 0), COALESCE(m.imdb_id, ''), COALESCE(m.status, ''), m.genres,
		COALESCE(m.network_or_studio, ''), COALESCE(m.rating, 0), m.size_bytes, m.added_at, m.tags,
		COALESCE(m.quality_profile, ''), COALESCE(m.monitored, 0), COALESCE(m.path, ''), COALESCE(m.has_file, 0),
		COALESCE(m.total_episodes, 0), COALESCE(m.downloaded_episodes, 0), COALESCE(m.watched_episodes, 0),
		COALESCE(m.runtime_minutes, 0), m.match_state, COALESCE(m.match_method, ''), m.play_count,
		m.completed_play_count, m.watch_seconds, m.distinct_viewers, m.last_viewed_at,
		COALESCE(NULLIF(a.friendly_name, ''), a.username, ''), m.watch_state, m.protected_local
		FROM media m LEFT JOIN plex_accounts a ON a.account_id = m.last_viewed_by WHERE m.id = ?`, id).Scan(
		&m.ID, &m.Kind, &m.ArrID, &m.PlexRatingKey, &m.Title, &m.TitleSlug, &m.Year, &m.Overview, &m.TvdbID,
		&m.TmdbID, &m.ImdbID, &m.Status, &genres, &m.NetworkOrStudio, &m.Rating, &m.SizeBytes, &added, &tags,
		&m.QualityProfile, &m.Monitored, &m.Path, &m.HasFile, &m.TotalEpisodes, &m.DownloadedEpisodes,
		&m.WatchedEpisodes, &m.RuntimeMinutes, &m.MatchState, &m.MatchMethod, &m.Plays, &m.CompletedPlays,
		&m.WatchSeconds, &m.Viewers, &last, &m.LastViewedBy, &m.WatchState, &m.ProtectedLocal)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	m.AddedAt, m.LastViewedAt = ptrInt(added), ptrInt(last)
	_ = json.Unmarshal([]byte(genres), &m.Genres)
	_ = json.Unmarshal([]byte(tags), &m.Tags)
	return &m, nil
}

// Season is a stored Sonarr season with watch progress (all users).
type Season struct {
	Number            int
	Monitored         bool
	EpisodeFileCount  int
	TotalEpisodeCount int
	WatchedEpisodes   int
	SizeBytes         int64
	LastViewedAt      *int64
	PlexRatingKey     string
}

// Seasons lists the seasons of a series.
func (s *Store) Seasons(ctx context.Context, mediaID int64) ([]Season, error) {
	var out []Season
	err := s.queryEach(ctx, `SELECT season_number, COALESCE(monitored, 0), episode_file_count, total_episode_count,
		watched_episodes, size_bytes, last_viewed_at, COALESCE(plex_rating_key, '') FROM seasons
		WHERE media_id = ? ORDER BY season_number`, func(rows *sql.Rows) error {
		var se Season
		var last sql.NullInt64
		if err := rows.Scan(&se.Number, &se.Monitored, &se.EpisodeFileCount, &se.TotalEpisodeCount,
			&se.WatchedEpisodes, &se.SizeBytes, &last, &se.PlexRatingKey); err != nil {
			return err
		}
		se.LastViewedAt = ptrInt(last)
		out = append(out, se)
		return nil
	}, mediaID)
	return out, err
}

// HistoryRow is one session of a media's history.
type HistoryRow struct {
	UserID            int64
	UserName          string
	StartedAt         int64
	StoppedAt         int64
	SeasonNumber      *int
	EpisodeNumber     *int
	Title             string
	PlaySeconds       int64
	PercentComplete   int
	Watched           bool
	Platform          string
	Player            string
	TranscodeDecision string
}

// MediaHistory returns the most recent sessions of a media, newest first.
func (s *Store) MediaHistory(ctx context.Context, mediaID int64, limit int) ([]HistoryRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT w.user_id, COALESCE(NULLIF(a.friendly_name, ''), a.username, ''),
		COALESCE(w.started_at, 0), COALESCE(w.stopped_at, 0), w.season_number, w.episode_number, COALESCE(w.title, ''),
		COALESCE(w.play_seconds, 0), COALESCE(w.percent_complete, 0), w.watched, COALESCE(w.platform, ''),
		COALESCE(w.player, ''), COALESCE(w.transcode_decision, '')
		FROM watch_events w LEFT JOIN plex_accounts a ON a.account_id = w.user_id
		WHERE w.media_id = ? ORDER BY w.stopped_at DESC, w.id DESC LIMIT ?`, mediaID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []HistoryRow
	for rows.Next() {
		var h HistoryRow
		var season, episode sql.NullInt64
		if err := rows.Scan(&h.UserID, &h.UserName, &h.StartedAt, &h.StoppedAt, &season, &episode, &h.Title,
			&h.PlaySeconds, &h.PercentComplete, &h.Watched, &h.Platform, &h.Player, &h.TranscodeDecision); err != nil {
			return nil, err
		}
		if season.Valid {
			n := int(season.Int64)
			h.SeasonNumber = &n
		}
		if episode.Valid {
			n := int(episode.Int64)
			h.EpisodeNumber = &n
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// EpisodeWatch summarizes one user's sessions on one episode (or the movie
// itself when Season and Episode are 0).
type EpisodeWatch struct {
	UserID    int64
	UserName  string
	Season    int
	Episode   int
	Watched   bool // at least one complete play
	Plays     int
	LastAt    int64
	FirstSeen int64
}

// UserWatches aggregates a media's sessions per user and episode.
func (s *Store) UserWatches(ctx context.Context, mediaID int64) ([]EpisodeWatch, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT w.user_id, COALESCE(NULLIF(a.friendly_name, ''), a.username, ''),
		COALESCE(w.season_number, 0), COALESCE(w.episode_number, 0), MAX(w.watched),
		COUNT(DISTINCT `+playKey+`), MAX(w.stopped_at), MIN(w.started_at)
		FROM watch_events w LEFT JOIN plex_accounts a ON a.account_id = w.user_id
		WHERE w.media_id = ? GROUP BY w.user_id, COALESCE(w.season_number, 0), COALESCE(w.episode_number, 0)
		ORDER BY 2 COLLATE NOCASE, 3, 4`, mediaID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []EpisodeWatch
	for rows.Next() {
		var e EpisodeWatch
		if err := rows.Scan(&e.UserID, &e.UserName, &e.Season, &e.Episode, &e.Watched, &e.Plays, &e.LastAt,
			&e.FirstSeen); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// DeleteMedia removes a media row (after it was deleted from Sonarr/Radarr).
// History rows keep their data with media_id set to NULL.
func (s *Store) DeleteMedia(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM media WHERE id = ?`, id)
	return err
}

// SetLocalProtection protects (or not) an item in Cleanarr only.
func (s *Store) SetLocalProtection(ctx context.Context, id int64, protected bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE media SET protected_local = ? WHERE id = ?`, protected, id)
	return err
}

// SetMediaTags replaces the local copy of an item's tag labels (after they
// were changed in Sonarr/Radarr; the next sync reads them again anyway).
func (s *Store) SetMediaTags(ctx context.Context, id int64, labels []string) error {
	if labels == nil {
		labels = []string{}
	}
	b, err := json.Marshal(labels)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE media SET tags = ? WHERE id = ?`, string(b), id)
	return err
}

// Action is a row of action_log.
type Action struct {
	ID              int64
	At              int64
	Action          string
	MediaKind       string
	ArrID           int64
	Title           string
	SeasonNumber    *int
	SizeFreedBytes  int64
	DeleteFiles     bool
	ImportExclusion bool
	DryRun          bool
	Success         bool
	Error           string
	Username        string
}

// AddAction appends to action_log.
func (s *Store) AddAction(ctx context.Context, a Action) error {
	var season sql.NullInt64
	if a.SeasonNumber != nil {
		season = sql.NullInt64{Int64: int64(*a.SeasonNumber), Valid: true}
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO action_log(at, action, media_kind, arr_id, title, season_number,
		size_freed_bytes, delete_files, import_exclusion, dry_run, success, error, username)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, a.At, a.Action, a.MediaKind, a.ArrID, a.Title, season,
		a.SizeFreedBytes, a.DeleteFiles, a.ImportExclusion, a.DryRun, a.Success, nullString(a.Error), nullString(a.Username))
	return err
}

// Actions returns a page of action_log, newest first, and whether more exist.
func (s *Store) Actions(ctx context.Context, offset, limit int) ([]Action, bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, at, action, media_kind, COALESCE(arr_id, 0), title, season_number,
		size_freed_bytes, delete_files, import_exclusion, dry_run, success, COALESCE(error, ''), COALESCE(username, '')
		FROM action_log ORDER BY id DESC LIMIT ? OFFSET ?`, limit+1, offset)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = rows.Close() }()
	var out []Action
	for rows.Next() {
		var a Action
		var season sql.NullInt64
		if err := rows.Scan(&a.ID, &a.At, &a.Action, &a.MediaKind, &a.ArrID, &a.Title, &season, &a.SizeFreedBytes,
			&a.DeleteFiles, &a.ImportExclusion, &a.DryRun, &a.Success, &a.Error, &a.Username); err != nil {
			return nil, false, err
		}
		if season.Valid {
			n := int(season.Int64)
			a.SeasonNumber = &n
		}
		out = append(out, a)
	}
	more := len(out) > limit
	if more {
		out = out[:limit]
	}
	return out, more, rows.Err()
}

// AuthEventsPage returns a page of auth_events, newest first, and whether more exist.
func (s *Store) AuthEventsPage(ctx context.Context, offset, limit int) ([]AuthEvent, bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT at, user_id, COALESCE(username, ''), event, COALESCE(ip, ''),
		COALESCE(user_agent, '') FROM auth_events ORDER BY id DESC LIMIT ? OFFSET ?`, limit+1, offset)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = rows.Close() }()
	var out []AuthEvent
	for rows.Next() {
		var e AuthEvent
		var uid sql.NullInt64
		if err := rows.Scan(&e.At, &uid, &e.Username, &e.Event, &e.IP, &e.UserAgent); err != nil {
			return nil, false, err
		}
		e.UserID = ptrInt(uid)
		out = append(out, e)
	}
	more := len(out) > limit
	if more {
		out = out[:limit]
	}
	return out, more, rows.Err()
}

// PlexMachineID returns the stored Plex server identifier ("" if unknown).
func (s *Store) PlexMachineID(ctx context.Context) string {
	v, _ := s.Setting(ctx, SettingPlexMachineID, "")
	return v
}

// SettingPlexMachineID stores the Plex server identifier used for links.
const SettingPlexMachineID = "plex_machine_id"
