package store

import (
	"context"
	"testing"
	"time"
)

func countRows(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.DB().QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestFingerprint(t *testing.T) {
	if got := Fingerprint([]string{"20", "3", "20", "100"}); got != "100,20,3" {
		t.Errorf("Fingerprint = %q", got)
	}
	in := []string{"b", "a"}
	_ = Fingerprint(in)
	if in[0] != "b" {
		t.Error("Fingerprint sorted its argument")
	}
}

func TestReplaceVersions(t *testing.T) {
	s, ctx := openTemp(t), context.Background()
	v := func(server, key, item, group string) MediaVersion {
		return MediaVersion{Server: server, VersionKey: key, ItemKey: item, GroupKey: group, File: "/m/" + key + ".mkv",
			SizeBytes: 100, Resolution: "4k"}
	}
	if err := s.ReplaceVersions(ctx, ServerJellyfin, []MediaVersion{v(ServerJellyfin, "j1", "j1", "tmdb:1"),
		v(ServerJellyfin, "j2", "j1", "tmdb:1")}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceVersions(ctx, ServerPlex, []MediaVersion{v(ServerPlex, "11", "1", "tmdb:1"),
		v(ServerPlex, "12", "1", "tmdb:1"), v(ServerPlex, "21", "2", "tmdb:2"), v(ServerPlex, "22", "3", "tmdb:2")}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	for _, g := range []string{"tmdb:1", "tmdb:2"} {
		if _, err := s.DB().ExecContext(ctx, `INSERT INTO duplicate_acks(server, group_key, fingerprint, acked_at)
			VALUES (?, ?, 'x', ?)`, ServerPlex, g, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.DB().ExecContext(ctx, `INSERT INTO duplicate_acks(server, group_key, fingerprint, acked_at)
		VALUES (?, 'tmdb:1', 'x', ?)`, ServerJellyfin, now); err != nil {
		t.Fatal(err)
	}

	// Next Plex sync: tmdb:2 is solved, tmdb:1 is still there.
	if err := s.ReplaceVersions(ctx, ServerPlex, []MediaVersion{v(ServerPlex, "11", "1", "tmdb:1"),
		v(ServerPlex, "12", "1", "tmdb:1")}); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM media_versions WHERE server = 'plex'`); n != 2 {
		t.Errorf("plex versions = %d, want 2", n)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM media_versions WHERE server = 'jellyfin'`); n != 2 {
		t.Errorf("jellyfin versions = %d, want 2 (other server untouched)", n)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM duplicate_acks WHERE server = 'plex' AND group_key = 'tmdb:2'`); n != 0 {
		t.Error("ack of a solved group kept")
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM duplicate_acks`); n != 2 {
		t.Errorf("acks = %d, want 2 (plex tmdb:1, jellyfin tmdb:1)", n)
	}

	// Nothing left: every Plex row and ack goes.
	if err := s.ReplaceVersions(ctx, ServerPlex, nil); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM duplicate_acks WHERE server = 'plex'`); n != 0 {
		t.Errorf("plex acks = %d, want 0", n)
	}
}

func TestReplaceVersionsDropsSingleVersionGroups(t *testing.T) {
	s, ctx := openTemp(t), context.Background()
	err := s.ReplaceVersions(ctx, ServerPlex, []MediaVersion{
		{Server: ServerPlex, VersionKey: "1", ItemKey: "a", GroupKey: "tmdb:1"},
		{Server: ServerPlex, VersionKey: "2", ItemKey: "a", GroupKey: "tmdb:1"},
		{Server: ServerPlex, VersionKey: "3", ItemKey: "b", GroupKey: "tmdb:2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM media_versions WHERE group_key = 'tmdb:2'`); n != 0 {
		t.Errorf("single-version group kept (%d rows)", n)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM media_versions WHERE group_key = 'tmdb:1'`); n != 2 {
		t.Errorf("tmdb:1 rows = %d, want 2", n)
	}
}

func TestRemoveVersion(t *testing.T) {
	s, ctx := openTemp(t), context.Background()
	v := func(key, group string) MediaVersion {
		return MediaVersion{Server: ServerPlex, VersionKey: key, ItemKey: "i" + key, GroupKey: group}
	}
	if err := s.ReplaceVersions(ctx, ServerPlex, []MediaVersion{v("1", "g3"), v("2", "g3"), v("3", "g3"),
		v("4", "g2"), v("5", "g2")}); err != nil {
		t.Fatal(err)
	}
	// Three versions, one deleted: still a duplicate.
	if err := s.RemoveVersion(ctx, ServerPlex, "1"); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM media_versions WHERE group_key = 'g3'`); n != 2 {
		t.Errorf("g3 = %d rows, want 2", n)
	}
	// Two versions, one deleted: the group is solved.
	if err := s.RemoveVersion(ctx, ServerPlex, "4"); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM media_versions WHERE group_key = 'g2'`); n != 0 {
		t.Errorf("g2 = %d rows, want 0", n)
	}
	// Unknown key or other server: nothing happens.
	if err := s.RemoveVersion(ctx, ServerJellyfin, "2"); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveVersion(ctx, ServerPlex, "nope"); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM media_versions`); n != 2 {
		t.Errorf("rows = %d, want 2", n)
	}
}

func TestReplaceVersionsRejects(t *testing.T) {
	s, ctx := openTemp(t), context.Background()
	if err := s.ReplaceVersions(ctx, "emby", nil); err == nil {
		t.Error("unknown server accepted")
	}
	ok := MediaVersion{Server: ServerPlex, VersionKey: "1", ItemKey: "1", GroupKey: "tmdb:1"}
	for name, bad := range map[string]MediaVersion{
		"other server": {Server: ServerJellyfin, VersionKey: "2", ItemKey: "1", GroupKey: "tmdb:1"},
		"no key":       {Server: ServerPlex, ItemKey: "1", GroupKey: "tmdb:1"},
		"no group":     {Server: ServerPlex, VersionKey: "2", ItemKey: "1"},
		"duplicate":    ok,
	} {
		if err := s.ReplaceVersions(ctx, ServerPlex, []MediaVersion{ok, bad}); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if n := countRows(t, s, `SELECT COUNT(*) FROM media_versions`); n != 0 {
			t.Errorf("%s: %d rows written by a failed replace", name, n)
		}
	}
}
