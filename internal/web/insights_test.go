package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestDashboard(t *testing.T) {
	e := newLibEnv(t, 0)
	r := e.do(e.c, "GET", "/", nil, nil)
	if r.status != http.StatusOK {
		t.Fatalf("status %d", r.status)
	}
	for _, want := range []string{"Tableau de bord", `class="tile-value">5<`, "91,4 Go", `<svg class="chart"`,
		"Heures regardées par jour", "Breaking Bad", "Derniers visionnages", "/media", "libres sur 8,0 To",
		`aria-label="Heures regardées par jour sur 30 jours"`} {
		if !strings.Contains(r.body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(r.body, "En cours de lecture") {
		t.Error("no live activity block (user decision)")
	}
	// The top block alone, via HTMX.
	r = e.do(e.c, "GET", "/?top=all", nil, map[string]string{"HX-Request": "true", "HX-Target": "top"})
	if strings.Contains(r.body, "<html") || !strings.Contains(r.body, "Les plus regardés") {
		t.Errorf("top partial:\n%s", r.body)
	}
	// Disk space is cached: a second view does not call the *arr apps again.
	e.mock.Reset()
	e.do(e.c, "GET", "/", nil, nil)
	if n := e.mock.Hits("sonarr:diskspace") + e.mock.Hits("radarr:diskspace"); n != 0 {
		t.Errorf("disk space not cached: %d calls", n)
	}
}

func TestStatsPages(t *testing.T) {
	e := newLibEnv(t, 0)
	r := e.do(e.c, "GET", "/stats?p=all", nil, nil)
	if r.status != http.StatusOK {
		t.Fatalf("status %d", r.status)
	}
	for _, want := range []string{`class="chart heatmap"`, "Genres stockés vs genres regardés", "Coût par lecture",
		"Classement des spectateurs", `href="/stats/users/1001"`, "Qui regarde quoi", "Films commencés, jamais finis",
		"Technique", "Abandonnés", "The Expanse"} {
		if !strings.Contains(r.body, want) {
			t.Errorf("missing %q", want)
		}
	}
	r = e.do(e.c, "GET", "/stats?p=all&user=1001&by=hours", nil, nil)
	if !strings.Contains(r.body, "· Alice") || !strings.Contains(r.body, "Les plus regardés (heures)") {
		t.Error("user filter / sort by hours")
	}
	r = e.do(e.c, "GET", "/stats/users/1002", nil, nil)
	if r.status != http.StatusOK || !strings.Contains(r.body, "<h1>Bob</h1>") || !strings.Contains(r.body, "Heures regardées par mois") ||
		!strings.Contains(r.body, "Shōgun") {
		t.Errorf("user page: %d", r.status)
	}
	if r := e.do(e.c, "GET", "/stats/users/4242", nil, nil); r.status != http.StatusNotFound {
		t.Errorf("unknown user: %d", r.status)
	}
}

func TestCleanupPageAndScoreSettings(t *testing.T) {
	e := newLibEnv(t, 0)
	r := e.do(e.c, "GET", "/cleanup", nil, nil)
	if r.status != http.StatusOK || !strings.Contains(r.body, "Saisons jamais vues") || !strings.Contains(r.body, "Saison 2") ||
		strings.Contains(r.body, ">Severance<") {
		t.Fatalf("cleanup page: %d", r.status)
	}
	if i, j := strings.Index(r.body, ">Tenet<"), strings.Index(r.body, ">Interstellar<"); i < 0 || j < 0 || i > j {
		t.Error("candidates must be sorted by score")
	}
	if r := e.do(e.c, "GET", "/cleanup?kind=movie", nil, nil); strings.Contains(r.body, ">The Expanse<") {
		t.Error("kind filter")
	}

	page := e.do(e.c, "GET", "/settings", nil, nil)
	tok := csrfFrom(t, page.body)
	bad := url.Values{"csrf_token": {tok}, "threshold": {"x"}, "never_watched": {"1"}, "continuing": {"1"}, "window_days": {"1"}}
	if r := e.do(e.c, "POST", "/settings/score", bad, nil); r.status != http.StatusUnprocessableEntity {
		t.Errorf("invalid score settings: %d", r.status)
	}
	good := url.Values{"csrf_token": {tok}, "threshold": {"0,2"}, "never_watched": {"3"}, "continuing": {"0.5"}, "window_days": {"60"}}
	r = e.do(e.c, "POST", "/settings/score", good, map[string]string{"HX-Request": "true"})
	if r.status != http.StatusOK || !strings.Contains(r.body, "scores recalculés") || !strings.Contains(r.body, `value="0.2"`) {
		t.Fatalf("save score settings: %d %s", r.status, r.body)
	}
	// A lower threshold makes more items recoverable (scores depend on today's date).
	r = e.do(e.c, "GET", "/cleanup", nil, nil)
	if !strings.Contains(r.body, "score ≥ 0,2") || strings.Count(r.body, `class="above"`) < 3 {
		t.Errorf("threshold / weights not applied:\n%s", r.body)
	}
}
