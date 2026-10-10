package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// MediaVersion is one version (one or more files) of a movie the media
// server has several times.
type MediaVersion struct {
	Server     string // ServerPlex or ServerJellyfin
	VersionKey string // Plex Media id, Jellyfin media source id
	ItemKey    string // Plex ratingKey, Jellyfin item id
	GroupKey   string // tmdb:N, imdb:ttN or item:KEY
	File       string // path of the first file
	Parts      int
	SizeBytes  int64
	Resolution string
	VideoCodec string
	AudioCodec string
	Container  string
	Bitrate    int64 // kbit/s
}

// Fingerprint identifies a set of versions: an acknowledgement only holds
// for the set it was given for.
func Fingerprint(versionKeys []string) string {
	keys := slices.Clone(versionKeys)
	slices.Sort(keys)
	return strings.Join(slices.Compact(keys), ",")
}

// RemoveVersion forgets a version deleted through the media server, until the
// next sync reads the server again; its group goes with it when one version
// is left.
func (s *Store) RemoveVersion(ctx context.Context, server, versionKey string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var group string
	err = tx.QueryRowContext(ctx, `DELETE FROM media_versions WHERE server = ? AND version_key = ?
		RETURNING group_key`, server, versionKey).Scan(&group)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM media_versions WHERE server = ? AND group_key = ?
		AND (SELECT COUNT(*) FROM media_versions WHERE server = ? AND group_key = ?) < 2`,
		server, group, server, group); err != nil {
		return err
	}
	return tx.Commit()
}

// ReplaceVersions stores the duplicate versions of one server, in place of
// those of its previous sync, and drops the acknowledgements of groups that
// are gone. The other server's rows are left as they are, like its keys in
// media.
func (s *Store) ReplaceVersions(ctx context.Context, server string, versions []MediaVersion) error {
	if _, ok := serverColumns[server]; !ok {
		return fmt.Errorf("unknown media server %q", server)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM media_versions WHERE server = ?`, server); err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO media_versions(server, version_key, item_key, group_key,
		file, parts, size_bytes, resolution, video_codec, audio_codec, container, bitrate)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()
	for _, v := range versions {
		if v.Server != server {
			return fmt.Errorf("version %s of %s in the versions of %s", v.VersionKey, v.Server, server)
		}
		if v.VersionKey == "" || v.ItemKey == "" || v.GroupKey == "" {
			return fmt.Errorf("version %q of item %q: missing key", v.VersionKey, v.ItemKey)
		}
		if _, err := stmt.ExecContext(ctx, server, v.VersionKey, v.ItemKey, v.GroupKey, v.File, max(v.Parts, 1),
			v.SizeBytes, v.Resolution, v.VideoCodec, v.AudioCodec, v.Container, v.Bitrate); err != nil {
			return fmt.Errorf("version %s: %w", v.VersionKey, err)
		}
	}
	// A group is listed from the item counts; a version may have no key or be
	// gone when its details are read. One version left is not a duplicate.
	if _, err := tx.ExecContext(ctx, `DELETE FROM media_versions WHERE server = ? AND group_key IN (
		SELECT group_key FROM media_versions WHERE server = ? GROUP BY group_key HAVING COUNT(*) < 2)`,
		server, server); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM duplicate_acks WHERE server = ?
		AND group_key NOT IN (SELECT group_key FROM media_versions WHERE server = ?)`, server, server); err != nil {
		return err
	}
	return tx.Commit()
}
