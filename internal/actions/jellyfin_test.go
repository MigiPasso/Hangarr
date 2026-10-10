package actions

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"testing"

	"hangarr/internal/mockarr"
	"hangarr/internal/store"
	"hangarr/internal/upstream"
)

const vacancesJF = "f0000000000000000000000000000105"

// useJellyfin makes Jellyfin the only media server (Plex is removed from the
// values): Vacances 2019 becomes a Jellyfin-only item, Home Videos stays known
// by Plex only (hidden).
func (e *env) useJellyfin() {
	e.t.Helper()
	if _, err := e.st.DB().Exec(`UPDATE media SET jellyfin_id = ? WHERE title = 'Vacances 2019'`, vacancesJF); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.st.DB().Exec(`UPDATE media SET match_state = 'hidden' WHERE title = 'Home Videos'`); err != nil {
		e.t.Fatal(err)
	}
	v := upstream.Values{}
	for k, val := range e.svc.Upstreams.Get().Values {
		v[k] = val
	}
	delete(v, upstream.PlexURL)
	delete(v, upstream.PlexToken)
	v[upstream.JellyfinURL], v[upstream.JellyfinAPIKey] = e.jellyfinURL, "k"
	e.svc.Upstreams.Set(upstream.Build(v, "auto"))
	if e.svc.Upstreams.Get().MediaServer != upstream.Jellyfin {
		e.t.Fatal("Jellyfin not in use")
	}
}

func (e *env) jellyfinCalls(method string) []mockarr.Call {
	var out []mockarr.Call
	for _, c := range e.mock.Calls() {
		if c.Service == "jellyfin" && c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

func TestDeleteJellyfinOnly(t *testing.T) {
	e := newEnv(t, false)
	e.useJellyfin()
	ctx := context.Background()
	movie := e.mediaID("movie", "Vacances 2019")
	res, err := e.svc.Delete(ctx, Request{MediaID: movie, Username: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != DeleteJellyfin || res.DryRun {
		t.Errorf("result: %+v", res)
	}
	if calls := e.jellyfinCalls(http.MethodDelete); len(calls) != 1 || calls[0].Path != "/Items/"+vacancesJF {
		t.Fatalf("jellyfin calls: %+v", calls)
	}
	if len(e.plexCalls()) != 0 || len(e.destructive()) != 0 {
		t.Error("Plex, Sonarr and Radarr must not be called")
	}
	if _, err := e.st.MediaByID(ctx, movie); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("local row must be removed: %v", err)
	}
	if a := e.lastAction(); !a.Success || a.Action != DeleteJellyfin || !a.DeleteFiles || a.Username != "admin" {
		t.Errorf("journal: %+v", a)
	}
}

func TestDeleteJellyfinDeniedAndDryRun(t *testing.T) {
	e := newEnv(t, false)
	e.useJellyfin()
	ctx := context.Background()
	movie := e.mediaID("movie", "Vacances 2019")
	e.mock.JellyfinDeletionDenied = true
	if _, err := e.svc.Delete(ctx, Request{MediaID: movie}); !errors.Is(err, ErrJellyfinDeletionDenied) {
		t.Errorf("denied: %v", err)
	}
	if a := e.lastAction(); a.Success || a.Action != DeleteJellyfin {
		t.Errorf("journal of the refusal: %+v", a)
	}
	if _, err := e.st.MediaByID(ctx, movie); err != nil {
		t.Errorf("refused deletion must keep the row: %v", err)
	}

	dry := newEnv(t, true)
	dry.useJellyfin()
	res, err := dry.svc.Delete(ctx, Request{MediaID: dry.mediaID("movie", "Vacances 2019")})
	if err != nil || !res.DryRun || res.Action != DeleteJellyfin {
		t.Fatalf("%+v %v", res, err)
	}
	if calls := dry.jellyfinCalls(http.MethodDelete); len(calls) != 0 {
		t.Errorf("dry-run must not call Jellyfin: %+v", calls)
	}
}

// Items only the server not in use knows are left alone, even though its
// client is still configured.
func TestInactiveServerItemsUntouched(t *testing.T) {
	e := newEnv(t, false)
	e.useJellyfin()
	ctx := context.Background()
	show := e.mediaID("series", "Home Videos")
	if _, err := e.svc.Delete(ctx, Request{MediaID: show, ConfirmTitle: "home videos"}); !errors.Is(err, ErrInactiveServer) {
		t.Errorf("delete: %v", err)
	}
	if _, err := e.svc.SetProtected(ctx, ProtectRequest{MediaID: show, Protect: true}); !errors.Is(err, ErrInactiveServer) {
		t.Errorf("protect: %v", err)
	}
	if len(e.mock.Calls()) != 0 {
		t.Errorf("nothing must be sent: %+v", e.mock.Calls())
	}
	// Back on Plex, before any sync: the Jellyfin-only item is refused too.
	e.svc.Upstreams.Set(upstream.Build(maps.Clone(e.plexValues), "auto"))
	if _, err := e.st.DB().Exec(`UPDATE media SET plex_rating_key = NULL WHERE title = 'Vacances 2019'`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Delete(ctx, Request{MediaID: e.mediaID("movie", "Vacances 2019")}); !errors.Is(err, ErrInactiveServer) {
		t.Errorf("Jellyfin item while Plex is used: %v", err)
	}
	// Local protection of a Jellyfin-only item while Jellyfin is used.
	e.useJellyfin()
	res, err := e.svc.SetProtected(ctx, ProtectRequest{MediaID: e.mediaID("movie", "Vacances 2019"), Protect: true})
	if err != nil || !res.Local {
		t.Errorf("local protection: %+v %v", res, err)
	}
}

// After deleting files, Jellyfin rescans its libraries once per batch.
func TestJellyfinRefreshAfterDelete(t *testing.T) {
	e := newEnv(t, false)
	e.useJellyfin()
	items := e.svc.DeleteBatch(context.Background(), []int64{e.mediaID("movie", "Tenet"), e.mediaID("series", "Arcane")},
		BatchOptions{DeleteFiles: true})
	for _, it := range items {
		if !it.OK {
			t.Fatalf("batch item: %+v", it)
		}
	}
	if n := e.mock.Hits("jellyfin:refresh"); n != 1 {
		t.Errorf("Jellyfin refreshes: %d, want 1", n)
	}
	if n := e.mock.Hits("plex:refresh"); n != 0 {
		t.Errorf("Plex must not be refreshed: %d", n)
	}
	if _, err := e.svc.Delete(context.Background(), Request{MediaID: e.mediaID("movie", "Dune"), DeleteFiles: true}); err != nil {
		t.Fatal(err)
	}
	if n := e.mock.Hits("jellyfin:refresh"); n != 2 {
		t.Errorf("single deletion refresh: %d", n)
	}
}

// Plex and Jellyfin both configured: which server owns an item is not
// settled, nothing goes through either; Sonarr/Radarr items are not concerned.
func TestServerConflictBlocksServerActions(t *testing.T) {
	e := newEnv(t, false)
	v := maps.Clone(e.plexValues)
	v[upstream.JellyfinURL], v[upstream.JellyfinAPIKey] = e.jellyfinURL, "k"
	e.svc.Upstreams.Set(upstream.Build(v, "auto"))
	if !e.svc.Upstreams.Get().ServerConflict {
		t.Fatal("both servers must be a conflict")
	}
	ctx := context.Background()
	movie := e.mediaID("movie", "Vacances 2019")
	if _, err := e.svc.Delete(ctx, Request{MediaID: movie}); !errors.Is(err, ErrServerConflict) {
		t.Errorf("delete through the server: %v", err)
	}
	if _, err := e.svc.SetProtected(ctx, ProtectRequest{MediaID: movie, Protect: true}); !errors.Is(err, ErrServerConflict) {
		t.Errorf("local protection: %v", err)
	}
	if calls := e.mock.Calls(); len(calls) != 0 {
		t.Errorf("nothing must be sent: %+v", calls)
	}
	if _, err := e.svc.Delete(ctx, Request{MediaID: e.mediaID("movie", "Tenet")}); err != nil {
		t.Errorf("a Radarr movie is still deleted through Radarr: %v", err)
	}
}
