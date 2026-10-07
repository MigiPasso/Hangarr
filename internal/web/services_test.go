package web

import (
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"hangarr/internal/secretbox"
	"hangarr/internal/upstream"
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
	if !strings.Contains(home.body, "Services non configurés (Sonarr, Radarr, Plex)") ||
		!strings.Contains(home.body, `href="/config">Configurer les services`) {
		t.Error("the banner must invite to configure the services")
	}
	page := h.do(c, "GET", "/config", nil, nil)
	if page.status != http.StatusOK {
		t.Fatalf("status %d", page.status)
	}
	for _, want := range []string{`name="sonarr.api_key" type="password" autocomplete="new-password"`,
		`value="http://env-radarr:7878" disabled`, "Défini par l&#39;environnement (<code>RADARR_URL</code>)",
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
		!strings.Contains(r.body, "Services non configurés (Radarr, Plex)") {
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
	if !strings.Contains(page.body, "Aucune clé maître n&#39;est définie") {
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

func TestTautulliWebhook(t *testing.T) {
	e := newLibEnv(t, 0)
	c := e.client() // no session: the webhook is public
	post := func(token string) resp {
		h := map[string]string{"Content-Type": "application/json"}
		if token != "" {
			h["X-Hangarr-Token"] = token
		}
		return e.do(c, "POST", "/webhooks/tautulli", url.Values{"payload": {"ignored"}}, h)
	}
	if r := post("anything"); r.status != http.StatusNotFound {
		t.Errorf("without secret the webhook must not exist: %d", r.status)
	}
	secret := strings.Repeat("s", 40)
	up := *e.srv.Upstreams.Get()
	vals := upstream.Values{}
	for k, v := range up.Values {
		vals[k] = v
	}
	vals[upstream.TautulliWebhook] = secret
	up.Values = vals
	e.srv.Upstreams.Set(&up)

	if r := post(""); r.status != http.StatusUnauthorized {
		t.Errorf("missing token: %d", r.status)
	}
	if r := post(secret + "x"); r.status != http.StatusUnauthorized {
		t.Errorf("wrong token: %d", r.status)
	}
	if r := post(secret); r.status != http.StatusAccepted || !strings.Contains(r.body, "accepted") {
		t.Errorf("valid token: %d %s", r.status, r.body)
	}
	// Header of Cleanarr (former name): still accepted, checked the same way.
	legacy := func(token string) resp {
		return e.do(c, "POST", "/webhooks/tautulli", nil, map[string]string{"X-Cleanarr-Token": token})
	}
	if r := legacy(secret); r.status != http.StatusAccepted {
		t.Errorf("legacy header: %d", r.status)
	}
	if r := legacy(secret + "x"); r.status != http.StatusUnauthorized {
		t.Errorf("legacy header, wrong token: %d", r.status)
	}
	if r := e.do(c, "GET", "/webhooks/tautulli", nil, nil); r.status == http.StatusAccepted {
		t.Error("GET must not trigger anything")
	}
	for i := 0; i < webhookPerMinute; i++ {
		post(secret)
	}
	if r := post(secret); r.status != http.StatusTooManyRequests || r.header.Get("Retry-After") == "" {
		t.Errorf("rate limit: %d", r.status)
	}
}

func TestWebhookSecretGeneration(t *testing.T) {
	h := newConfigHarness(t, nil, true)
	c := h.loggedIn()
	page := h.do(c, "GET", "/config", nil, nil)
	if !strings.Contains(page.body, ">Générer un secret</button>") || !strings.Contains(page.body, "/webhooks/tautulli</code>") {
		t.Fatal("missing webhook instructions")
	}
	csrf := csrfFrom(t, page.body)
	h.do(c, "POST", "/config/tautulli", url.Values{"csrf_token": {csrf}, "tautulli.url": {"http://tautulli:8181"}}, nil)
	r := h.do(c, "POST", "/config/tautulli/webhook-secret", url.Values{"csrf_token": {csrf}}, map[string]string{"HX-Request": "true"})
	m := regexp.MustCompile(`"X-Hangarr-Token": "([A-Za-z0-9_-]{43})"`).FindStringSubmatch(r.body)
	if m == nil {
		t.Fatalf("secret not shown:\n%s", r.body)
	}
	if got := h.srv.Upstreams.Get(); got.WebhookSecret() != m[1] || got.Values[upstream.TautulliURL] != "http://tautulli:8181" {
		t.Errorf("secret not saved or URL lost: %+v", got.Values)
	}
	if page := h.do(c, "GET", "/config", nil, nil); strings.Contains(page.body, m[1]) ||
		!strings.Contains(page.body, ">Générer un nouveau secret</button>") {
		t.Error("the secret must be shown only once")
	}
}
