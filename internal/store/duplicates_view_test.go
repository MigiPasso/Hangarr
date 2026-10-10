package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// duplicateStore holds Dune (a Radarr movie linked to Plex key 6894, two
// versions) and two Tenet items only Plex knows, one version each: two groups.
func duplicateStore(t *testing.T) *Store {
	t.Helper()
	s, ctx, now := openTemp(t), context.Background(), time.Unix(1_790_812_800, 0)
	if err := s.ReplaceArrCatalog(ctx, "movie", []CatalogItem{{Kind: "movie", ArrID: 7, Title: "Dune", Year: 2021,
		TmdbID: 438631, Tags: []string{"Keep "}}}, now); err != nil {
		t.Fatal(err)
	}
	var dune int64
	if err := s.DB().QueryRow(`SELECT id FROM media WHERE arr_id = 7`).Scan(&dune); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyServerMatches(ctx, ServerPlex, []ServerLink{{MediaID: dune, Key: "6894", Method: "tmdb"}},
		[]ServerOnlyItem{{Kind: "movie", Key: "10", Title: "Tenet", Year: 2020, TmdbID: 577922},
			{Kind: "movie", Key: "11", Title: "Tenet", Year: 2020, TmdbID: 577922}}, nil, now); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceVersions(ctx, ServerPlex, plexDuplicateVersions()); err != nil {
		t.Fatal(err)
	}
	return s
}

func plexDuplicateVersions() []MediaVersion {
	v := func(key, item, group, file string, size int64) MediaVersion {
		return MediaVersion{Server: ServerPlex, VersionKey: key, ItemKey: item, GroupKey: group, File: file, SizeBytes: size}
	}
	return []MediaVersion{
		v("11195", "6894", "tmdb:438631", "/movies/Dune (2021)/Dune.2021.1080p.mkv", 12_000_000_000),
		v("11205", "6894", "tmdb:438631", "/movies/Dune (2021)/Dune.2021.2160p.mkv", 78_000_000_000),
		v("21", "10", "tmdb:577922", "/movies/Tenet (2020)/Tenet.mkv", 5_000_000_000),
		v("22", "11", "tmdb:577922", "/movies/Tenet (2020) 2/Tenet.mkv", 5_000_000_000),
	}
}

func mediaIDOf(t *testing.T, s *Store, query string, args ...any) int64 {
	t.Helper()
	var id int64
	if err := s.DB().QueryRow(query, args...).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func groupsByKey(groups []DuplicateGroup) map[string]DuplicateGroup {
	out := map[string]DuplicateGroup{}
	for _, g := range groups {
		out[g.GroupKey] = g
	}
	return out
}

func TestDuplicateGroups(t *testing.T) {
	s, ctx := duplicateStore(t), context.Background()
	dune := mediaIDOf(t, s, `SELECT id FROM media WHERE arr_id = 7`)
	tenetFirst := mediaIDOf(t, s, `SELECT MIN(id) FROM media WHERE title = 'Tenet'`)
	tenet10 := mediaIDOf(t, s, `SELECT id FROM media WHERE plex_rating_key = '10'`)
	tenet11 := mediaIDOf(t, s, `SELECT id FROM media WHERE plex_rating_key = '11'`)

	groups, err := s.DuplicateGroups(ctx, ServerPlex, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 || groups[0].GroupKey != "tmdb:438631" || groups[1].GroupKey != "tmdb:577922" {
		t.Fatalf("groups: %+v", groups)
	}
	d := groups[0]
	if d.Server != ServerPlex || d.MediaID != dune || d.Title != "Dune" || d.Year != 2021 || d.ArrID != 7 ||
		d.SizeBytes != 90_000_000_000 {
		t.Errorf("Dune: %+v", d)
	}
	if len(d.Versions) != 2 || d.Versions[0].VersionKey != "11205" || d.Versions[1].VersionKey != "11195" {
		t.Fatalf("Dune versions: %+v", d.Versions)
	}
	for _, v := range d.Versions {
		if v.MediaID != dune || v.Server != ServerPlex || v.GroupKey != "tmdb:438631" {
			t.Errorf("Dune version %s: media %d server %q group %q", v.VersionKey, v.MediaID, v.Server, v.GroupKey)
		}
	}
	if d.Fingerprint != "11195,11205" || d.AckedAt != nil {
		t.Errorf("Dune fingerprint %q, acked %v", d.Fingerprint, d.AckedAt)
	}

	tn := groups[1]
	if tn.ArrID != 0 || tn.MediaID != tenetFirst || tn.Title != "Tenet" || tn.Year != 2020 || tn.SizeBytes != 10_000_000_000 {
		t.Errorf("Tenet: %+v, want media %d", tn, tenetFirst)
	}
	if len(tn.Versions) != 2 || tn.Versions[0].MediaID != tenet10 || tn.Versions[1].MediaID != tenet11 {
		t.Errorf("Tenet versions: %+v", tn.Versions)
	}

	if other, err := s.DuplicateGroups(ctx, ServerJellyfin, nil); err != nil || len(other) != 0 {
		t.Errorf("Jellyfin groups: %+v %v, want none", other, err)
	}
}

func TestDuplicateGroupsProtected(t *testing.T) {
	s, ctx := duplicateStore(t), context.Background()
	groups, err := s.DuplicateGroups(ctx, ServerPlex, []string{"keep"})
	if err != nil {
		t.Fatal(err)
	}
	byKey := groupsByKey(groups)
	if !byKey["tmdb:438631"].Protected || byKey["tmdb:577922"].Protected {
		t.Errorf("tag rule: Dune %v, Tenet %v", byKey["tmdb:438631"].Protected, byKey["tmdb:577922"].Protected)
	}
	if groups, err = s.DuplicateGroups(ctx, ServerPlex, nil); err != nil || groupsByKey(groups)["tmdb:438631"].Protected {
		t.Errorf("without protected tags Dune is protected (%v)", err)
	}

	if _, err := s.DB().ExecContext(ctx, `UPDATE media SET protected_local = 1 WHERE plex_rating_key = '11'`); err != nil {
		t.Fatal(err)
	}
	if groups, err = s.DuplicateGroups(ctx, ServerPlex, []string{"keep"}); err != nil || !groupsByKey(groups)["tmdb:577922"].Protected {
		t.Errorf("Tenet protected locally: not protected (%v)", err)
	}
}

func TestAckDuplicate(t *testing.T) {
	s, ctx := duplicateStore(t), context.Background()
	now := time.Unix(1_790_812_800, 0)
	const dune = "tmdb:438631"

	if err := s.AckDuplicate(ctx, ServerPlex, dune, "11195,11205", now); err != nil {
		t.Fatal(err)
	}
	g, err := s.DuplicateGroup(ctx, ServerPlex, dune, nil)
	if err != nil {
		t.Fatal(err)
	}
	if g.AckedAt == nil || g.AckedAt.Unix() != now.Unix() {
		t.Errorf("acked at %v, want %v", g.AckedAt, now)
	}
	if err := s.AckDuplicate(ctx, ServerPlex, dune, "11195", now); !errors.Is(err, ErrStaleDuplicate) {
		t.Errorf("wrong fingerprint: %v, want ErrStaleDuplicate", err)
	}
	if err := s.AckDuplicate(ctx, ServerPlex, "tmdb:1", "1", now); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown group: %v, want ErrNotFound", err)
	}

	// A third version: the acknowledgement held for the former set only.
	versions := append(plexDuplicateVersions(), MediaVersion{Server: ServerPlex, VersionKey: "11206", ItemKey: "6894",
		GroupKey: dune, SizeBytes: 1})
	if err := s.ReplaceVersions(ctx, ServerPlex, versions); err != nil {
		t.Fatal(err)
	}
	if g, err = s.DuplicateGroup(ctx, ServerPlex, dune, nil); err != nil {
		t.Fatal(err)
	}
	if g.AckedAt != nil || g.Fingerprint != "11195,11205,11206" {
		t.Errorf("after a new version: acked %v, fingerprint %q", g.AckedAt, g.Fingerprint)
	}

	if err := s.AckDuplicate(ctx, ServerPlex, dune, "11195,11205,11206", now); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.UnackDuplicate(ctx, ServerPlex, dune); err != nil {
			t.Fatal(err)
		}
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM duplicate_acks`); n != 0 {
		t.Errorf("acks after unack: %d, want 0", n)
	}
	if g, err = s.DuplicateGroup(ctx, ServerPlex, dune, nil); err != nil || g.AckedAt != nil {
		t.Errorf("still acknowledged after unack: %v %v", g.AckedAt, err)
	}
}

func TestDuplicateGroupHiddenAndOrphan(t *testing.T) {
	s, ctx := duplicateStore(t), context.Background()
	// Items without media row: the group is shown, titled after the file of
	// its largest version.
	orphans := []MediaVersion{
		{Server: ServerPlex, VersionKey: "51", ItemKey: "998", GroupKey: "tmdb:5", File: "/films/Orphelin (2001).mkv", SizeBytes: 1_000_000_000},
		{Server: ServerPlex, VersionKey: "52", ItemKey: "999", GroupKey: "tmdb:5", File: "/films/Orphelin (2001) 4K.mkv", SizeBytes: 2_000_000_000},
	}
	if err := s.ReplaceVersions(ctx, ServerPlex, append(plexDuplicateVersions(), orphans...)); err != nil {
		t.Fatal(err)
	}
	g, err := s.DuplicateGroup(ctx, ServerPlex, "tmdb:5", nil)
	if err != nil {
		t.Fatal(err)
	}
	if g.MediaID != 0 || g.ArrID != 0 || g.Year != 0 || g.Title != "Orphelin (2001) 4K.mkv" || len(g.Versions) != 2 {
		t.Errorf("orphan group: %+v", g)
	}
	for _, v := range g.Versions {
		if v.MediaID != 0 {
			t.Errorf("orphan version %s linked to media %d", v.VersionKey, v.MediaID)
		}
	}

	// A hidden row is not joined: the version of the hidden item has no row.
	if _, err := s.DB().ExecContext(ctx, `UPDATE media SET match_state = 'hidden' WHERE plex_rating_key = '10'`); err != nil {
		t.Fatal(err)
	}
	tenet11 := mediaIDOf(t, s, `SELECT id FROM media WHERE plex_rating_key = '11'`)
	tn, err := s.DuplicateGroup(ctx, ServerPlex, "tmdb:577922", nil)
	if err != nil {
		t.Fatal(err)
	}
	if tn.MediaID != tenet11 || tn.Versions[0].VersionKey != "21" || tn.Versions[0].MediaID != 0 ||
		tn.Versions[1].MediaID != tenet11 {
		t.Errorf("hidden item still joined: %+v", tn)
	}

	if _, err := s.DuplicateGroups(ctx, "emby", nil); err == nil {
		t.Error("DuplicateGroups: unknown server accepted")
	}
	if _, err := s.DuplicateGroup(ctx, "emby", "tmdb:5", nil); err == nil {
		t.Error("DuplicateGroup: unknown server accepted")
	}
	if err := s.AckDuplicate(ctx, "emby", "tmdb:5", "51,52", time.Unix(1, 0)); err == nil {
		t.Error("AckDuplicate: unknown server accepted")
	}
	if err := s.UnackDuplicate(ctx, "emby", "tmdb:5"); err == nil {
		t.Error("UnackDuplicate: unknown server accepted")
	}
}

func TestDuplicateGroupNotFound(t *testing.T) {
	s, ctx := duplicateStore(t), context.Background()
	for _, key := range []string{"tmdb:999", ""} {
		if _, err := s.DuplicateGroup(ctx, ServerPlex, key, nil); !errors.Is(err, ErrNotFound) {
			t.Errorf("group %q: %v, want ErrNotFound", key, err)
		}
	}
}
