package store

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"
)

// DuplicateVersion is a version of a duplicate group, with the media row of its item.
type DuplicateVersion struct {
	MediaVersion
	MediaID int64 // media row of its item; 0 if none
}

// DuplicateGroup is a movie the media server has several times.
type DuplicateGroup struct {
	Server      string
	GroupKey    string
	MediaID     int64  // row shown for the group: one managed by Radarr (arr_id NOT NULL) first, else the smallest id; 0 if none
	Title       string // of that row; else the base name of the first version's file
	Year        int
	ArrID       int64              // arr_id of the row managed by Radarr, 0 if none
	Protected   bool               // a media row of the group is protected
	SizeBytes   int64              // sum of the versions
	Versions    []DuplicateVersion // size descending, then version key
	Fingerprint string             // Fingerprint of the version keys
	AckedAt     *time.Time         // set only if acknowledged for this exact Fingerprint
}

// ErrStaleDuplicate: the versions changed since the page was shown.
var ErrStaleDuplicate = errors.New("the versions of this duplicate changed")

// dupMedia is a media row joined to a version (id 0: no row).
type dupMedia struct {
	id        int64
	arrID     int64
	managed   bool // managed by Radarr (arr_id set)
	title     string
	year      int
	protected bool
}

// before reports whether m is shown in place of o: a row managed by Radarr
// first, then the smallest id.
func (m dupMedia) before(o dupMedia) bool {
	if m.managed != o.managed {
		return m.managed
	}
	return m.id < o.id
}

// dupAck is a duplicate_acks row.
type dupAck struct {
	fingerprint string
	at          int64
}

// DuplicateGroups lists the movies the server has several times, largest
// first. Acknowledged groups are included: the caller filters them.
func (s *Store) DuplicateGroups(ctx context.Context, server string, protectedTags []string) ([]DuplicateGroup, error) {
	return s.duplicateGroups(ctx, server, "", protectedTags)
}

// DuplicateGroup returns one duplicate group; ErrNotFound if the server has no
// versions for it.
func (s *Store) DuplicateGroup(ctx context.Context, server, groupKey string, protectedTags []string) (*DuplicateGroup, error) {
	if err := checkServer(server); err != nil {
		return nil, err
	}
	// An empty key would select every group; no group has one.
	if groupKey == "" {
		return nil, ErrNotFound
	}
	groups, err := s.duplicateGroups(ctx, server, groupKey, protectedTags)
	if err != nil {
		return nil, err
	}
	if len(groups) == 0 {
		return nil, ErrNotFound
	}
	return &groups[0], nil
}

// AckDuplicate keeps a duplicate group on purpose, for the versions given by
// fingerprint: ErrStaleDuplicate if they changed since, ErrNotFound if the
// group has no versions any more.
func (s *Store) AckDuplicate(ctx context.Context, server, groupKey, fingerprint string, now time.Time) error {
	if err := checkServer(server); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	keys, err := groupVersionKeys(ctx, tx, server, groupKey)
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		return ErrNotFound
	}
	if Fingerprint(keys) != fingerprint {
		return ErrStaleDuplicate
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO duplicate_acks(server, group_key, fingerprint, acked_at)
		VALUES (?, ?, ?, ?) ON CONFLICT(server, group_key) DO UPDATE SET fingerprint = excluded.fingerprint,
		acked_at = excluded.acked_at`, server, groupKey, fingerprint, now.Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

// UnackDuplicate withdraws an acknowledgement; a group without one is left as is.
func (s *Store) UnackDuplicate(ctx context.Context, server, groupKey string) error {
	if err := checkServer(server); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM duplicate_acks WHERE server = ? AND group_key = ?`, server, groupKey)
	return err
}

// checkServer rejects a server without key column (serverColumns is the whitelist).
func checkServer(server string) error {
	if _, ok := serverColumns[server]; !ok {
		return fmt.Errorf("unknown media server %q", server)
	}
	return nil
}

// groupVersionKeys lists the version keys of a group, inside tx.
func groupVersionKeys(ctx context.Context, tx *sql.Tx, server, groupKey string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT version_key FROM media_versions WHERE server = ? AND group_key = ?`,
		server, groupKey)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// duplicateGroups reads the versions of a server, or only those of groupKey
// when it is not empty, with their media rows and the acknowledgements of the
// server, and assembles them in Go.
func (s *Store) duplicateGroups(ctx context.Context, server, groupKey string, protectedTags []string) ([]DuplicateGroup, error) {
	col, ok := serverColumns[server]
	if !ok {
		return nil, fmt.Errorf("unknown media server %q", server)
	}
	// A version can join several media rows (no unique index on the keys of
	// the rows managed by Radarr): the best of them is shown.
	//nolint:gosec // G202: col comes from the serverColumns whitelist
	query := `SELECT v.group_key, v.version_key, v.item_key, v.file, v.parts, v.size_bytes, v.resolution,
		v.video_codec, v.audio_codec, v.container, v.bitrate, COALESCE(m.id, 0), m.arr_id,
		COALESCE(m.title, ''), COALESCE(m.year, 0), COALESCE(m.tags, '[]'), COALESCE(m.protected_local, 0)
		FROM media_versions v LEFT JOIN media m ON m.kind = 'movie' AND m.` + col + ` = v.item_key
		AND m.match_state <> 'hidden'
		WHERE v.server = ?`
	args := []any{server}
	if groupKey != "" {
		query += ` AND v.group_key = ?`
		args = append(args, groupKey)
	}

	groups := map[string]*DuplicateGroup{}
	shownGroup := map[string]dupMedia{}   // row shown for each group
	shownVersion := map[string]dupMedia{} // row shown for each version
	err := s.queryEach(ctx, query, func(rows *sql.Rows) error {
		var gk, tags string
		var v DuplicateVersion
		var m dupMedia
		var arr sql.NullInt64
		if err := rows.Scan(&gk, &v.VersionKey, &v.ItemKey, &v.File, &v.Parts, &v.SizeBytes, &v.Resolution,
			&v.VideoCodec, &v.AudioCodec, &v.Container, &v.Bitrate, &m.id, &arr, &m.title, &m.year, &tags,
			&m.protected); err != nil {
			return err
		}
		g := groups[gk]
		if g == nil {
			g = &DuplicateGroup{Server: server, GroupKey: gk}
			groups[gk] = g
		}
		if _, seen := shownVersion[v.VersionKey]; !seen {
			v.Server, v.GroupKey = server, gk
			g.Versions = append(g.Versions, v)
			g.SizeBytes += v.SizeBytes
			shownVersion[v.VersionKey] = dupMedia{}
		}
		if m.id == 0 {
			return nil
		}
		m.managed, m.arrID = arr.Valid, arr.Int64
		var labels []string
		_ = json.Unmarshal([]byte(tags), &labels)
		m.protected = ProtectionOf(labels, m.protected, protectedTags) != Unprotected
		if cur := shownVersion[v.VersionKey]; cur.id == 0 || m.before(cur) {
			shownVersion[v.VersionKey] = m
		}
		if cur := shownGroup[gk]; cur.id == 0 || m.before(cur) {
			shownGroup[gk] = m
		}
		g.Protected = g.Protected || m.protected
		return nil
	}, args...)
	if err != nil {
		return nil, err
	}

	acks := map[string]dupAck{}
	err = s.queryEach(ctx, `SELECT group_key, fingerprint, acked_at FROM duplicate_acks WHERE server = ?`,
		func(rows *sql.Rows) error {
			var gk string
			var a dupAck
			if err := rows.Scan(&gk, &a.fingerprint, &a.at); err != nil {
				return err
			}
			acks[gk] = a
			return nil
		}, server)
	if err != nil {
		return nil, err
	}

	out := make([]DuplicateGroup, 0, len(groups))
	for gk, g := range groups {
		slices.SortFunc(g.Versions, func(a, b DuplicateVersion) int {
			return cmp.Or(cmp.Compare(b.SizeBytes, a.SizeBytes), strings.Compare(a.VersionKey, b.VersionKey))
		})
		keys := make([]string, 0, len(g.Versions))
		for i := range g.Versions {
			g.Versions[i].MediaID = shownVersion[g.Versions[i].VersionKey].id
			keys = append(keys, g.Versions[i].VersionKey)
		}
		g.Fingerprint = Fingerprint(keys)
		if best := shownGroup[gk]; best.id != 0 {
			g.MediaID, g.Title, g.Year = best.id, best.title, best.year
			if best.managed {
				g.ArrID = best.arrID
			}
		} else if file := g.Versions[0].File; file != "" {
			g.Title = path.Base(file)
		}
		if a, ok := acks[gk]; ok && a.fingerprint == g.Fingerprint {
			at := time.Unix(a.at, 0)
			g.AckedAt = &at
		}
		out = append(out, *g)
	}
	slices.SortFunc(out, func(a, b DuplicateGroup) int {
		return cmp.Or(cmp.Compare(b.SizeBytes, a.SizeBytes), strings.Compare(a.Title, b.Title),
			strings.Compare(a.GroupKey, b.GroupKey))
	})
	return out, nil
}
