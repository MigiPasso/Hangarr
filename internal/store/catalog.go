package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"hangarr/internal/matching"
)

// CatalogItem is a Sonarr series or Radarr movie to upsert.
type CatalogItem struct {
	Kind               string
	ArrID              int64
	Title              string
	SortTitle          string
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
	TotalEpisodes      *int
	DownloadedEpisodes *int
	RuntimeMinutes     int
	PosterURL          string
	FanartURL          string
	TitleSlug          string
	Seasons            []SeasonRecord
	// Matching hints, not stored: other titles and the movie file name.
	AltTitles []string
	FileName  string
}

// SeasonRecord is a Sonarr season.
type SeasonRecord struct {
	Number            int
	Monitored         bool
	EpisodeFileCount  int
	TotalEpisodeCount int
	SizeBytes         int64
}

func jsonList(v []string) string {
	if v == nil {
		v = []string{}
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func nullString(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

func nullPositive(n int64) sql.NullInt64 { return sql.NullInt64{Int64: n, Valid: n > 0} }

func nullIntPtr(n *int) sql.NullInt64 {
	if n == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(*n), Valid: true}
}

func idsJSON(ids []int64) string {
	if ids == nil {
		ids = []int64{}
	}
	b, _ := json.Marshal(ids)
	return string(b)
}

// ReplaceArrCatalog upserts the full Sonarr (kind "series") or Radarr (kind
// "movie") catalog and deletes the items that disappeared, in one
// transaction. Plex link and watch statistics columns are left untouched.
func (s *Store) ReplaceArrCatalog(ctx context.Context, kind string, items []CatalogItem, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	prot, err := readProtectionState(ctx, tx)
	if err != nil {
		return err
	}
	arrIDs, err := upsertItems(ctx, tx, kind, items, now)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM media WHERE kind = ? AND arr_id IS NOT NULL
		AND arr_id NOT IN (SELECT value FROM json_each(?))`, kind, idsJSON(arrIDs)); err != nil {
		return err
	}
	if err := refreshLocalProtection(ctx, tx, prot, now); err != nil {
		return err
	}
	return tx.Commit()
}

// UpsertCatalogItem refreshes a single Sonarr/Radarr item (targeted resync
// after a deletion).
func (s *Store) UpsertCatalogItem(ctx context.Context, kind string, item CatalogItem, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	prot, err := readProtectionState(ctx, tx)
	if err != nil {
		return err
	}
	if _, err := upsertItems(ctx, tx, kind, []CatalogItem{item}, now); err != nil {
		return err
	}
	if err := refreshLocalProtection(ctx, tx, prot, now); err != nil {
		return err
	}
	return tx.Commit()
}

func upsertItems(ctx context.Context, tx *sql.Tx, kind string, items []CatalogItem, now time.Time) ([]int64, error) {
	upsert, err := tx.PrepareContext(ctx, `INSERT INTO media(kind, arr_id, title, sort_title, year, overview,
		tvdb_id, tmdb_id, imdb_id, status, genres, network_or_studio, rating, size_bytes, added_at, tags,
		quality_profile, monitored, path, has_file, total_episodes, downloaded_episodes, runtime_minutes,
		poster_remote_url, fanart_remote_url, match_state, updated_at, title_norm, title_slug)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'arr_only', ?, ?, ?)
		ON CONFLICT(kind, arr_id) DO UPDATE SET
		  title = excluded.title, title_norm = excluded.title_norm, sort_title = excluded.sort_title, year = excluded.year,
		  overview = excluded.overview, tvdb_id = excluded.tvdb_id, tmdb_id = excluded.tmdb_id,
		  imdb_id = excluded.imdb_id, status = excluded.status, genres = excluded.genres,
		  network_or_studio = excluded.network_or_studio, rating = excluded.rating,
		  size_bytes = excluded.size_bytes, added_at = excluded.added_at, tags = excluded.tags,
		  quality_profile = excluded.quality_profile, monitored = excluded.monitored, path = excluded.path,
		  has_file = excluded.has_file, total_episodes = excluded.total_episodes,
		  downloaded_episodes = excluded.downloaded_episodes, runtime_minutes = excluded.runtime_minutes,
		  poster_remote_url = excluded.poster_remote_url, fanart_remote_url = excluded.fanart_remote_url,
		  updated_at = excluded.updated_at, title_slug = excluded.title_slug
		RETURNING id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = upsert.Close() }()
	upsertSeason, err := tx.PrepareContext(ctx, `INSERT INTO seasons(media_id, season_number, monitored,
		episode_file_count, total_episode_count, size_bytes) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(media_id, season_number) DO UPDATE SET monitored = excluded.monitored,
		  episode_file_count = excluded.episode_file_count, total_episode_count = excluded.total_episode_count,
		  size_bytes = excluded.size_bytes`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = upsertSeason.Close() }()

	arrIDs := make([]int64, 0, len(items))
	for _, it := range items {
		var rating sql.NullFloat64
		if it.Rating > 0 {
			rating = sql.NullFloat64{Float64: it.Rating, Valid: true}
		}
		var id int64
		err := upsert.QueryRowContext(ctx, kind, it.ArrID, it.Title, nullString(it.SortTitle), nullPositive(int64(it.Year)),
			nullString(it.Overview), nullPositive(it.TvdbID), nullPositive(it.TmdbID), nullString(it.ImdbID),
			nullString(it.Status), jsonList(it.Genres), nullString(it.NetworkOrStudio), rating, it.SizeBytes,
			nullInt(it.AddedAt), jsonList(it.Tags), nullString(it.QualityProfile), it.Monitored, nullString(it.Path),
			it.HasFile, nullIntPtr(it.TotalEpisodes), nullIntPtr(it.DownloadedEpisodes),
			nullPositive(int64(it.RuntimeMinutes)), nullString(it.PosterURL), nullString(it.FanartURL), now.Unix(),
			matching.NormalizeTitle(it.Title), nullString(it.TitleSlug),
		).Scan(&id)
		if err != nil {
			return nil, fmt.Errorf("upsert %s %d: %w", kind, it.ArrID, err)
		}
		arrIDs = append(arrIDs, it.ArrID)

		if kind != "series" {
			continue
		}
		numbers := make([]int64, 0, len(it.Seasons))
		for _, se := range it.Seasons {
			if _, err := upsertSeason.ExecContext(ctx, id, se.Number, se.Monitored, se.EpisodeFileCount,
				se.TotalEpisodeCount, se.SizeBytes); err != nil {
				return nil, err
			}
			numbers = append(numbers, int64(se.Number))
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM seasons WHERE media_id = ?
			AND season_number NOT IN (SELECT value FROM json_each(?))`, id, idsJSON(numbers)); err != nil {
			return nil, err
		}
	}
	return arrIDs, nil
}

// ArrMedia is a stored Sonarr/Radarr item, as needed for matching.
type ArrMedia struct {
	ID     int64
	ArrID  int64
	Kind   string
	TvdbID int64
	TmdbID int64
	ImdbID string
	Title  string
	Year   int
}

// ListArrMedia returns every media row managed by Sonarr/Radarr.
func (s *Store) ListArrMedia(ctx context.Context) ([]ArrMedia, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, arr_id, kind, COALESCE(tvdb_id, 0), COALESCE(tmdb_id, 0),
		COALESCE(imdb_id, ''), title, COALESCE(year, 0) FROM media WHERE arr_id IS NOT NULL ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ArrMedia
	for rows.Next() {
		var m ArrMedia
		if err := rows.Scan(&m.ID, &m.ArrID, &m.Kind, &m.TvdbID, &m.TmdbID, &m.ImdbID, &m.Title, &m.Year); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Media servers: the server whose catalog the sync reads (one at a time).
const (
	ServerPlex     = "plex"
	ServerJellyfin = "jellyfin"
)

// serverColumns maps a server to its key column (whitelist: the name is
// concatenated into SQL).
var serverColumns = map[string]string{ServerPlex: "plex_rating_key", ServerJellyfin: "jellyfin_id"}

// SettingCatalogServer records the server of the last catalog match: the
// match states describe that server.
const SettingCatalogServer = "catalog_server"

// ServerLink attaches a media server key (Plex rating key or Jellyfin id)
// to an arr media row.
type ServerLink struct {
	MediaID int64
	Key     string
	Method  string
}

// ServerOnlyItem is a media server item not managed by Sonarr/Radarr.
type ServerOnlyItem struct {
	Kind    string
	Key     string
	Title   string
	Year    int
	TvdbID  int64
	TmdbID  int64
	ImdbID  string
	AddedAt int64
}

// ServerSeason maps a media server season to its show.
type ServerSeason struct {
	ShowKey string
	Number  int
	Key     string
}

// ApplyServerMatches stores the result of a matching pass against server in
// one transaction: arr rows get their link to it (or become arr_only), its
// server_only rows are upserted and stale ones deleted, and Sonarr seasons
// get its season keys. The keys of the other server are left as they are
// (its history stays linked) and the items only it knows are hidden, with
// their local protection, until it is the catalog server again. Local
// protections are bound to the items' identity (local_protections), not to
// the rows: they follow an item that Sonarr/Radarr took over or that got a
// new key.
func (s *Store) ApplyServerMatches(ctx context.Context, server string, links []ServerLink, only []ServerOnlyItem,
	seasons []ServerSeason, now time.Time) error {
	col, ok := serverColumns[server]
	if !ok {
		return fmt.Errorf("unknown media server %q", server)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	prot, err := readProtectionState(ctx, tx)
	if err != nil {
		return err
	}

	//nolint:gosec // G202: col comes from the serverColumns whitelist
	if _, err := tx.ExecContext(ctx, `UPDATE media SET `+col+` = NULL, match_state = 'arr_only',
		match_method = NULL WHERE arr_id IS NOT NULL`); err != nil {
		return err
	}
	for _, l := range links {
		//nolint:gosec // G202: col comes from the serverColumns whitelist
		if _, err := tx.ExecContext(ctx, `UPDATE media SET `+col+` = ?, match_state = 'matched',
			match_method = ? WHERE id = ?`, l.Key, l.Method, l.MediaID); err != nil {
			return err
		}
	}
	//nolint:gosec // G202: col comes from the serverColumns whitelist
	if _, err := tx.ExecContext(ctx, `UPDATE media SET match_state = 'hidden'
		WHERE arr_id IS NULL AND `+col+` IS NULL`); err != nil {
		return err
	}

	keys := make([]string, 0, len(only))
	for _, p := range only {
		var added sql.NullInt64
		if p.AddedAt > 0 {
			added = sql.NullInt64{Int64: p.AddedAt, Valid: true}
		}
		//nolint:gosec // G202: col comes from the serverColumns whitelist
		if _, err := tx.ExecContext(ctx, `INSERT INTO media(kind, `+col+`, title, year, tvdb_id, tmdb_id,
			imdb_id, added_at, match_state, updated_at, title_norm) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'server_only', ?, ?)
			ON CONFLICT(kind, `+col+`) WHERE arr_id IS NULL DO UPDATE SET title = excluded.title,
			  title_norm = excluded.title_norm, match_state = 'server_only',
			  year = excluded.year, tvdb_id = excluded.tvdb_id, tmdb_id = excluded.tmdb_id,
			  imdb_id = excluded.imdb_id, added_at = excluded.added_at, updated_at = excluded.updated_at`,
			p.Kind, p.Key, p.Title, nullPositive(int64(p.Year)), nullPositive(p.TvdbID), nullPositive(p.TmdbID),
			nullString(p.ImdbID), added, now.Unix(), matching.NormalizeTitle(p.Title)); err != nil {
			return err
		}
		keys = append(keys, p.Kind+"|"+p.Key)
	}
	b, _ := json.Marshal(keys)
	//nolint:gosec // G202: col comes from the serverColumns whitelist
	if _, err := tx.ExecContext(ctx, `DELETE FROM media WHERE arr_id IS NULL AND `+col+` IS NOT NULL
		AND kind || '|' || `+col+` NOT IN (SELECT value FROM json_each(?))`, string(b)); err != nil {
		return err
	}

	//nolint:gosec // G202: col comes from the serverColumns whitelist
	if _, err := tx.ExecContext(ctx, `UPDATE seasons SET `+col+` = NULL`); err != nil {
		return err
	}
	for _, se := range seasons {
		//nolint:gosec // G202: col comes from the serverColumns whitelist
		if _, err := tx.ExecContext(ctx, `UPDATE seasons SET `+col+` = ? WHERE season_number = ?
			AND media_id IN (SELECT id FROM media WHERE kind = 'series' AND `+col+` = ?)`,
			se.Key, se.Number, se.ShowKey); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO app_settings(key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		SettingCatalogServer, server, now.Unix()); err != nil {
		return err
	}
	// After the stale server_only rows are gone: a protection follows its item
	// to the row that took its key (Sonarr/Radarr took it over, new key).
	if err := refreshLocalProtection(ctx, tx, prot, now); err != nil {
		return err
	}
	return tx.Commit()
}

// CatalogServer returns the server the match states describe: that of the
// last catalog sync (Plex before any).
func (s *Store) CatalogServer(ctx context.Context) string {
	v, _ := s.Setting(ctx, SettingCatalogServer, ServerPlex)
	return v
}

// MatchCounts reports how many media rows are in each match state.
func (s *Store) MatchCounts(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT match_state, COUNT(*) FROM media GROUP BY match_state`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int{}
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, rows.Err()
}
