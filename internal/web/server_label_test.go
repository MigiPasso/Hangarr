package web

import (
	"maps"
	"net/http/httptest"
	"strings"
	"testing"

	"hangarr/internal/upstream"
)

// With Jellyfin as the media server, the labels that named Plex name Jellyfin.
func TestServerLabelsJellyfin(t *testing.T) {
	e := newLibEnv(t, 0)
	const chip = `class="server-chip" href="/config" title="Serveur multimédia utilisé : %s" aria-label="Serveur multimédia utilisé : %s">%s</a>`
	if r := e.do(e.c, "GET", "/stats", nil, nil); !strings.Contains(r.body, strings.ReplaceAll(chip, "%s", "Plex")) {
		t.Error("top bar: the chip must name Plex")
	}
	jf := httptest.NewServer(e.mock.Jellyfin())
	t.Cleanup(jf.Close)
	v := maps.Clone(e.srv.Upstreams.Get().Values)
	delete(v, upstream.PlexURL)
	delete(v, upstream.PlexToken)
	v[upstream.JellyfinURL], v[upstream.JellyfinAPIKey] = jf.URL, "k"
	e.srv.Upstreams.Set(upstream.Build(v, "auto"))

	r := e.do(e.c, "GET", "/movies?match=server_only", nil, nil)
	for _, want := range []string{"Jellyfin et Sonarr/Radarr", "Jellyfin seul"} {
		if !strings.Contains(r.body, want) {
			t.Errorf("movies: missing %q", want)
		}
	}
	if r := e.do(e.c, "GET", "/health", nil, nil); !strings.Contains(r.body, "Dans Jellyfin mais pas dans Sonarr/Radarr") {
		t.Error("health: the gap must name Jellyfin")
	}
	if r := e.do(e.c, "GET", "/", nil, nil); strings.Contains(r.body, "configurez Tautulli") || !strings.Contains(r.body, "Jellyfin ne garde que") {
		t.Error("home: the duration note must not ask to configure Tautulli")
	}
	if r := e.do(e.c, "GET", "/sync/status", nil, map[string]string{"HX-Request": "true"}); !strings.Contains(r.body,
		strings.ReplaceAll(chip, "%s", "Jellyfin")) {
		t.Errorf("top bar: the chip must name Jellyfin:\n%s", r.body)
	}

	// No media server configured: no chip.
	delete(v, upstream.PlexToken)
	delete(v, upstream.JellyfinAPIKey)
	e.srv.Upstreams.Set(upstream.Build(v, "auto"))
	if r := e.do(e.c, "GET", "/stats", nil, nil); strings.Contains(r.body, "server-chip") {
		t.Error("top bar: no chip without a media server")
	}
}
