package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"cleanarr/internal/matching"
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
	arrIDs, err := upsertItems(ctx, tx, kind, items, now)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM media WHERE kind = ? AND arr_id IS NOT NULL
		AND arr_id NOT IN (SELECT value FROM json_each(?))`, kind, idsJSON(arrIDs)); err != nil {
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
	if _, err := upsertItems(ctx, tx, kind, []CatalogItem{item}, now); err != nil {
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

// PlexLink attaches a Plex rating key to an arr media row.
type PlexLink struct {
	MediaID   int64
	RatingKey string
	Method    string
}

// PlexOnlyItem is a Plex item not managed by Sonarr/Radarr.
type PlexOnlyItem struct {
	Kind      string
	RatingKey string
	Title     string
	Year      int
	TvdbID    int64
	TmdbID    int64
	ImdbID    string
	AddedAt   int64
}

// PlexSeason maps a Plex season to its show.
type PlexSeason struct {
	ShowRatingKey string
	Number        int
	RatingKey     string
}

// ApplyPlexMatches stores the result of a matching pass in one transaction:
// arr rows get their link (or become arr_only), plex_only rows are upserted
// and stale ones deleted, and Sonarr seasons get their Plex rating keys.
func (s *Store) ApplyPlexMatches(ctx context.Context, links []PlexLink, plexOnly []PlexOnlyItem, seasons []PlexSeason, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `UPDATE media SET plex_rating_key = NULL, match_state = 'arr_only',
		match_method = NULL WHERE arr_id IS NOT NULL`); err != nil {
		return err
	}
	for _, l := range links {
		if _, err := tx.ExecContext(ctx, `UPDATE media SET plex_rating_key = ?, match_state = 'matched',
			match_method = ? WHERE id = ?`, l.RatingKey, l.Method, l.MediaID); err != nil {
			return err
		}
	}

	keys := make([]string, 0, len(plexOnly))
	for _, p := range plexOnly {
		var added sql.NullInt64
		if p.AddedAt > 0 {
			added = sql.NullInt64{Int64: p.AddedAt, Valid: true}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO media(kind, plex_rating_key, title, year, tvdb_id, tmdb_id,
			imdb_id, added_at, match_state, updated_at, title_norm) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'plex_only', ?, ?)
			ON CONFLICT(kind, plex_rating_key) WHERE arr_id IS NULL DO UPDATE SET title = excluded.title,
			  title_norm = excluded.title_norm,
			  year = excluded.year, tvdb_id = excluded.tvdb_id, tmdb_id = excluded.tmdb_id,
			  imdb_id = excluded.imdb_id, added_at = excluded.added_at, updated_at = excluded.updated_at`,
			p.Kind, p.RatingKey, p.Title, nullPositive(int64(p.Year)), nullPositive(p.TvdbID), nullPositive(p.TmdbID),
			nullString(p.ImdbID), added, now.Unix(), matching.NormalizeTitle(p.Title)); err != nil {
			return err
		}
		keys = append(keys, p.Kind+"|"+p.RatingKey)
	}
	b, _ := json.Marshal(keys)
	if _, err := tx.ExecContext(ctx, `DELETE FROM media WHERE arr_id IS NULL
		AND kind || '|' || plex_rating_key NOT IN (SELECT value FROM json_each(?))`, string(b)); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `UPDATE seasons SET plex_rating_key = NULL`); err != nil {
		return err
	}
	for _, se := range seasons {
		if _, err := tx.ExecContext(ctx, `UPDATE seasons SET plex_rating_key = ? WHERE season_number = ?
			AND media_id IN (SELECT id FROM media WHERE kind = 'series' AND plex_rating_key = ?)`,
			se.RatingKey, se.Number, se.ShowRatingKey); err != nil {
			return err
		}
	}
	return tx.Commit()
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
