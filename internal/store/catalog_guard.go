package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Guard against a Sonarr/Radarr answer that would empty or gut the local
// catalog (BUG-02): a starting or freshly restored instance, the wrong
// instance after a URL change, a proxy answering []. Deleting the rows would
// drop their protection tags and turn their files into items only the media
// server knows, deletable through it. Such a catalog is refused, the previous
// one kept, until it is accepted on the Health page.
const (
	// CatalogShrinkMaxPercent: a sync may remove at most this share of the
	// items of a kind...
	CatalogShrinkMaxPercent = 20
	// catalogShrinkFloor ...unless it removes this many items or fewer
	// (small libraries); an empty answer is always suspicious.
	catalogShrinkFloor = 5
)

// CatalogShrinkPrefix starts the message of a refused catalog (sync_runs.error).
const CatalogShrinkPrefix = "suspicious catalog"

// CatalogShrinkError reports a refused catalog: Removed of the Before stored
// items are missing from the answer of After items.
type CatalogShrinkError struct {
	Kind    string
	Before  int
	After   int
	Removed int
}

func (e *CatalogShrinkError) Error() string {
	return fmt.Sprintf("%s: %d of %d %s missing from the answer (%d received), the previous catalog is kept; accept it on the Health page if it is right",
		CatalogShrinkPrefix, e.Removed, e.Before, kindPlural(e.Kind), e.After)
}

func kindPlural(kind string) string {
	if kind == "movie" {
		return "movies"
	}
	return "series"
}

// catalogShrinkKey is the setting that accepts the next shrink of a kind.
func catalogShrinkKey(kind string) string { return "catalog_shrink_accepted:" + kind }

// AcceptCatalogShrink lets the next sync of kind replace the catalog even if
// it removes many items (once; the acceptance expires after a day).
func (s *Store) AcceptCatalogShrink(ctx context.Context, kind string, now time.Time) error {
	return s.SetSetting(ctx, catalogShrinkKey(kind), fmt.Sprint(now.Unix()), now)
}

// checkCatalogShrink refuses a catalog of n items that would remove too many
// stored ones, unless the shrink was accepted (the acceptance is consumed).
func checkCatalogShrink(ctx context.Context, tx *sql.Tx, kind string, arrIDs []int64, now time.Time) error {
	var before, removed int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(arr_id NOT IN (SELECT value FROM json_each(?))), 0)
		FROM media WHERE kind = ? AND arr_id IS NOT NULL`, idsJSON(arrIDs), kind).Scan(&before, &removed); err != nil {
		return err
	}
	suspicious := removed > 0 && (len(arrIDs) == 0 ||
		(removed > catalogShrinkFloor && removed*100 > before*CatalogShrinkMaxPercent))
	if !suspicious {
		return nil
	}
	var at int64
	err := tx.QueryRowContext(ctx, `SELECT CAST(value AS INTEGER) FROM app_settings WHERE key = ?`, catalogShrinkKey(kind)).Scan(&at)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil && now.Unix()-at < int64((24*time.Hour).Seconds()) {
		_, err := tx.ExecContext(ctx, `DELETE FROM app_settings WHERE key = ?`, catalogShrinkKey(kind))
		return err
	}
	return &CatalogShrinkError{Kind: kind, Before: before, After: len(arrIDs), Removed: removed}
}

// ArrCatalogHealthy reports whether the last finished sync of the
// Sonarr/Radarr catalog of kind succeeded (true before any). While it is
// not, the local catalog may miss items Sonarr/Radarr manage (and their
// protection tags): nothing is deleted through the media server.
func (s *Store) ArrCatalogHealthy(ctx context.Context, kind string) (bool, error) {
	source := "radarr"
	if kind == "series" {
		source = "sonarr"
	}
	var status string
	err := s.db.QueryRowContext(ctx, `SELECT status FROM sync_runs WHERE source = ? AND finished_at IS NOT NULL
		ORDER BY id DESC LIMIT 1`, source).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return status == "success", nil
}
