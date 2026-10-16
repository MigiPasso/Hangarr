package store

import (
	"hangarr/internal/media"

	"context"
	"database/sql"
	"encoding/json"
	"strings"
)

// Aggregates for the dashboard, statistics and cleanup pages. Time windows
// are expressed as "since" Unix timestamps (0 = all time); user 0 means all
// users. Every value is a bound parameter.

func userClause(alias string, user int64) (string, []any) {
	if user == 0 {
		return "", nil
	}
	return " AND " + alias + ".user_id = ?", []any{user}
}

// HasProtectedTag reports whether labels contain one of the protection tags
// (PROTECTED_TAGS), ignoring case and surrounding spaces. It is the rule of
// every check in Go (badges, batch selection, deletion); protectedClause is
// its SQL twin (TestProtectedTagRuleGoAndSQLAgree).
func HasProtectedTag(labels, protected []string) bool {
	for _, l := range labels {
		for _, p := range protected {
			if strings.EqualFold(strings.TrimSpace(l), strings.TrimSpace(p)) {
				return true
			}
		}
	}
	return false
}

// protectedClause excludes media carrying one of the protection tags, or
// protected in Hangarr only. SQLite's lower() only folds ASCII letters:
// *arr tag labels are lowercase anyway.
func protectedClause(alias string, tags []string) (string, []any) {
	local := " AND NOT " + alias + ".protected_local"
	if len(tags) == 0 {
		return local, nil
	}
	args := make([]any, len(tags))
	for i, t := range tags {
		args[i] = strings.ToLower(strings.TrimSpace(t))
	}
	return local + " AND NOT EXISTS (SELECT 1 FROM json_each(" + alias + ".tags) j WHERE lower(trim(j.value)) IN (" +
		placeholders(len(tags)) + "))", args
}

// Placeholder labels, translated by the web layer.
const (
	NoGenre = "(none)"    // item without genre
	Unknown = "(unknown)" // empty player / platform / transcode decision
)

// Overview are the dashboard tiles.
type Overview struct {
	Series            int
	Movies            int
	TotalBytes        int64
	NeverWatchedBytes int64
	RecoverableBytes  int64
	RecoverableCount  int
	WatchSeconds30d   int64
}

// CleanupScope selects cleanup candidates: Sonarr/Radarr items with files,
// not protected, that nobody watched since Cutoff (never-watched items count
// from the date they were added; an unknown date counts as old).
type CleanupScope struct {
	ProtectedTags []string
	Cutoff        int64 // unix time: last activity must be older
}

// InactiveSince is the SQL expression of an item's last activity: its last
// view, or the date it was added when never watched.
func inactiveSince(alias string) string {
	return `COALESCE(` + alias + `.last_viewed_at, ` + alias + `.added_at, 0)`
}

func (c CleanupScope) base(alias string) (string, []any) {
	prot, args := protectedClause(alias, c.ProtectedTags)
	return alias + `.arr_id IS NOT NULL AND ` + alias + `.size_bytes > 0` + prot, args
}

func (c CleanupScope) where(alias string) (string, []any) {
	where, args := c.base(alias)
	return where + ` AND ` + inactiveSince(alias) + ` < ?`, append(args, c.Cutoff)
}

// Overview computes the dashboard tiles.
func (s *Store) Overview(ctx context.Context, scope CleanupScope, since30d int64) (*Overview, error) {
	o := &Overview{}
	err := s.db.QueryRowContext(ctx, `SELECT
		COUNT(CASE WHEN kind = 'series' THEN 1 END), COUNT(CASE WHEN kind = 'movie' THEN 1 END),
		COALESCE(SUM(size_bytes), 0), COALESCE(SUM(CASE WHEN watch_state = 'never' THEN size_bytes END), 0)
		FROM media WHERE arr_id IS NOT NULL`).Scan(&o.Series, &o.Movies, &o.TotalBytes, &o.NeverWatchedBytes)
	if err != nil {
		return nil, err
	}
	where, args := scope.where("m")
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(m.size_bytes), 0) FROM media m
		WHERE `+where, args...).Scan(&o.RecoverableCount, &o.RecoverableBytes)
	if err != nil {
		return nil, err
	}
	err = s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(play_seconds), 0) FROM watch_events WHERE stopped_at >= ?`,
		since30d).Scan(&o.WatchSeconds30d)
	return o, err
}

// TopItem is a ranked media.
type TopItem struct {
	MediaID      int64
	ArrID        int64
	Kind         string
	Title        string
	Year         int
	Plays        int
	WatchSeconds int64
	Viewers      int
	SizeBytes    int64
}

// TopMedia ranks media of a kind by plays or watch time over a window.
func (s *Store) TopMedia(ctx context.Context, kind string, since, user int64, byHours bool, limit int) ([]TopItem, error) {
	uc, uargs := userClause("w", user)
	order := "plays DESC, secs DESC"
	if byHours {
		order = "secs DESC, plays DESC"
	}
	args := append([]any{kind, since}, uargs...)
	args = append(args, limit)
	//nolint:gosec // G202: only whitelisted fragments (order, user clause) are concatenated
	rows, err := s.db.QueryContext(ctx, `SELECT m.id, COALESCE(m.arr_id, 0), m.kind, m.title, COALESCE(m.year, 0),
		COUNT(DISTINCT `+playKeyW+`) AS plays, COALESCE(SUM(w.play_seconds), 0) AS secs, COUNT(DISTINCT w.user_id),
		m.size_bytes
		FROM watch_events w JOIN media m ON m.id = w.media_id
		WHERE m.kind = ? AND w.stopped_at >= ?`+uc+`
		GROUP BY m.id ORDER BY `+order+`, m.title_norm LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []TopItem
	for rows.Next() {
		var t TopItem
		if err := rows.Scan(&t.MediaID, &t.ArrID, &t.Kind, &t.Title, &t.Year, &t.Plays, &t.WatchSeconds, &t.Viewers,
			&t.SizeBytes); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// playKeyW is playKey for the "w" alias.
const playKeyW = `COALESCE('r' || w.reference_id, 'e' || w.id)`

// RecentView is one of the last sessions.
type RecentView struct {
	MediaID    int64 // 0 when the media no longer exists
	ArrID      int64
	PlexKey    string // Plex rating key of the media (poster of server_only items)
	JellyfinID string // Jellyfin id of the media (same)
	Kind       string
	Title      string
	Episode    string // "S01E02 · Title" for episodes
	UserName   string
	StoppedAt  int64
}

// RecentViews returns the last plays (resumed sessions grouped), newest first,
// looking back 90 days from the latest session.
func (s *Store) RecentViews(ctx context.Context, limit int) ([]RecentView, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT COALESCE(m.id, 0), COALESCE(m.arr_id, 0), COALESCE(m.plex_rating_key, ''),
		COALESCE(m.jellyfin_id, ''), COALESCE(m.kind, ''),
		COALESCE(m.title, NULLIF(w.grandparent_title, ''), w.title, ''), w.media_type, COALESCE(w.season_number, 0),
		COALESCE(w.episode_number, 0), COALESCE(w.title, ''), COALESCE(NULLIF(a.friendly_name, ''), a.username, ''),
		COALESCE(w.stopped_at, 0)
		FROM (SELECT x.*, ROW_NUMBER() OVER (PARTITION BY COALESCE('r' || x.reference_id, 'e' || x.id)
		        ORDER BY x.stopped_at DESC, x.id DESC) AS rn
		      FROM watch_events x
		      WHERE x.stopped_at >= (SELECT COALESCE(MAX(stopped_at), 0) FROM watch_events) - 90 * 86400) w
		LEFT JOIN media m ON m.id = w.media_id
		LEFT JOIN plex_accounts a ON a.account_id = w.user_id
		WHERE w.rn = 1 ORDER BY w.stopped_at DESC, w.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []RecentView
	for rows.Next() {
		var r RecentView
		var mediaType, epTitle string
		var season, episode int
		if err := rows.Scan(&r.MediaID, &r.ArrID, &r.PlexKey, &r.JellyfinID, &r.Kind, &r.Title, &mediaType, &season, &episode, &epTitle,
			&r.UserName, &r.StoppedAt); err != nil {
			return nil, err
		}
		if mediaType == "episode" {
			r.Episode = media.EpisodeCode(season, episode) + " · " + epTitle
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PlaySession is a minimal watch event, for time bucketing done in Go (time
// zone aware).
type PlaySession struct {
	UserID      int64
	StartedAt   int64
	StoppedAt   int64
	PlaySeconds int64
}

// Sessions returns sessions stopped since a date, optionally for one user.
func (s *Store) Sessions(ctx context.Context, since, user int64) ([]PlaySession, error) {
	uc, uargs := userClause("w", user)
	//nolint:gosec // G202: only the whitelisted user clause is concatenated
	rows, err := s.db.QueryContext(ctx, `SELECT w.user_id, COALESCE(w.started_at, 0), COALESCE(w.stopped_at, 0),
		COALESCE(w.play_seconds, 0) FROM watch_events w WHERE w.stopped_at >= ?`+uc, append([]any{since}, uargs...)...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []PlaySession
	for rows.Next() {
		var x PlaySession
		if err := rows.Scan(&x.UserID, &x.StartedAt, &x.StoppedAt, &x.PlaySeconds); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// CostItem is storage per play.
type CostItem struct {
	TopItem
	BytesPerPlay int64
}

// CostPerPlay ranks Sonarr/Radarr items by stored bytes per play (all
// time, at least one play): the most "expensive" first.
func (s *Store) CostPerPlay(ctx context.Context, limit int) ([]CostItem, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, arr_id, kind, title, COALESCE(year, 0), play_count, watch_seconds,
		distinct_viewers, size_bytes, size_bytes / play_count FROM media
		WHERE arr_id IS NOT NULL AND play_count > 0 AND size_bytes > 0
		ORDER BY size_bytes * 1.0 / play_count DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []CostItem
	for rows.Next() {
		var c CostItem
		if err := rows.Scan(&c.MediaID, &c.ArrID, &c.Kind, &c.Title, &c.Year, &c.Plays, &c.WatchSeconds, &c.Viewers,
			&c.SizeBytes, &c.BytesPerPlay); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GenreShare is a genre's share of storage and of plays. A title with n
// genres counts 1/n in each, so shares sum to 100 %.
type GenreShare struct {
	Genre       string
	StoredBytes float64
	Plays       float64
	StoredShare float64 // 0..1
	PlaysShare  float64 // 0..1
}

// GenreShares compares stored space and plays per genre.
func (s *Store) GenreShares(ctx context.Context, since, user int64) ([]GenreShare, error) {
	byGenre := map[string]*GenreShare{}
	get := func(g string) *GenreShare {
		if byGenre[g] == nil {
			byGenre[g] = &GenreShare{Genre: g}
		}
		return byGenre[g]
	}
	var totalBytes, totalPlays float64
	err := s.queryEach(ctx, `SELECT genres, size_bytes FROM media WHERE arr_id IS NOT NULL AND size_bytes > 0`,
		func(rows *sql.Rows) error {
			var raw string
			var size int64
			if err := rows.Scan(&raw, &size); err != nil {
				return err
			}
			var genres []string
			if json.Unmarshal([]byte(raw), &genres) != nil || len(genres) == 0 {
				genres = []string{NoGenre}
			}
			for _, g := range genres {
				get(g).StoredBytes += float64(size) / float64(len(genres))
			}
			totalBytes += float64(size)
			return nil
		})
	if err != nil {
		return nil, err
	}
	uc, uargs := userClause("w", user)
	err = s.queryEach(ctx, `SELECT m.genres, COUNT(DISTINCT `+playKeyW+`) FROM watch_events w
		JOIN media m ON m.id = w.media_id WHERE w.stopped_at >= ?`+uc+` GROUP BY m.id`, func(rows *sql.Rows) error {
		var raw string
		var plays int
		if err := rows.Scan(&raw, &plays); err != nil {
			return err
		}
		var genres []string
		if json.Unmarshal([]byte(raw), &genres) != nil || len(genres) == 0 {
			genres = []string{NoGenre}
		}
		for _, g := range genres {
			get(g).Plays += float64(plays) / float64(len(genres))
		}
		totalPlays += float64(plays)
		return nil
	}, append([]any{since}, uargs...)...)
	if err != nil {
		return nil, err
	}
	out := make([]GenreShare, 0, len(byGenre))
	for _, g := range byGenre {
		if totalBytes > 0 {
			g.StoredShare = g.StoredBytes / totalBytes
		}
		if totalPlays > 0 {
			g.PlaysShare = g.Plays / totalPlays
		}
		out = append(out, *g)
	}
	return out, nil
}

// UserRank is a user's activity over a window.
type UserRank struct {
	UserID       int64
	Name         string
	WatchSeconds int64
	Plays        int
	Titles       int
	LastAt       int64
}

// UserRanking ranks users by watch time.
func (s *Store) UserRanking(ctx context.Context, since int64) ([]UserRank, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT w.user_id, COALESCE(NULLIF(a.friendly_name, ''), a.username, ''),
		COALESCE(SUM(w.play_seconds), 0), COUNT(DISTINCT `+playKeyW+`),
		COUNT(DISTINCT COALESCE('m' || w.media_id, 't' || COALESCE(w.grandparent_title, w.title))), MAX(w.stopped_at)
		FROM watch_events w LEFT JOIN plex_accounts a ON a.account_id = w.user_id
		WHERE w.stopped_at >= ? GROUP BY w.user_id ORDER BY 3 DESC`, since)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []UserRank
	for rows.Next() {
		var u UserRank
		if err := rows.Scan(&u.UserID, &u.Name, &u.WatchSeconds, &u.Plays, &u.Titles, &u.LastAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ViewerOfSeries is one user's progress on a series.
type ViewerOfSeries struct {
	UserID  int64
	Name    string
	Watched int // distinct episodes with a complete play
	LastAt  int64
}

// SeriesViewers is a stored series and who watches it.
type SeriesViewers struct {
	TopItem
	State      string
	Downloaded int
	Viewers    []ViewerOfSeries
}

// WhoWatchesWhat lists the largest series and, for each, its viewers.
func (s *Store) WhoWatchesWhat(ctx context.Context, limit int) ([]SeriesViewers, error) {
	var out []SeriesViewers
	idx := map[int64]int{}
	err := s.queryEach(ctx, `SELECT id, COALESCE(arr_id, 0), title, COALESCE(year, 0), size_bytes, watch_state,
		COALESCE(downloaded_episodes, 0) FROM media WHERE kind = 'series' AND arr_id IS NOT NULL AND size_bytes > 0
		ORDER BY size_bytes DESC LIMIT ?`, func(rows *sql.Rows) error {
		var sv SeriesViewers
		sv.Kind = "series"
		if err := rows.Scan(&sv.MediaID, &sv.ArrID, &sv.Title, &sv.Year, &sv.SizeBytes, &sv.State, &sv.Downloaded); err != nil {
			return err
		}
		idx[sv.MediaID] = len(out)
		out = append(out, sv)
		return nil
	}, limit)
	if err != nil || len(out) == 0 {
		return out, err
	}
	ids := make([]int64, 0, len(out))
	for _, sv := range out {
		ids = append(ids, sv.MediaID)
	}
	err = s.queryEach(ctx, `SELECT w.media_id, w.user_id, COALESCE(NULLIF(a.friendly_name, ''), a.username, ''),
		COUNT(DISTINCT CASE WHEN w.watched = 1 AND w.season_number > 0 THEN w.season_number * 10000 + w.episode_number END),
		MAX(w.stopped_at)
		FROM watch_events w LEFT JOIN plex_accounts a ON a.account_id = w.user_id
		WHERE w.media_id IN (SELECT value FROM json_each(?)) GROUP BY w.media_id, w.user_id ORDER BY 5 DESC`,
		func(rows *sql.Rows) error {
			var mediaID int64
			var v ViewerOfSeries
			if err := rows.Scan(&mediaID, &v.UserID, &v.Name, &v.Watched, &v.LastAt); err != nil {
				return err
			}
			out[idx[mediaID]].Viewers = append(out[idx[mediaID]].Viewers, v)
			return nil
		}, idsJSON(ids))
	return out, err
}

// Completion summarizes movies started in a window.
type Completion struct {
	Started    int
	Completed  int
	Unfinished []TopItem // started, never completed, most recent first
}

// MovieCompletion computes how many started movies were finished.
func (s *Store) MovieCompletion(ctx context.Context, since, user int64, limit int) (*Completion, error) {
	uc, uargs := userClause("w", user)
	c := &Completion{}
	err := s.queryEach(ctx, `SELECT m.id, COALESCE(m.arr_id, 0), m.title, COALESCE(m.year, 0),
		COUNT(DISTINCT `+playKeyW+`), MAX(w.watched), MAX(w.stopped_at), MAX(w.percent_complete)
		FROM watch_events w JOIN media m ON m.id = w.media_id
		WHERE m.kind = 'movie' AND w.stopped_at >= ?`+uc+` GROUP BY m.id ORDER BY 7 DESC`, func(rows *sql.Rows) error {
		var t TopItem
		var watched bool
		var last int64
		var pct int
		if err := rows.Scan(&t.MediaID, &t.ArrID, &t.Title, &t.Year, &t.Plays, &watched, &last, &pct); err != nil {
			return err
		}
		t.Kind = "movie"
		c.Started++
		if watched {
			c.Completed++
		} else if len(c.Unfinished) < limit {
			t.Viewers = pct // reused as "max % reached"
			c.Unfinished = append(c.Unfinished, t)
		}
		return nil
	}, append([]any{since}, uargs...)...)
	return c, err
}

// Count is a labeled count.
type Count struct {
	Label string
	N     int
}

// Breakdown counts sessions by a column (transcode_decision, platform,
// player) over a window, most frequent first.
func (s *Store) Breakdown(ctx context.Context, column string, since, user int64, limit int) ([]Count, error) {
	allowed := map[string]bool{"transcode_decision": true, "platform": true, "player": true}
	if !allowed[column] {
		return nil, nil
	}
	uc, uargs := userClause("w", user)
	args := append([]any{since}, uargs...)
	//nolint:gosec // G202: column is checked against a whitelist above
	rows, err := s.db.QueryContext(ctx, `SELECT COALESCE(NULLIF(w.`+column+`, ''), '`+Unknown+`'), COUNT(*)
		FROM watch_events w WHERE w.stopped_at >= ?`+uc+` GROUP BY 1 ORDER BY 2 DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Count
	for rows.Next() {
		var c Count
		if err := rows.Scan(&c.Label, &c.N); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// LastEpisodes returns, for one user, the last completely watched episode
// of each series (media id → season, episode).
func (s *Store) LastEpisodes(ctx context.Context, user int64) (map[int64][2]int, error) {
	out := map[int64][2]int{}
	err := s.queryEach(ctx, `SELECT media_id, season_number, episode_number FROM (
		SELECT media_id, season_number, episode_number,
		  ROW_NUMBER() OVER (PARTITION BY media_id ORDER BY stopped_at DESC, id DESC) AS rn
		FROM watch_events WHERE user_id = ? AND watched = 1 AND media_type = 'episode' AND media_id IS NOT NULL
		  AND season_number > 0) WHERE rn = 1`, func(rows *sql.Rows) error {
		var id int64
		var se, ep int
		if err := rows.Scan(&id, &se, &ep); err != nil {
			return err
		}
		out[id] = [2]int{se, ep}
		return nil
	}, user)
	return out, err
}

// SeasonTotals returns stored episode counts per season of the given series.
func (s *Store) SeasonTotals(ctx context.Context, ids []int64) (map[int64]map[int]int, error) {
	out := map[int64]map[int]int{}
	err := s.queryEach(ctx, `SELECT media_id, season_number, total_episode_count FROM seasons
		WHERE media_id IN (SELECT value FROM json_each(?)) AND season_number > 0`, func(rows *sql.Rows) error {
		var id int64
		var n, total int
		if err := rows.Scan(&id, &n, &total); err != nil {
			return err
		}
		if out[id] == nil {
			out[id] = map[int]int{}
		}
		out[id][n] = total
		return nil
	}, idsJSON(ids))
	return out, err
}

// CleanupItem is a cleanup candidate (whole series or movie).
type CleanupItem struct {
	LibraryItem
	InactiveSince int64 // last view, or added date when never watched (0: unknown)
}

// cleanupSorts maps the cleanup sort keys to their ORDER BY expression
// (whitelist). "inactivity" ascending puts the most recently active first.
var cleanupSorts = map[string]string{
	"title":      "m.title_norm",
	"size":       "m.size_bytes",
	"inactivity": "(0 - " + inactiveSince("m") + ")",
	"state": `CASE m.watch_state WHEN 'never' THEN 0 WHEN 'abandoned' THEN 1 WHEN 'in_progress' THEN 2
		WHEN 'completed' THEN 3 ELSE 4 END`,
	"plays": "m.play_count",
}

// ValidCleanupSort reports whether name is a cleanup sort key.
func ValidCleanupSort(name string) bool { _, ok := cleanupSorts[name]; return ok }

// CleanupFilter narrows the cleanup candidates (zero value: all of them).
type CleanupFilter struct {
	Kind      string   // series | movie | "" (both)
	States    []string // watch states; empty: any
	PlaysMin  int
	PlaysMax  *int // nil: no maximum (0 is meaningful)
	SizeMinGB float64
	SizeMaxGB float64 // 0: no maximum
}

func (f CleanupFilter) where(alias string) (string, []any) {
	var b strings.Builder
	var args []any
	if f.Kind != "" {
		b.WriteString(" AND " + alias + ".kind = ?")
		args = append(args, f.Kind)
	}
	if len(f.States) > 0 {
		b.WriteString(" AND " + alias + ".watch_state IN (?" + strings.Repeat(", ?", len(f.States)-1) + ")")
		for _, st := range f.States {
			args = append(args, st)
		}
	}
	if f.PlaysMin > 0 {
		b.WriteString(" AND " + alias + ".play_count >= ?")
		args = append(args, f.PlaysMin)
	}
	if f.PlaysMax != nil {
		b.WriteString(" AND " + alias + ".play_count <= ?")
		args = append(args, *f.PlaysMax)
	}
	if f.SizeMinGB > 0 {
		b.WriteString(" AND " + alias + ".size_bytes >= ?")
		args = append(args, int64(f.SizeMinGB*1e9))
	}
	if f.SizeMaxGB > 0 {
		b.WriteString(" AND " + alias + ".size_bytes <= ?")
		args = append(args, int64(f.SizeMaxGB*1e9))
	}
	return b.String(), args
}

// CleanupCandidates lists the candidates matching f, sorted by sort (a
// cleanupSorts key; default: largest first), with their total size.
func (s *Store) CleanupCandidates(ctx context.Context, scope CleanupScope, f CleanupFilter, sort string, desc bool, limit int) ([]CleanupItem, int64, error) {
	where, args := scope.where("m")
	fw, fargs := f.where("m")
	where += fw
	args = append(args, fargs...)
	order, ok := cleanupSorts[sort]
	if !ok {
		order, desc = cleanupSorts["size"], true
	}
	if desc {
		order += " DESC"
	} else {
		order += " ASC"
	}
	var total int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(m.size_bytes), 0) FROM media m WHERE `+where,
		args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	//nolint:gosec // G202: the scope clause is built from bound parameters only
	rows, err := s.db.QueryContext(ctx, `SELECT m.id, m.arr_id, m.kind, m.title, COALESCE(m.year, 0), m.watch_state,
		m.play_count, m.distinct_viewers, COALESCE(m.watched_episodes, 0), COALESCE(m.downloaded_episodes, 0),
		m.last_viewed_at, COALESCE(NULLIF(a.friendly_name, ''), a.username, ''), m.size_bytes, m.added_at,
		`+inactiveSince("m")+`, m.tags, COALESCE(m.status, ''), m.protected_local
		FROM media m LEFT JOIN plex_accounts a ON a.account_id = m.last_viewed_by
		WHERE `+where+` ORDER BY `+order+`, m.size_bytes DESC, m.id LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	var out []CleanupItem
	for rows.Next() {
		var c CleanupItem
		var last, added sql.NullInt64
		var tags string
		if err := rows.Scan(&c.ID, &c.ArrID, &c.Kind, &c.Title, &c.Year, &c.State, &c.Plays, &c.Viewers,
			&c.WatchedEpisodes, &c.DownloadedEpisodes, &last, &c.LastViewedBy, &c.SizeBytes, &added, &c.InactiveSince, &tags,
			&c.Status, &c.ProtectedLocal); err != nil {
			return nil, 0, err
		}
		c.LastViewedAt, c.AddedAt = ptrInt(last), ptrInt(added)
		_ = json.Unmarshal([]byte(tags), &c.Tags)
		out = append(out, c)
	}
	return out, total, rows.Err()
}

// UnwatchedSeason is a never-watched season of a series watched otherwise.
type UnwatchedSeason struct {
	MediaID      int64
	ArrID        int64
	Title        string
	Season       int
	Files        int
	SizeBytes    int64
	LastViewedAt *int64 // of the series
}

// UnwatchedSeasons lists seasons nobody watched although the series was,
// largest first: protected series are excluded, and the series must have
// been added before the cutoff (the series itself may still be watched).
func (s *Store) UnwatchedSeasons(ctx context.Context, scope CleanupScope, limit int) ([]UnwatchedSeason, int64, error) {
	where, args := scope.base("m")
	where += ` AND COALESCE(m.added_at, 0) < ?`
	args = append(args, scope.Cutoff)
	var total int64
	var out []UnwatchedSeason
	err := s.queryEach(ctx, `SELECT m.id, m.arr_id, m.title, se.season_number, se.episode_file_count, se.size_bytes,
		m.last_viewed_at FROM seasons se JOIN media m ON m.id = se.media_id
		WHERE m.kind = 'series' AND `+where+` AND COALESCE(m.watched_episodes, 0) > 0
		  AND se.season_number > 0 AND se.episode_file_count > 0 AND se.watched_episodes = 0
		ORDER BY se.size_bytes DESC`, func(rows *sql.Rows) error {
		var u UnwatchedSeason
		var last sql.NullInt64
		if err := rows.Scan(&u.MediaID, &u.ArrID, &u.Title, &u.Season, &u.Files, &u.SizeBytes, &last); err != nil {
			return err
		}
		u.LastViewedAt = ptrInt(last)
		total += u.SizeBytes
		if len(out) < limit {
			out = append(out, u)
		}
		return nil
	}, args...)
	return out, total, err
}
