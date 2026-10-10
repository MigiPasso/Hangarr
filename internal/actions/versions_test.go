package actions

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"testing"
	"testing/fstest"

	"hangarr/internal/store"
	"hangarr/internal/upstream"
)

// Dune is in Plex twice (testdata/README.md): the Remux-2160p (media 1021) is
// Radarr's file, monitored; the WEB-DL 1080p (media 1022) is an extra copy.
const duneGroup = "tmdb:438631"

func (e *env) duplicate(server, group string) *store.DuplicateGroup {
	e.t.Helper()
	g, err := e.st.DuplicateGroup(context.Background(), server, group, e.svc.ProtectedTags)
	if err != nil {
		e.t.Fatalf("group %s: %v", group, err)
	}
	return g
}

// editRadarrMovie changes a movie of radarr/movie.json, as Radarr would
// answer it from now on.
func (e *env) editRadarrMovie(title string, edit func(map[string]any)) {
	e.t.Helper()
	var movies []map[string]any
	if err := json.Unmarshal(e.fsys["radarr/movie.json"].Data, &movies); err != nil {
		e.t.Fatal(err)
	}
	for _, m := range movies {
		if m["title"] == title {
			edit(m)
		}
	}
	b, _ := json.Marshal(movies)
	e.fsys["radarr/movie.json"] = &fstest.MapFile{Data: b}
}

// keepPlexMedia makes Plex list only these media ids for Dune from now on.
func (e *env) keepPlexMedia(ids ...float64) {
	e.t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(e.fsys["plex/metadata-102.json"].Data, &doc); err != nil {
		e.t.Fatal(err)
	}
	item := doc["MediaContainer"].(map[string]any)["Metadata"].([]any)[0].(map[string]any)
	var kept []any
	for _, m := range item["Media"].([]any) {
		for _, id := range ids {
			if m.(map[string]any)["id"] == id {
				kept = append(kept, m)
			}
		}
	}
	item["Media"] = kept
	b, _ := json.Marshal(doc)
	e.fsys["plex/metadata-102.json"] = &fstest.MapFile{Data: b}
}

func (e *env) deleteVersion(key string, accept bool) (*VersionResult, error) {
	g := e.duplicate(store.ServerPlex, duneGroup)
	return e.svc.DeleteVersion(context.Background(), VersionRequest{GroupKey: duneGroup, VersionKey: key,
		Fingerprint: g.Fingerprint, AcceptRadarr: accept, Username: "admin"})
}

func TestDeleteVersionPlex(t *testing.T) {
	e := newEnv(t, false)
	res, err := e.deleteVersion("1022", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.DryRun || res.RadarrFile || res.SizeFreed != 8000000000 || res.Title != "Dune" {
		t.Errorf("result = %+v", res)
	}
	calls := e.plexCalls()
	if len(calls) != 1 || calls[0].Path != "/library/metadata/102/media/1022" {
		t.Fatalf("plex calls = %+v", calls)
	}
	if calls := e.destructive(); len(calls) != 0 {
		t.Errorf("Radarr touched: %+v", calls)
	}
	a := e.lastAction()
	if !a.Success || a.Action != DeleteVersion || a.SizeFreedBytes != 8000000000 || a.Username != "admin" ||
		a.Title != "Dune (Dune.2021.1080p.WEB-DL.DDP5.1.H.264-GRP.mkv)" {
		t.Errorf("journal = %+v", a)
	}
	// One version left: the duplicate is solved until the next sync.
	if _, err := e.st.DuplicateGroup(context.Background(), store.ServerPlex, duneGroup, nil); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("group still listed: %v", err)
	}
	if e.mock.Hits("plex:refresh") == 0 {
		t.Error("Plex not refreshed after the deletion")
	}
}

func TestDeleteVersionDryRun(t *testing.T) {
	e := newEnv(t, true)
	res, err := e.deleteVersion("1022", false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.DryRun {
		t.Error("not marked as dry-run")
	}
	if calls := e.plexCalls(); len(calls) != 0 {
		t.Errorf("dry-run deleted: %+v", calls)
	}
	if a := e.lastAction(); !a.Success || !a.DryRun || a.Action != DeleteVersion {
		t.Errorf("journal = %+v", a)
	}
	if g := e.duplicate(store.ServerPlex, duneGroup); len(g.Versions) != 2 {
		t.Errorf("versions = %d after a dry-run", len(g.Versions))
	}
}

// Radarr's file of a monitored movie: Radarr would download it again.
func TestDeleteVersionRadarrMonitored(t *testing.T) {
	e := newEnv(t, false)
	if _, err := e.deleteVersion("1021", true); !errors.Is(err, ErrRadarrMonitored) {
		t.Fatalf("got %v", err)
	}
	if calls := e.plexCalls(); len(calls) != 0 {
		t.Errorf("deleted anyway: %+v", calls)
	}
	if a := e.lastAction(); a.Success || a.Action != DeleteVersion {
		t.Errorf("refusal not journaled: %+v", a)
	}
}

// Radarr's file of a movie Radarr does not monitor: allowed once the user
// confirmed that Radarr will see the movie missing.
func TestDeleteVersionRadarrUnmonitored(t *testing.T) {
	e := newEnv(t, false)
	e.editRadarrMovie("Dune", func(m map[string]any) { m["monitored"] = false })
	if _, err := e.deleteVersion("1021", false); !errors.Is(err, ErrRadarrConfirm) {
		t.Fatalf("no confirmation asked: %v", err)
	}
	if calls := e.plexCalls(); len(calls) != 0 {
		t.Errorf("deleted before the confirmation: %+v", calls)
	}
	if actions, _, _ := e.st.Actions(context.Background(), 0, 10); len(actions) != 0 {
		t.Errorf("a question is not an attempt: %+v", actions)
	}
	res, err := e.deleteVersion("1021", true)
	if err != nil {
		t.Fatal(err)
	}
	if !res.RadarrFile {
		t.Error("RadarrFile not set")
	}
	if calls := e.plexCalls(); len(calls) != 1 || calls[0].Path != "/library/metadata/102/media/1021" {
		t.Errorf("plex calls = %+v", calls)
	}
}

// Radarr has a file none of the versions is named like: which one it is is
// unknown, so a monitored movie is refused and an unmonitored one asks.
func TestDeleteVersionRadarrFileUnknown(t *testing.T) {
	e := newEnv(t, false)
	e.editRadarrMovie("Dune", func(m map[string]any) {
		m["movieFile"].(map[string]any)["relativePath"] = "Dune (2021) Bluray-1080p.mkv"
	})
	if _, err := e.deleteVersion("1022", true); !errors.Is(err, ErrRadarrFileUnknown) {
		t.Fatalf("got %v", err)
	}
	e.editRadarrMovie("Dune", func(m map[string]any) { m["monitored"] = false })
	if _, err := e.deleteVersion("1022", false); !errors.Is(err, ErrRadarrConfirm) {
		t.Fatalf("got %v", err)
	}
	// A file Radarr knows by no name at all counts as unknown too.
	e.editRadarrMovie("Dune", func(m map[string]any) {
		m["monitored"] = true
		delete(m["movieFile"].(map[string]any), "relativePath")
	})
	if _, err := e.deleteVersion("1022", true); !errors.Is(err, ErrRadarrFileUnknown) {
		t.Fatalf("got %v", err)
	}
	if calls := e.plexCalls(); len(calls) != 0 {
		t.Errorf("deleted: %+v", calls)
	}
}

// A movie Radarr no longer has, or no longer has a file for, sets no
// condition.
func TestDeleteVersionRadarrWithoutFile(t *testing.T) {
	e := newEnv(t, true)
	e.editRadarrMovie("Dune", func(m map[string]any) { m["hasFile"] = false })
	if _, err := e.deleteVersion("1021", false); err != nil {
		t.Fatalf("no file in Radarr: %v", err)
	}
	e.editRadarrMovie("Dune", func(m map[string]any) { m["id"] = 999 }) // gone from Radarr
	if _, err := e.deleteVersion("1021", false); err != nil {
		t.Fatalf("movie gone from Radarr: %v", err)
	}
}

func TestDeleteVersionProtected(t *testing.T) {
	e := newEnv(t, false)
	// Tag added in Radarr after the last sync: the live value wins.
	e.editRadarrMovie("Dune", func(m map[string]any) { m["tags"] = []int{1} })
	if _, err := e.deleteVersion("1022", false); !errors.Is(err, ErrProtected) {
		t.Fatalf("live tag ignored: %v", err)
	}
	e.editRadarrMovie("Dune", func(m map[string]any) { m["tags"] = []int{} })
	if _, err := e.st.DB().Exec(`UPDATE media SET protected_local = 1 WHERE title = 'Dune'`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.deleteVersion("1022", false); !errors.Is(err, ErrProtected) {
		t.Fatalf("local protection ignored: %v", err)
	}
	if calls := e.plexCalls(); len(calls) != 0 {
		t.Errorf("protected movie touched: %+v", calls)
	}
	if a := e.lastAction(); a.Success {
		t.Errorf("refusal not journaled: %+v", a)
	}
}

func TestDeleteVersionStale(t *testing.T) {
	e := newEnv(t, false)
	ctx := context.Background()
	g := e.duplicate(store.ServerPlex, duneGroup)
	for name, c := range map[string]struct {
		req  VersionRequest
		want error
	}{
		"other versions seen": {VersionRequest{GroupKey: duneGroup, VersionKey: "1022", Fingerprint: "1022"}, ErrStaleVersions},
		"unknown group":       {VersionRequest{GroupKey: "tmdb:1", VersionKey: "1022", Fingerprint: g.Fingerprint}, ErrNotFound},
		"unknown version":     {VersionRequest{GroupKey: duneGroup, VersionKey: "1", Fingerprint: g.Fingerprint}, ErrNotFound},
	} {
		if _, err := e.svc.DeleteVersion(ctx, c.req); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", name, err, c.want)
		}
	}

	// Plex no longer has the version: stale; it has only this one: the last.
	original := e.fsys["plex/metadata-102.json"]
	e.keepPlexMedia(1021)
	if _, err := e.deleteVersion("1022", false); !errors.Is(err, ErrStaleVersions) {
		t.Errorf("version gone from Plex: %v", err)
	}
	e.fsys["plex/metadata-102.json"] = original
	e.keepPlexMedia(1022)
	if _, err := e.deleteVersion("1022", false); !errors.Is(err, ErrLastVersion) {
		t.Errorf("last version: %v", err)
	}
	if calls := e.plexCalls(); len(calls) != 0 {
		t.Errorf("deleted: %+v", calls)
	}
}

func TestDeleteVersionPlexDenied(t *testing.T) {
	e := newEnv(t, false)
	e.mock.PlexDeletionDenied = true
	if _, err := e.deleteVersion("1022", false); !errors.Is(err, ErrPlexDeletionDenied) {
		t.Fatalf("got %v", err)
	}
	if g := e.duplicate(store.ServerPlex, duneGroup); len(g.Versions) != 2 {
		t.Error("version forgotten although Plex refused")
	}
}

// Two Plex items for one movie (two libraries): the copy is deleted whole,
// and its row leaves the library when only Plex knew it.
func TestDeleteVersionPlexWholeItem(t *testing.T) {
	e := newEnv(t, false)
	ctx := context.Background()
	if err := e.st.ApplyServerMatches(ctx, store.ServerPlex, []store.ServerLink{
		{MediaID: e.mediaID("movie", "Tenet"), Key: "103", Method: "tmdb"},
	}, []store.ServerOnlyItem{{Kind: "movie", Key: "105", Title: "Tenet", Year: 2020, TmdbID: 577922}}, nil, e.svc.now()); err != nil {
		t.Fatal(err)
	}
	if err := e.st.ReplaceVersions(ctx, store.ServerPlex, []store.MediaVersion{
		{Server: store.ServerPlex, VersionKey: "1031", ItemKey: "103", GroupKey: "tmdb:577922", File: "/data/movies/Tenet (2020)/Tenet (2020) Bluray-2160p.mkv"},
		{Server: store.ServerPlex, VersionKey: "1051", ItemKey: "105", GroupKey: "tmdb:577922", File: "/data/home/Vacances 2019/Vacances 2019.mp4"},
	}); err != nil {
		t.Fatal(err)
	}
	copyRow := e.mediaIDByKey("105")
	g := e.duplicate(store.ServerPlex, "tmdb:577922")
	if _, err := e.svc.DeleteVersion(ctx, VersionRequest{GroupKey: "tmdb:577922", VersionKey: "1051",
		Fingerprint: g.Fingerprint}); err != nil {
		t.Fatal(err)
	}
	if calls := e.plexCalls(); len(calls) != 1 || calls[0].Path != "/library/metadata/105" {
		t.Fatalf("plex calls = %+v", calls)
	}
	if _, err := e.st.MediaByID(ctx, copyRow); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Plex-only row of the deleted copy kept: %v", err)
	}
}

func (e *env) mediaIDByKey(key string) int64 {
	e.t.Helper()
	var id int64
	if err := e.st.DB().QueryRow(`SELECT id FROM media WHERE plex_rating_key = ?`, key).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

// Jellyfin deletes the folder of an item: a version sharing it with another
// one is refused (seen in the lab: both versions were deleted); a copy in a
// folder of its own is deleted with its item.
func TestDeleteVersionJellyfin(t *testing.T) {
	e := newEnv(t, false)
	e.useJellyfin()
	ctx := context.Background()
	const dune, dune1080, tenet, tenetCopy = "f0000000000000000000000000000102", "f0000000000000000000000000000202",
		"f0000000000000000000000000000103", "f0000000000000000000000000000203"
	if err := e.st.ReplaceVersions(ctx, store.ServerJellyfin, []store.MediaVersion{
		{Server: store.ServerJellyfin, VersionKey: dune, ItemKey: dune, GroupKey: duneGroup},
		{Server: store.ServerJellyfin, VersionKey: dune1080, ItemKey: dune, GroupKey: duneGroup},
		{Server: store.ServerJellyfin, VersionKey: tenet, ItemKey: tenet, GroupKey: "tmdb:577922"},
		{Server: store.ServerJellyfin, VersionKey: tenetCopy, ItemKey: tenetCopy, GroupKey: "tmdb:577922"},
	}); err != nil {
		t.Fatal(err)
	}
	del := func(group, key string) error {
		g := e.duplicate(store.ServerJellyfin, group)
		_, err := e.svc.DeleteVersion(ctx, VersionRequest{GroupKey: group, VersionKey: key, Fingerprint: g.Fingerprint})
		return err
	}
	if err := del(duneGroup, dune1080); !errors.Is(err, ErrSameFolder) {
		t.Fatalf("grouped version: %v", err)
	}
	if len(e.jellyfinCalls(http.MethodDelete)) != 0 {
		t.Fatal("grouped version deleted")
	}
	if err := del("tmdb:577922", tenetCopy); err != nil {
		t.Fatal(err)
	}
	calls := e.jellyfinCalls(http.MethodDelete)
	if len(calls) != 1 || calls[0].Path != "/Items/"+tenetCopy {
		t.Fatalf("jellyfin calls = %+v", calls)
	}
	if e.mock.Hits("jellyfin:refresh") == 0 {
		t.Error("Jellyfin not refreshed after the deletion")
	}
}

// Two copies in one folder but not grouped by Jellyfin: the folder would go
// with either of them.
func TestDeleteVersionJellyfinSameFolderItems(t *testing.T) {
	e := newEnv(t, false)
	e.useJellyfin()
	ctx := context.Background()
	var doc map[string]any
	_ = json.Unmarshal(e.fsys["jellyfin/versions.json"].Data, &doc)
	for _, it := range doc["Items"].([]any) {
		item := it.(map[string]any)
		if item["Id"] == "f0000000000000000000000000000203" {
			src := item["MediaSources"].([]any)[0].(map[string]any)
			src["Path"] = "/data/movies/Tenet (2020)/Tenet (2020) copy.mkv"
		}
	}
	b, _ := json.Marshal(doc)
	e.fsys["jellyfin/versions.json"] = &fstest.MapFile{Data: b}
	if err := e.st.ReplaceVersions(ctx, store.ServerJellyfin, []store.MediaVersion{
		{Server: store.ServerJellyfin, VersionKey: "f0000000000000000000000000000103", ItemKey: "f0000000000000000000000000000103", GroupKey: "tmdb:577922"},
		{Server: store.ServerJellyfin, VersionKey: "f0000000000000000000000000000203", ItemKey: "f0000000000000000000000000000203", GroupKey: "tmdb:577922"},
	}); err != nil {
		t.Fatal(err)
	}
	g := e.duplicate(store.ServerJellyfin, "tmdb:577922")
	_, err := e.svc.DeleteVersion(ctx, VersionRequest{GroupKey: "tmdb:577922",
		VersionKey: "f0000000000000000000000000000203", Fingerprint: g.Fingerprint})
	if !errors.Is(err, ErrSameFolder) {
		t.Fatalf("got %v", err)
	}
}

func TestDeleteVersionServerConflict(t *testing.T) {
	e := newEnv(t, false)
	g := e.duplicate(store.ServerPlex, duneGroup)
	v := maps.Clone(e.plexValues)
	v[upstream.JellyfinURL], v[upstream.JellyfinAPIKey] = e.jellyfinURL, "k"
	e.svc.Upstreams.Set(upstream.Build(v, "auto"))
	_, err := e.svc.DeleteVersion(context.Background(), VersionRequest{GroupKey: duneGroup, VersionKey: "1022",
		Fingerprint: g.Fingerprint})
	if !errors.Is(err, ErrServerConflict) {
		t.Fatalf("got %v", err)
	}
	if calls := e.mock.Calls(); len(calls) != 0 {
		t.Errorf("nothing must be sent: %+v", calls)
	}
}
