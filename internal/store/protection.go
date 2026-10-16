package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Actions journaled by the sync when a local protection follows its item to
// another row, or no longer finds it (action_log.action).
const (
	ProtectCarried   = "protect_carried"   // now applies to an item Sonarr/Radarr manages
	ProtectUnmatched = "protect_unmatched" // no item matches it any more (kept until it comes back)
)

// localMatch is the SQL condition "local protection lp applies to media row
// m": same kind and one shared identifier (TMDB, TVDB, IMDb, Plex key,
// Jellyfin id); a protection without any external id also matches on the
// normalized title and year, so a new Plex key does not drop it. It errs on
// the side of protecting.
func localMatch(lp, m string) string {
	return lp + `.kind = ` + m + `.kind AND (` +
		lp + `.tmdb_id = ` + m + `.tmdb_id OR ` +
		lp + `.tvdb_id = ` + m + `.tvdb_id OR ` +
		lp + `.imdb_id = ` + m + `.imdb_id OR ` +
		lp + `.plex_rating_key = ` + m + `.plex_rating_key OR ` +
		lp + `.jellyfin_id = ` + m + `.jellyfin_id OR (` +
		lp + `.tmdb_id IS NULL AND ` + lp + `.tvdb_id IS NULL AND ` + lp + `.imdb_id IS NULL AND ` +
		lp + `.title_norm <> '' AND ` + lp + `.title_norm = ` + m + `.title_norm AND COALESCE(` +
		lp + `.year, 0) = COALESCE(` + m + `.year, 0)))`
}

// syncProtectedLocal recomputes the media.protected_local cache.
//
//nolint:gosec // G202: localMatch only concatenates constant column names
var syncProtectedLocal = `UPDATE media SET protected_local = new.p FROM (
	SELECT m.id, EXISTS (SELECT 1 FROM local_protections lp WHERE ` + localMatch("lp", "m") + `) AS p FROM media m
) AS new WHERE media.id = new.id AND media.protected_local <> new.p`

// strongMatch is localMatch without the title fallback: used to learn the
// external ids of a protection from the row it certainly designates.
func strongMatch(lp, m string) string {
	return lp + `.kind = ` + m + `.kind AND (` +
		lp + `.tmdb_id = ` + m + `.tmdb_id OR ` +
		lp + `.tvdb_id = ` + m + `.tvdb_id OR ` +
		lp + `.imdb_id = ` + m + `.imdb_id OR ` +
		lp + `.plex_rating_key = ` + m + `.plex_rating_key OR ` +
		lp + `.jellyfin_id = ` + m + `.jellyfin_id)`
}

// protectionState is what refreshLocalProtection compares before and after.
type protectionState struct {
	media   map[int64]bool // protected media rows
	matched map[int64]bool // local protections matching at least one row
}

func readProtectionState(ctx context.Context, q *sql.Tx) (protectionState, error) {
	media, err := idSet(ctx, q, `SELECT id FROM media WHERE protected_local`)
	if err != nil {
		return protectionState{}, err
	}
	//nolint:gosec // G202: localMatch only concatenates constant column names
	matched, err := idSet(ctx, q, `SELECT lp.id FROM local_protections lp
		WHERE EXISTS (SELECT 1 FROM media m WHERE `+localMatch("lp", "m")+`)`)
	return protectionState{media: media, matched: matched}, err
}

// idSet runs a query returning one id column.
func idSet(ctx context.Context, q *sql.Tx, query string) (map[int64]bool, error) {
	rows, err := q.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// refreshLocalProtection recomputes media.protected_local from the
// local_protections table, learns missing external ids of the protections
// from the rows they designate, and journals (action_log) a protection that
// now applies to an item managed by Sonarr/Radarr, or that no longer finds
// any item. before is the state read at the start of the transaction, before
// the catalog changed.
func refreshLocalProtection(ctx context.Context, q *sql.Tx, before protectionState, now time.Time) error {
	for _, col := range []string{"tmdb_id", "tvdb_id", "imdb_id"} {
		//nolint:gosec // G202: col is one of the constants above
		if _, err := q.ExecContext(ctx, `UPDATE local_protections SET `+col+` = (SELECT m.`+col+` FROM media m
			WHERE `+strongMatch("local_protections", "m")+` AND m.`+col+` IS NOT NULL ORDER BY m.arr_id IS NULL, m.id LIMIT 1)
			WHERE `+col+` IS NULL`); err != nil {
			return fmt.Errorf("local protection ids: %w", err)
		}
	}
	if _, err := q.ExecContext(ctx, syncProtectedLocal); err != nil {
		return fmt.Errorf("local protection: %w", err)
	}
	after, err := readProtectionState(ctx, q)
	if err != nil {
		return fmt.Errorf("local protection: %w", err)
	}

	var carried []int64
	for id := range after.media {
		if !before.media[id] {
			carried = append(carried, id)
		}
	}
	for _, id := range carried {
		var kind, title string
		var arrID sql.NullInt64
		if err := q.QueryRowContext(ctx, `SELECT kind, arr_id, title FROM media WHERE id = ?`, id).Scan(&kind, &arrID, &title); err != nil {
			return err
		}
		if !arrID.Valid {
			continue // an item only the media server knows, under a new key: nothing to report
		}
		if err := journalSystem(ctx, q, now, ProtectCarried, kind, arrID, title); err != nil {
			return err
		}
	}
	for id := range before.matched {
		if after.matched[id] {
			continue
		}
		var kind, title string
		if err := q.QueryRowContext(ctx, `SELECT kind, title FROM local_protections WHERE id = ?`, id).Scan(&kind, &title); err != nil {
			return err
		}
		if err := journalSystem(ctx, q, now, ProtectUnmatched, kind, sql.NullInt64{}, title); err != nil {
			return err
		}
	}
	return nil
}

func journalSystem(ctx context.Context, q *sql.Tx, now time.Time, action, kind string, arrID sql.NullInt64, title string) error {
	_, err := q.ExecContext(ctx, `INSERT INTO action_log(at, action, media_kind, arr_id, title, delete_files,
		import_exclusion, dry_run, success, username) VALUES (?, ?, ?, ?, ?, 0, 0, 0, 1, '')`,
		now.Unix(), action, kind, arrID, title)
	return err
}

// SetLocalProtection protects (or not) an item in Hangarr only. Protecting
// records its identity in local_protections; unprotecting removes every
// local protection that designates it. media.protected_local follows.
func (s *Store) SetLocalProtection(ctx context.Context, id int64, protected bool, username string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var res sql.Result
	if protected {
		res, err = tx.ExecContext(ctx, `INSERT INTO local_protections(kind, tmdb_id, tvdb_id, imdb_id,
			plex_rating_key, jellyfin_id, title, title_norm, year, created_at, username)
			SELECT kind, tmdb_id, tvdb_id, imdb_id, plex_rating_key, jellyfin_id, title, title_norm, year, ?, ?
			FROM media WHERE id = ?`, now.Unix(), nullString(username), id)
	} else {
		//nolint:gosec // G202: localMatch only concatenates constant column names
		res, err = tx.ExecContext(ctx, `DELETE FROM local_protections AS lp WHERE EXISTS (
			SELECT 1 FROM media m WHERE m.id = ? AND `+localMatch("lp", "m")+`)`, id)
	}
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 && protected {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, syncProtectedLocal); err != nil {
		return err
	}
	return tx.Commit()
}
