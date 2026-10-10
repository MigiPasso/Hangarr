package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"hangarr/internal/actions"
	"hangarr/internal/i18n"
	"hangarr/internal/secretbox"
	"hangarr/internal/store"
	"hangarr/internal/upstream"
)

// serviceSection returns the Configuration section of svc in body.
func serviceSection(t *testing.T, body, svc string) string {
	t.Helper()
	i := strings.Index(body, `id="service-`+svc+`"`)
	if i < 0 {
		t.Fatalf("no %s section in:\n%s", svc, body)
	}
	start := strings.LastIndex(body[:i], "<section")
	end := strings.Index(body[i:], "</section>")
	if start < 0 || end < 0 {
		t.Fatalf("%s section not delimited", svc)
	}
	return body[start : i+end]
}

// inputTags returns the fields of a section: its input elements, without the
// CSRF token.
func inputTags(sec string) []string {
	var tags []string
	for _, part := range strings.Split(sec, "<input")[1:] {
		if end := strings.Index(part, ">"); end >= 0 && !strings.Contains(part[:end], `type="hidden"`) {
			tags = append(tags, part[:end])
		}
	}
	return tags
}

func TestJellyfinCardDisabledByPlex(t *testing.T) {
	h := newConfigHarness(t, upstream.Values{upstream.PlexURL: "http://plex:32400", upstream.PlexToken: testAPIKey}, true)
	if _, err := h.srv.Services.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	c := h.loggedIn()
	page := h.do(c, "GET", "/config", nil, nil)
	jf := serviceSection(t, page.body, upstream.Jellyfin)
	if !strings.Contains(jf, "is-disabled") || !strings.Contains(jf, "Plex est configuré : videz sa configuration pour utiliser Jellyfin.") {
		t.Fatalf("Jellyfin section must explain why it is disabled:\n%s", jf)
	}
	if sonarr := serviceSection(t, page.body, upstream.Sonarr); strings.Contains(sonarr, "is-disabled") {
		t.Error("Sonarr is not disabled")
	}
	tags := inputTags(jf)
	if len(tags) == 0 {
		t.Fatal("no field in the Jellyfin section")
	}
	for _, tag := range tags {
		if !strings.Contains(tag, " disabled") {
			t.Errorf("field not disabled: %s", tag)
		}
	}
	if strings.Contains(jf, "Enregistrer</button>") || strings.Contains(jf, "Tester la connexion</button>") ||
		strings.Contains(jf, "clear_all") {
		t.Errorf("a disabled card with nothing saved offers no save, test or clear button:\n%s", jf)
	}
	if strings.Contains(jf, "Défini par l&#39;environnement : changez-le là.") {
		t.Errorf("no field of the Jellyfin section comes from the environment:\n%s", jf)
	}
	r := h.do(c, "POST", "/config/jellyfin", url.Values{"csrf_token": {csrfFrom(t, page.body)},
		"jellyfin.url": {"http://jellyfin:8096"}, "jellyfin.api_key": {testAPIKey}}, nil)
	if r.status != http.StatusUnprocessableEntity || !strings.Contains(r.body, `<p class="field-error" role="alert">Plex est configuré`) {
		t.Errorf("saving a disabled card: %d", r.status)
	}
	if got := h.srv.Upstreams.Get().Values[upstream.JellyfinURL]; got != "" {
		t.Errorf("Jellyfin saved while disabled: %q", got)
	}
}

func TestClearPlexEnablesJellyfin(t *testing.T) {
	h := newConfigHarness(t, nil, true)
	c := h.loggedIn()
	csrf := csrfFrom(t, h.do(c, "GET", "/config", nil, nil).body)
	htmx := map[string]string{"HX-Request": "true"}
	if r := h.do(c, "POST", "/config/plex", url.Values{"csrf_token": {csrf}, "plex.url": {"http://plex:32400"},
		"plex.token": {testAPIKey}}, htmx); r.status != http.StatusOK {
		t.Fatalf("save Plex: %d\n%s", r.status, r.body)
	}
	if jf := serviceSection(t, h.do(c, "GET", "/config", nil, nil).body, upstream.Jellyfin); !strings.Contains(jf, "is-disabled") {
		t.Fatal("Jellyfin must be disabled while Plex is configured")
	}
	// Plex cleared from its own section: Jellyfin is enabled again, and its
	// section is swapped out of band next to the saved one.
	r := h.do(c, "POST", "/config/plex", url.Values{"csrf_token": {csrf}, "plex.url": {""}, "plex.token.clear": {"1"}}, htmx)
	jf := serviceSection(t, r.body, upstream.Jellyfin)
	if !strings.Contains(jf, `hx-swap-oob="true"`) || strings.Contains(jf, "is-disabled") {
		t.Errorf("Jellyfin section after clearing Plex:\n%s", jf)
	}
	for _, tag := range inputTags(jf) {
		if strings.Contains(tag, " disabled") {
			t.Errorf("Jellyfin field still disabled: %s", tag)
		}
	}
	if plex := serviceSection(t, r.body, upstream.Plex); strings.Contains(plex, "hx-swap-oob") {
		t.Error("the saved section is the target of the swap, not an out-of-band one")
	}
}

func TestClearAllOnDisabledCard(t *testing.T) {
	ctx := context.Background()
	// Jellyfin is saved while Plex is not configured; then Plex comes from the
	// environment (set before the server starts): both are configured.
	h := newConfigHarness(t, nil, true, func(m *upstream.Manager) {
		jf := upstream.Input{Values: map[string]string{upstream.JellyfinURL: "http://jellyfin:8096",
			upstream.JellyfinAPIKey: testAPIKey}}
		if _, err := m.Save(ctx, upstream.Jellyfin, jf, store.AuthEvent{Username: "admin"}, time.Now()); err != nil {
			t.Fatal(err)
		}
		m.Env = upstream.Values{upstream.PlexURL: "http://plex:32400", upstream.PlexToken: testAPIKey}
		if _, err := m.Reload(ctx); err != nil {
			t.Fatal(err)
		}
	})
	c := h.loggedIn()
	if !h.srv.Upstreams.Get().ServerConflict {
		t.Fatal("Plex and Jellyfin are both configured: conflict expected")
	}
	if page := h.do(c, "GET", "/", nil, nil); !strings.Contains(page.body,
		"Plex et Jellyfin sont configurés tous les deux : synchronisation désactivée. Videz l&#39;un des deux.") {
		t.Error("missing the conflict banner")
	}
	config := h.do(c, "GET", "/config", nil, nil)
	jf := serviceSection(t, config.body, upstream.Jellyfin)
	if !strings.Contains(jf, "Effacer cette configuration") {
		t.Fatalf("the disabled card must offer to clear its configuration:\n%s", jf)
	}
	// Plex comes from the environment (URL and token; public URL empty): the
	// card says so, and offers nothing to clear.
	if plex := serviceSection(t, config.body, upstream.Plex); strings.Contains(plex, "clear_all") ||
		strings.Contains(plex, "Enregistrer</button>") || !strings.Contains(plex, "Défini par l&#39;environnement : changez-le là.") {
		t.Errorf("environment-set card:\n%s", plex)
	}
	r := h.do(c, "POST", "/config/jellyfin", url.Values{"csrf_token": {csrfFrom(t, config.body)}, "clear_all": {"1"}}, nil)
	if r.status != http.StatusOK {
		t.Fatalf("clear_all: %d\n%s", r.status, r.body)
	}
	if up := h.srv.Upstreams.Get(); up.ServerConflict || up.Values[upstream.JellyfinURL] != "" {
		t.Errorf("Jellyfin not cleared: conflict=%v url=%q", up.ServerConflict, up.Values[upstream.JellyfinURL])
	}
	if page := h.do(c, "GET", "/", nil, nil); strings.Contains(page.body, "sont configurés tous les deux") {
		t.Error("the conflict banner is still shown")
	}
}

// A saved credential the master key cannot open still counts as recorded: the
// disabled card offers to clear it.
func TestClearAllRemovesUnreadableSecret(t *testing.T) {
	ctx := context.Background()
	h := newConfigHarness(t, nil, true, func(m *upstream.Manager) {
		// Saved with the harness key, then read with another one.
		in := upstream.Input{Values: map[string]string{upstream.JellyfinAPIKey: testAPIKey}}
		if _, err := m.Save(ctx, upstream.Jellyfin, in, store.AuthEvent{Username: "admin"}, time.Now()); err != nil {
			t.Fatal(err)
		}
		box, err := secretbox.New([]byte(strings.Repeat("z", secretbox.KeySize)))
		if err != nil {
			t.Fatal(err)
		}
		m.Box = box
		m.Env = upstream.Values{upstream.PlexURL: "http://plex:32400", upstream.PlexToken: testAPIKey}
		if _, err := m.Reload(ctx); err != nil {
			t.Fatal(err)
		}
	})
	c := h.loggedIn()
	config := h.do(c, "GET", "/config", nil, nil)
	if jf := serviceSection(t, config.body, upstream.Jellyfin); !strings.Contains(jf, "Effacer cette configuration") {
		t.Fatalf("an unreadable saved key must be clearable:\n%s", jf)
	}
	r := h.do(c, "POST", "/config/jellyfin", url.Values{"csrf_token": {csrfFrom(t, config.body)}, "clear_all": {"1"}}, nil)
	if r.status != http.StatusOK {
		t.Fatalf("clear_all: %d\n%s", r.status, r.body)
	}
	if jf := serviceSection(t, h.do(c, "GET", "/config", nil, nil).body, upstream.Jellyfin); strings.Contains(jf, "Effacer cette configuration") ||
		strings.Contains(jf, "illisible") {
		t.Errorf("unreadable key not cleared:\n%s", jf)
	}
}

// Only the linked services (Plex, Jellyfin, Tautulli) can change with a save:
// saving Sonarr answers with its own section and the banner only.
func TestSavingSonarrSwapsOnlyItsSection(t *testing.T) {
	h := newConfigHarness(t, nil, true)
	c := h.loggedIn()
	csrf := csrfFrom(t, h.do(c, "GET", "/config", nil, nil).body)
	r := h.do(c, "POST", "/config/sonarr", url.Values{"csrf_token": {csrf}, "sonarr.url": {"http://sonarr:8989"},
		"sonarr.api_key": {testAPIKey}}, map[string]string{"HX-Request": "true"})
	if r.status != http.StatusOK || !strings.Contains(r.body, "Enregistré.") {
		t.Fatalf("save Sonarr: %d\n%s", r.status, r.body)
	}
	for _, svc := range []string{upstream.Plex, upstream.Jellyfin, upstream.Tautulli} {
		if strings.Contains(r.body, `id="service-`+svc+`"`) {
			t.Errorf("saving Sonarr must not swap the %s section", svc)
		}
	}
	if sonarr := serviceSection(t, r.body, upstream.Sonarr); strings.Contains(sonarr, "hx-swap-oob") {
		t.Error("the saved section is the target of the swap, not an out-of-band one")
	}
	if !strings.Contains(r.body, `id="config-banner" hx-swap-oob="true"`) {
		t.Error("the banner must still be swapped out of band")
	}
}

func TestTautulliDisabledWithJellyfin(t *testing.T) {
	h := newConfigHarness(t, nil, true)
	c := h.loggedIn()
	page := h.do(c, "GET", "/config", nil, nil)
	if r := h.do(c, "POST", "/config/jellyfin", url.Values{"csrf_token": {csrfFrom(t, page.body)},
		"jellyfin.url": {"http://jellyfin:8096"}, "jellyfin.api_key": {testAPIKey}}, nil); r.status != http.StatusOK {
		t.Fatalf("save Jellyfin: %d\n%s", r.status, r.body)
	}
	tautulli := serviceSection(t, h.do(c, "GET", "/config", nil, nil).body, upstream.Tautulli)
	if !strings.Contains(tautulli, "is-disabled") || !strings.Contains(tautulli, "Tautulli ne fonctionne qu&#39;avec Plex") {
		t.Errorf("Tautulli must be disabled while Jellyfin is used:\n%s", tautulli)
	}
}

func TestServerConflictActionMessage(t *testing.T) {
	got := explain(i18n.For(i18n.FR), actions.ErrServerConflict, "Échec : %s")
	if want := "Plex et Jellyfin sont configurés tous les deux : videz d'abord l'un des deux."; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// The reasons of upstream.Disabled are shown as they are: the French catalog
// must hold them.
func TestDisabledReasonsTranslated(t *testing.T) {
	plex := upstream.Values{upstream.PlexURL: "http://plex:32400", upstream.PlexToken: testAPIKey}
	jellyfin := upstream.Values{upstream.JellyfinURL: "http://jellyfin:8096", upstream.JellyfinAPIKey: testAPIKey}
	for _, reason := range []string{upstream.Disabled(plex, upstream.Jellyfin),
		upstream.Disabled(jellyfin, upstream.Plex), upstream.Disabled(jellyfin, upstream.Tautulli)} {
		if reason == "" || !i18n.Has(i18n.FR, reason) {
			t.Errorf("no French translation for %q", reason)
		}
	}
}
