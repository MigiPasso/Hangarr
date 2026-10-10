package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"hangarr/internal/matching"
)

// LibraryFilter holds the library filters (SPEC §7.2). Zero values mean "no
// filter". Every value reaches SQL as a bound parameter; only whitelisted
// fragments are concatenated.
type LibraryFilter struct {
	Kind   string   // movie | series (required)
	Query  string   // free text, accent/case-insensitive
	States []string // never | in_progress | completed | abandoned
	// InactiveDays: not watched for N days or more; never-watched items count
	// from the date they were added (same rule as the cleanup page).
	InactiveDays int
	Added        string   // lt30 | gt180 | gt365
	Genres       []string // any of
	YearMin      int
	YearMax      int
	RatingMin    float64
	Tags         []string // any of
	Profile      string
	Network      string // network (series) or studio (movies)
	Status       string // continuing | ended (series)
	SizeMinGB    float64
	SizeMaxGB    float64
	Match        []string // matched | arr_only | server_only
	// User: "who?": states, progress, plays and last view become this
	// user's (0: everyone).
	User   int64
	Sort   string // title | last | added | size | plays | rating | inactive
	Desc   bool
	Offset int
	Limit  int
}

// LibraryItem is one card / table row.
type LibraryItem struct {
	ID                 int64
	ArrID              int64 // 0 for server_only
	PlexRatingKey      string
	JellyfinID         string
	Kind               string
	Title              string
	Year               int
	State              string
	Plays              int
	Viewers            int
	WatchedEpisodes    int
	DownloadedEpisodes int
	TotalEpisodes      int
	LastViewedAt       *int64
	LastViewedBy       string
	SizeBytes          int64
	AddedAt            *int64
	Rating             float64
	Tags               []string
	MatchState         string
	Status             string
	ProtectedLocal     bool // protected in Hangarr only (server_only items)
}

// SortKeys maps sort names to their ORDER BY expression (whitelist).
var sortKeys = map[string]string{
	"title":  "v.title_norm",
	"last":   "v.eff_last",
	"added":  "v.added_at",
	"size":   "v.size_bytes",
	"plays":  "v.eff_plays",
	"rating": "v.rating",
	// Ascending: most recently active first, like a duration of inactivity.
	"inactive": "(0 - COALESCE(v.eff_last, v.added_at, 0))",
}

// ValidSort reports whether name is a known sort key.
func ValidSort(name string) bool { _, ok := sortKeys[name]; return ok }

const day = 24 * 60 * 60

type sqlBuilder struct {
	sb   strings.Builder
	args []any
}

func (b *sqlBuilder) add(fragment string, args ...any) {
	b.sb.WriteString(fragment)
	b.args = append(b.args, args...)
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

func anyStrings(v []string) []any {
	out := make([]any, len(v))
	for i := range v {
		out[i] = v[i]
	}
	return out
}

// escapeLike escapes LIKE wildcards; queries use ESCAPE '\'.
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// libraryView writes the CTE "v": media rows of one kind with effective
// columns (eff_state, eff_plays, eff_last, eff_last_by, eff_watched), either
// the stored all-users values or, with a perspective user, values computed
// on the fly from that user's sessions (same rules as package scoring).
// Items only the inactive media server knows (hidden) are left out.
func libraryView(b *sqlBuilder, f LibraryFilter, now time.Time, abandonDays int) {
	if f.User == 0 {
		b.add(`WITH v AS (SELECT m.*, m.watch_state AS eff_state, m.play_count AS eff_plays,
			m.last_viewed_at AS eff_last, m.last_viewed_by AS eff_last_by,
			COALESCE(m.watched_episodes, 0) AS eff_watched FROM media m
			WHERE m.kind = ? AND m.match_state <> 'hidden')`, f.Kind)
		return
	}
	recent := now.Unix() - int64(abandonDays)*day
	// ev reads the user's plays once, and only those of the listed kind
	// (the costly part, see BenchmarkLibraryView); up and ue come from it.
	b.add(`WITH ev AS MATERIALIZED (
			SELECT media_id, `+playKey+` AS pk, watched, stopped_at, media_type, season_number, episode_number
			FROM watch_events WHERE user_id = ? AND media_id IN (SELECT id FROM media WHERE kind = ?)),
		up AS (
			SELECT media_id, COUNT(DISTINCT pk) AS plays, COUNT(DISTINCT CASE WHEN watched = 1 THEN pk END) AS completed,
			  MAX(stopped_at) AS last
			FROM ev GROUP BY media_id),
		ue AS (
			SELECT media_id, season_number, COUNT(DISTINCT episode_number) AS w FROM ev
			WHERE watched = 1 AND media_type = 'episode' AND season_number > 0
			GROUP BY media_id, season_number),
		uw AS (
			SELECT ue.media_id, SUM(ue.w) AS raw,
			  SUM(CASE WHEN COALESCE(s.episode_file_count, 0) > 0 AND ue.w > s.episode_file_count
			      THEN s.episode_file_count ELSE ue.w END) AS capped
			FROM ue LEFT JOIN seasons s ON s.media_id = ue.media_id AND s.season_number = ue.season_number
			GROUP BY ue.media_id),
		v AS (
			SELECT m.*, COALESCE(up.plays, 0) AS eff_plays, up.last AS eff_last,
			  CASE WHEN up.media_id IS NOT NULL THEN ? END AS eff_last_by, COALESCE(uw.capped, 0) AS eff_watched,
			  CASE WHEN m.kind = 'movie' THEN
			    CASE WHEN COALESCE(up.completed, 0) > 0 THEN 'completed'
			         WHEN COALESCE(up.plays, 0) = 0 THEN 'never'
			         WHEN up.last >= ? THEN 'in_progress' ELSE 'abandoned' END
			  ELSE
			    CASE WHEN COALESCE(uw.raw, 0) = 0 THEN 'never'
			         WHEN NOT EXISTS (SELECT 1 FROM seasons s WHERE s.media_id = m.id AND s.season_number > 0
			              AND s.episode_file_count > 0
			              AND COALESCE((SELECT ue.w FROM ue WHERE ue.media_id = s.media_id
			                            AND ue.season_number = s.season_number), 0) < s.episode_file_count)
			           THEN 'completed'
			         WHEN (SELECT COALESCE((SELECT ue.w FROM ue WHERE ue.media_id = s.media_id
			                 AND ue.season_number = s.season_number), 0) >= s.episode_file_count
			               FROM seasons s WHERE s.media_id = m.id AND s.season_number > 0 AND s.episode_file_count > 0
			               ORDER BY s.season_number DESC LIMIT 1) = 1
			           THEN 'completed' -- last season finished (scoring.WatchState)
			         WHEN up.last >= ? THEN 'in_progress' ELSE 'abandoned' END
			  END AS eff_state
			FROM media m LEFT JOIN up ON up.media_id = m.id LEFT JOIN uw ON uw.media_id = m.id
			WHERE m.kind = ? AND m.match_state <> 'hidden')`,
		f.User, f.Kind, f.User, recent, recent, f.Kind)
}

// libraryWhere writes the WHERE clause (always starts with "WHERE 1 = 1").
func libraryWhere(b *sqlBuilder, f LibraryFilter, now time.Time) {
	b.add(` WHERE 1 = 1`)
	if q := matching.NormalizeTitle(f.Query); q != "" {
		b.add(` AND v.title_norm LIKE ? ESCAPE '\'`, "%"+escapeLike(q)+"%")
	}
	if len(f.States) > 0 {
		b.add(` AND v.eff_state IN (`+placeholders(len(f.States))+`)`, anyStrings(f.States)...)
	}
	if f.InactiveDays > 0 {
		b.add(` AND COALESCE(v.eff_last, v.added_at, 0) < ?`, now.Unix()-int64(f.InactiveDays)*day)
	}
	switch f.Added {
	case "lt30":
		b.add(` AND v.added_at >= ?`, now.Unix()-30*day)
	case "gt180":
		b.add(` AND v.added_at < ?`, now.Unix()-180*day)
	case "gt365":
		b.add(` AND v.added_at < ?`, now.Unix()-365*day)
	}
	if len(f.Genres) > 0 {
		b.add(` AND EXISTS (SELECT 1 FROM json_each(v.genres) j WHERE j.value IN (`+placeholders(len(f.Genres))+`))`,
			anyStrings(f.Genres)...)
	}
	if len(f.Tags) > 0 {
		b.add(` AND EXISTS (SELECT 1 FROM json_each(v.tags) j WHERE j.value IN (`+placeholders(len(f.Tags))+`))`,
			anyStrings(f.Tags)...)
	}
	if f.YearMin > 0 {
		b.add(` AND v.year >= ?`, f.YearMin)
	}
	if f.YearMax > 0 {
		b.add(` AND v.year <= ?`, f.YearMax)
	}
	if f.RatingMin > 0 {
		b.add(` AND v.rating >= ?`, f.RatingMin)
	}
	if f.Profile != "" {
		b.add(` AND v.quality_profile = ?`, f.Profile)
	}
	if f.Network != "" {
		b.add(` AND v.network_or_studio = ?`, f.Network)
	}
	if f.Status != "" {
		b.add(` AND v.status = ?`, f.Status)
	}
	if f.SizeMinGB > 0 {
		b.add(` AND v.size_bytes >= ?`, int64(f.SizeMinGB*1e9))
	}
	if f.SizeMaxGB > 0 {
		b.add(` AND v.size_bytes <= ?`, int64(f.SizeMaxGB*1e9))
	}
	if len(f.Match) > 0 {
		b.add(` AND v.match_state IN (`+placeholders(len(f.Match))+`)`, anyStrings(f.Match)...)
	}
}

// LibraryQuery builds the paginated listing query (exported for tests). Its
// last two columns are the number and total size of all the matching items
// (window functions): the perspective of a user is costly to compute, the
// count must not compute it a second time.
func LibraryQuery(f LibraryFilter, now time.Time, abandonDays int) (string, []any) {
	b := &sqlBuilder{}
	libraryView(b, f, now, abandonDays)
	b.add(` SELECT v.id, COALESCE(v.arr_id, 0), COALESCE(v.plex_rating_key, ''), COALESCE(v.jellyfin_id, ''), v.kind, v.title, COALESCE(v.year, 0), v.eff_state, v.eff_plays,
		v.distinct_viewers, v.eff_watched, COALESCE(v.downloaded_episodes, 0), COALESCE(v.total_episodes, 0),
		v.eff_last, COALESCE(NULLIF(a.friendly_name, ''), a.username, ''), v.size_bytes, v.added_at,
		COALESCE(v.rating, 0), v.tags, v.match_state, COALESCE(v.status, ''), v.protected_local,
		COUNT(*) OVER (), COALESCE(SUM(v.size_bytes) OVER (), 0)
		FROM v LEFT JOIN plex_accounts a ON a.account_id = v.eff_last_by`)
	libraryWhere(b, f, now)
	col, ok := sortKeys[f.Sort]
	if !ok {
		col = sortKeys["title"]
	}
	dir := " ASC"
	if f.Desc {
		dir = " DESC"
	}
	b.add(` ORDER BY ` + col + dir + ` NULLS LAST, v.title_norm ASC, v.id ASC`)
	limit := f.Limit
	if limit <= 0 {
		limit = 48
	}
	b.add(` LIMIT ? OFFSET ?`, limit, max(f.Offset, 0))
	return b.sb.String(), b.args
}

// LibraryPage is one page of the library and the totals of the filter.
type LibraryPage struct {
	Items []LibraryItem
	// Count and SizeBytes cover every matching item, not only this page
	// (0 when the page is past the end).
	Count     int
	SizeBytes int64
}

// Library returns one page of items.
func (s *Store) Library(ctx context.Context, f LibraryFilter, now time.Time, abandonDays int) ([]LibraryItem, error) {
	p, err := s.LibraryPage(ctx, f, now, abandonDays)
	if err != nil {
		return nil, err
	}
	return p.Items, nil
}

// LibraryPage returns one page of items with the number and total size of
// the matching items, in a single query.
func (s *Store) LibraryPage(ctx context.Context, f LibraryFilter, now time.Time, abandonDays int) (*LibraryPage, error) {
	query, args := LibraryQuery(f, now, abandonDays)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	p := &LibraryPage{}
	for rows.Next() {
		var it LibraryItem
		var last, added sql.NullInt64
		var tags string
		if err := rows.Scan(&it.ID, &it.ArrID, &it.PlexRatingKey, &it.JellyfinID, &it.Kind, &it.Title, &it.Year, &it.State, &it.Plays, &it.Viewers,
			&it.WatchedEpisodes, &it.DownloadedEpisodes, &it.TotalEpisodes, &last, &it.LastViewedBy, &it.SizeBytes,
			&added, &it.Rating, &tags, &it.MatchState, &it.Status, &it.ProtectedLocal, &p.Count, &p.SizeBytes); err != nil {
			return nil, err
		}
		it.LastViewedAt, it.AddedAt = ptrInt(last), ptrInt(added)
		_ = json.Unmarshal([]byte(tags), &it.Tags)
		p.Items = append(p.Items, it)
	}
	return p, rows.Err()
}

// Account is a Plex user for filters and labels.
type Account struct {
	ID   int64
	Name string
}

// LibraryFacets are the values offered by the filter form.
type LibraryFacets struct {
	Genres   []string
	Tags     []string
	Profiles []string
	Networks []string
	Users    []Account
}

func (s *Store) strings(ctx context.Context, query string, args ...any) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Facets lists distinct genres, tags, profiles, networks and users.
func (s *Store) Facets(ctx context.Context, kind string) (*LibraryFacets, error) {
	f := &LibraryFacets{}
	var err error
	if f.Genres, err = s.strings(ctx, `SELECT DISTINCT j.value FROM media, json_each(media.genres) j
		WHERE media.kind = ? ORDER BY 1`, kind); err != nil {
		return nil, err
	}
	if f.Tags, err = s.strings(ctx, `SELECT DISTINCT j.value FROM media, json_each(media.tags) j
		WHERE media.kind = ? ORDER BY 1`, kind); err != nil {
		return nil, err
	}
	if f.Profiles, err = s.strings(ctx, `SELECT DISTINCT quality_profile FROM media
		WHERE kind = ? AND quality_profile IS NOT NULL ORDER BY 1`, kind); err != nil {
		return nil, err
	}
	if f.Networks, err = s.strings(ctx, `SELECT DISTINCT network_or_studio FROM media
		WHERE kind = ? AND network_or_studio IS NOT NULL AND network_or_studio <> '' ORDER BY 1`, kind); err != nil {
		return nil, err
	}
	f.Users, err = s.Accounts(ctx)
	return f, err
}

// Accounts lists Plex users by display name.
func (s *Store) Accounts(ctx context.Context) ([]Account, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT account_id, COALESCE(NULLIF(friendly_name, ''), username, '')
		FROM plex_accounts ORDER BY is_active DESC, 2 COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Account
	for rows.Next() {
		var a Account
		if err := rows.Scan(&a.ID, &a.Name); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// LastSyncAt returns the end of the most recent full sync, i.e. of a catalog
// source: history-only syncs (Tautulli webhook) do not count, so that they
// never hide a stale catalog from the startup catch-up. Interrupted runs do
// not count either.
func (s *Store) LastSyncAt(ctx context.Context) (*int64, error) {
	var v sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT MAX(finished_at) FROM sync_runs
		WHERE source IN ('sonarr', 'radarr', 'plex', 'jellyfin') AND status <> 'interrupted'`).Scan(&v)
	return ptrInt(v), err
}

// ServerItemTitle returns the title of the item a media server key (Plex
// rating key, Jellyfin id) belongs to; ok is false when no item has it.
func (s *Store) ServerItemTitle(ctx context.Context, server, key string) (title string, ok bool, err error) {
	col, known := serverColumns[server]
	if !known || key == "" {
		return "", false, nil
	}
	//nolint:gosec // G202: col comes from the serverColumns whitelist
	err = s.db.QueryRowContext(ctx, `SELECT title FROM media WHERE `+col+` = ? LIMIT 1`, key).Scan(&title)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return title, err == nil, err
}

// ImageInfo returns the title and remote image URL of a Sonarr/Radarr item;
// ok is false when no such item exists (media server items: ServerItemTitle).
func (s *Store) ImageInfo(ctx context.Context, kind string, arrID int64, variant string) (title, remote string, ok bool, err error) {
	col := "poster_remote_url"
	if variant == "fanart" {
		col = "fanart_remote_url"
	}
	err = s.db.QueryRowContext(ctx, `SELECT title, COALESCE(`+col+`, '') FROM media WHERE kind = ? AND arr_id = ?`,
		kind, arrID).Scan(&title, &remote)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	return title, remote, err == nil, err
}
