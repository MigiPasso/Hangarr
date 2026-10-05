package web

import (
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"arrstats/internal/secretbox"
	"arrstats/internal/upstream"
)

const testAPIKey = "0123456789abcdef0123456789abcdef"

func newConfigHarness(t *testing.T, env upstream.Values, withKey bool, opts ...func(*upstream.Manager)) *harness {
	t.Helper()
	return newHarness(t, nil, func(d *Deps) {
		reg := upstream.NewRegistry(nil)
		m := &upstream.Manager{Store: d.Store, Env: env, HistoryMode: "auto", Registry: reg,
			Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
		if withKey {
			box, err := secretbox.New([]byte(strings.Repeat("k", secretbox.KeySize)))
			if err != nil {
				t.Fatal(err)
			}
			m.Box = box
		}
		for _, o := range opts {
			o(m)
		}
		d.Upstreams, d.Services = reg, m
	})
}

func TestConfigPage(t *testing.T) {
	h := newConfigHarness(t, upstream.Values{upstream.RadarrURL: "http://env-radarr:7878"}, true)
	c := h.loggedIn()

	home := h.do(c, "GET", "/", nil, nil)
	if !strings.Contains(home.body, "Configuration incomplète (Sonarr, Radarr, Plex)") ||
		!strings.Contains(home.body, `href="/config">Configurer les services`) {
		t.Error("the banner must invite to configure the services")
	}
	page := h.do(c, "GET", "/config", nil, nil)
	if page.status != http.StatusOK {
		t.Fatalf("status %d", page.status)
	}
	for _, want := range []string{`name="sonarr.api_key" type="password" autocomplete="new-password"`,
		`value="http://env-radarr:7878" disabled`, "Défini par l'environnement (<code>RADARR_URL</code>)",
		`hx-post="/config/sonarr/test"`} {
		if !strings.Contains(page.body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(page.body, "Aucune clé maître") {
		t.Error("master key is set")
	}

	csrf := csrfFrom(t, page.body)
	r := h.do(c, "POST", "/config/sonarr", url.Values{"csrf_token": {csrf}, "sonarr.url": {"http://sonarr:8989"},
		"sonarr.api_key": {testAPIKey}}, map[string]string{"HX-Request": "true"})
	if r.status != http.StatusOK || !strings.Contains(r.body, "Enregistré.") {
		t.Fatalf("save: %d\n%s", r.status, r.body)
	}
	if strings.Contains(r.body, testAPIKey) || !strings.Contains(r.body, "••••cdef") {
		t.Error("the credential must never be sent back, only its masked hint")
	}
	if !strings.Contains(r.body, `id="config-banner" hx-swap-oob="true"`) ||
		!strings.Contains(r.body, "Configuration incomplète (Radarr, Plex)") {
		t.Error("the banner must be updated out of band")
	}
	if r := h.do(c, "GET", "/config", nil, nil); strings.Contains(r.body, testAPIKey) {
		t.Error("credential leaked in the page")
	}
	if got := h.srv.Upstreams.Get().Values[upstream.SonarrAPIKey]; got != testAPIKey {
		t.Errorf("clients not rebuilt: %q", got)
	}
	journal := h.do(c, "GET", "/journal?tab=security", nil, nil)
	if !strings.Contains(journal.body, "Configuration Sonarr modifiée") {
		t.Error("configuration change not in the security journal")
	}

	// Validation error without HTMX: 422, the typed URL is kept.
	r = h.do(c, "POST", "/config/plex", url.Values{"csrf_token": {csrf}, "plex.url": {"plex:32400"}}, nil)
	if r.status != http.StatusUnprocessableEntity || !strings.Contains(r.body, `value="plex:32400"`) ||
		!strings.Contains(r.body, "Adresse http(s) attendue") {
		t.Errorf("validation: %d", r.status)
	}
	// Connection test against an unreachable address.
	r = h.do(c, "POST", "/config/plex/test", url.Values{"csrf_token": {csrf}, "plex.url": {"http://127.0.0.1:1"},
		"plex.token": {"x"}}, map[string]string{"HX-Request": "true"})
	if !strings.Contains(r.body, `id="test-plex"`) || !strings.Contains(r.body, "Injoignable") {
		t.Errorf("test result: %s", r.body)
	}
	if r := h.do(c, "POST", "/config/prowlarr", url.Values{"csrf_token": {csrf}}, nil); r.status != http.StatusNotFound {
		t.Errorf("unknown service: %d", r.status)
	}
	// CSRF is required.
	if r := h.do(c, "POST", "/config/sonarr", url.Values{"sonarr.url": {"http://evil"}}, nil); r.status != http.StatusForbidden {
		t.Errorf("missing CSRF: %d", r.status)
	}
}

func TestConfigWithoutMasterKey(t *testing.T) {
	h := newConfigHarness(t, nil, false)
	c := h.loggedIn()
	page := h.do(c, "GET", "/config", nil, nil)
	if !strings.Contains(page.body, "Aucune clé maître n'est définie") {
		t.Error("missing master key warning")
	}
	if !strings.Contains(page.body, `placeholder="Non défini"
               disabled`) {
		t.Error("credential fields must be disabled without a master key")
	}
}

func TestConfigKeyInDataDir(t *testing.T) {
	h := newConfigHarness(t, nil, true, func(m *upstream.Manager) { m.KeyInDataDir = true })
	page := h.do(h.loggedIn(), "GET", "/config", nil, nil)
	if !strings.Contains(page.body, "Clé de chiffrement stockée avec les données.") {
		t.Error("missing warning about the generated key")
	}
	if strings.Contains(page.body, "Aucune clé maître") {
		t.Error("credentials can be saved with a generated key")
	}
}
