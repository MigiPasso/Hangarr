package upstream

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"hangarr/internal/store"
)

func jellyfinValues(withPlex bool) Values {
	urls := map[string]string{Sonarr: "http://s", Radarr: "http://r", Jellyfin: "http://j"}
	if withPlex {
		urls[Plex] = "http://p"
	}
	v := TestValues(urls, "k")
	if !withPlex {
		delete(v, PlexToken)
	}
	delete(v, TautulliAPIKey) // Tautulli is not needed with Jellyfin
	return v
}

func TestBuildMediaServer(t *testing.T) {
	c := Build(jellyfinValues(false), "auto")
	if !c.Ready() || c.MediaServer != Jellyfin || c.ServerLabel() != "Jellyfin" || c.ServerConflict {
		t.Fatalf("Jellyfin alone: ready=%v missing=%v server=%s", c.Ready(), c.Missing, c.MediaServer)
	}
	if c.HistorySource != "jellyfin" || !c.JellyfinHistory() || c.PlexHistory() || c.HistoryClient() != nil ||
		c.Merged() || !c.NoDurations() {
		t.Errorf("Jellyfin history: %s", c.HistorySource)
	}
	// HISTORY_SOURCE concerns Plex only.
	if c := Build(jellyfinValues(false), "tautulli"); !c.Ready() || c.HistorySource != "jellyfin" {
		t.Errorf("HISTORY_SOURCE=tautulli with Jellyfin: %v %s", c.Missing, c.HistorySource)
	}

	// Both configured: a conflict, nothing is synced until one is cleared.
	both := jellyfinValues(true)
	if c := Build(both, "plex"); c.MediaServer != Plex || !c.ServerConflict || c.Ready() || c.JellyfinHistory() || !c.PlexHistory() {
		t.Errorf("both: server=%s conflict=%v ready=%v", c.MediaServer, c.ServerConflict, c.Ready())
	}
}

// Plex and Jellyfin exclude each other, and Tautulli only feeds Plex.
func TestDisabled(t *testing.T) {
	plexOnly := jellyfinValues(true)
	delete(plexOnly, JellyfinURL)
	delete(plexOnly, JellyfinAPIKey)
	jellyfinOnly := jellyfinValues(false)
	const plexWhy = "Plex is configured: clear it to use Jellyfin."
	const jellyfinWhy = "Jellyfin is configured: clear it to use Plex."
	const tautulliWhy = "Tautulli only works with Plex: it is not read while Jellyfin is used."
	for _, c := range []struct {
		name, svc, want string
		v               Values
	}{
		{"Plex only", Plex, "", plexOnly},
		{"Plex only", Jellyfin, plexWhy, plexOnly},
		{"Plex only", Tautulli, "", plexOnly},
		{"Jellyfin only", Plex, jellyfinWhy, jellyfinOnly},
		{"Jellyfin only", Jellyfin, "", jellyfinOnly},
		{"Jellyfin only", Tautulli, tautulliWhy, jellyfinOnly},
		{"none", Plex, "", Values{}},
		{"none", Jellyfin, "", Values{}},
		{"none", Tautulli, "", Values{}},
	} {
		if got := Disabled(c.v, c.svc); got != c.want {
			t.Errorf("%s, %s: %q", c.name, c.svc, got)
		}
	}
}

func TestCheckJellyfin(t *testing.T) {
	product := "Jellyfin Server"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/System/Info" || r.Header.Get("Authorization") != `MediaBrowser Token="k"` {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"Version":"12.0.1","ProductName":"` + product + `","Id":"5e0c1e6a3b2f4d7c9a8b1c2d3e4f5a6b"}`))
	}))
	defer srv.Close()
	c := Build(Values{JellyfinURL: srv.URL, JellyfinAPIKey: "k"}, "auto")
	if v, err := Check(context.Background(), c, Jellyfin); err != nil || v != "12.0.1" {
		t.Fatalf("check: %q %v", v, err)
	}
	product = "Emby Server"
	_, err := Check(context.Background(), c, Jellyfin)
	var we *WrongServiceError
	if !errors.As(err, &we) || we.Got != "Emby Server" {
		t.Errorf("Emby must be refused: %v", err)
	}
	bad := Build(Values{JellyfinURL: srv.URL, JellyfinAPIKey: "nope"}, "auto")
	if _, err := Check(context.Background(), bad, Jellyfin); err == nil || !strings.Contains(Describe(fr, err), "401") {
		t.Errorf("wrong key: %v", err)
	}
}

func TestJellyfinPublicURL(t *testing.T) {
	for raw, page := range map[string]bool{
		"https://jf.example.com":            false,
		"https://example.com/jellyfin":      false,
		"https://jf.example.com/web":        true,
		"https://jf.example.com/web/#/home": true,
	} {
		if got := pageOfTheApp(Jellyfin, raw); got != page {
			t.Errorf("%s: %v", raw, got)
		}
	}
	c := Build(Values{JellyfinURL: "http://jellyfin:8096"}, "auto")
	if c.JellyfinWebURL() != "http://jellyfin:8096" {
		t.Errorf("web URL: %s", c.JellyfinWebURL())
	}
}

// Changing the API address of Jellyfin requires its key again.
func TestJellyfinKeptCredentials(t *testing.T) {
	m := newManager(t, nil, newBox(t, "k"))
	save(t, m, Jellyfin, input(JellyfinURL, "http://jellyfin:8096", JellyfinAPIKey, apiKey))
	_, err := m.Save(context.Background(), Jellyfin, input(JellyfinURL, "http://evil:8096"), store.AuthEvent{Username: "admin"}, time.Now())
	var verr ValidationError
	if !errors.As(err, &verr) || verr[JellyfinAPIKey] == "" {
		t.Fatalf("key kept for another host: %v", err)
	}
}

// Plex and Jellyfin exclude each other. A refused save writes nothing; clearing
// is always allowed.
func TestExclusiveSave(t *testing.T) {
	m := newManager(t, nil, newBox(t, "k"))
	ctx := context.Background()
	plexIn := input(PlexURL, "http://plex:32400", PlexToken, apiKey)
	jellyfinIn := input(JellyfinURL, "http://jellyfin:8096", JellyfinAPIKey, apiKey)
	save(t, m, Plex, plexIn)
	count := func() int {
		var n int
		_ = m.Store.DB().QueryRow(`SELECT COUNT(*) FROM app_settings`).Scan(&n)
		return n
	}
	before := count()
	var verr ValidationError
	if _, err := m.Save(ctx, Jellyfin, jellyfinIn, store.AuthEvent{Username: "admin"}, time.Now()); !errors.As(err, &verr) ||
		verr[JellyfinURL] != "Plex is configured: clear it to use Jellyfin." {
		t.Fatalf("Jellyfin with Plex configured: %v", err)
	}
	if count() != before {
		t.Error("a refused save wrote settings")
	}
	if st := state(t, m, Jellyfin); st.Disabled == "" {
		t.Error("Jellyfin must be disabled while Plex is configured")
	}

	clearPlex := input(PlexURL, "")
	clearPlex.Clear[PlexToken] = true
	save(t, m, Plex, clearPlex)
	save(t, m, Jellyfin, jellyfinIn)
	if st := state(t, m, Plex); st.Disabled != "Jellyfin is configured: clear it to use Plex." {
		t.Errorf("Plex with Jellyfin configured: %q", st.Disabled)
	}
	if _, err := m.Save(ctx, Tautulli, input(TautulliURL, "http://tautulli:8181"), store.AuthEvent{}, time.Now()); !errors.As(err, &verr) || verr[TautulliURL] == "" {
		t.Errorf("Tautulli with Jellyfin configured: %v", err)
	}
	if _, err := m.Save(ctx, Plex, plexIn, store.AuthEvent{}, time.Now()); !errors.As(err, &verr) || verr[PlexURL] == "" {
		t.Errorf("Plex with Jellyfin configured: %v", err)
	}
	if _, err := m.Test(ctx, Plex, plexIn); !errors.As(err, &verr) || verr[PlexURL] == "" {
		t.Errorf("test of a disabled service: %v", err)
	}

	// Plex defined by the environment: Jellyfin is refused too.
	env := newManager(t, Values{PlexURL: "http://plex:32400", PlexToken: apiKey}, newBox(t, "k"))
	if _, err := env.Save(ctx, Jellyfin, jellyfinIn, store.AuthEvent{}, time.Now()); !errors.As(err, &verr) || verr[JellyfinURL] == "" {
		t.Errorf("Jellyfin with Plex from the environment: %v", err)
	}
}
