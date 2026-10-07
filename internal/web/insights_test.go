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
	for _, global := range []string{"Classement des spectateurs", "Coût par lecture", "Qui regarde quoi"} {
		if strings.Contains(r.body, global) {
			t.Errorf("%q is about every viewer: hidden for one", global)
		}
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

func TestCleanupPageAndSettings(t *testing.T) {
	e := newLibEnv(t, 0)
	r := e.do(e.c, "GET", "/cleanup", nil, nil)
	if r.status != http.StatusOK || !strings.Contains(r.body, "Saisons jamais vues") || !strings.Contains(r.body, "Saison 2") ||
		strings.Contains(r.body, ">Severance<") {
		t.Fatalf("cleanup page: %d", r.status)
	}
	// Default rule: nobody watched for 180 days. Largest first, with the reason.
	if i, j := strings.Index(r.body, ">Interstellar<"), strings.Index(r.body, ">Tenet<"); i < 0 || j < 0 || i > j {
		t.Error("candidates must be sorted by size")
	}
	for _, want := range []string{"depuis 180 jours", "Dernière lecture il y a", "Jamais vu, ajouté il y a"} {
		if !strings.Contains(r.body, want) {
			t.Errorf("missing %q", want)
		}
	}
	candidates := func(body string) string {
		before, _, _ := strings.Cut(body, "<h2>Saisons jamais vues")
		return before
	}
	if strings.Contains(candidates(r.body), ">The Expanse<") {
		t.Error("The Expanse was watched less than 180 days ago")
	}
	if r := e.do(e.c, "GET", "/cleanup?kind=series", nil, nil); strings.Contains(r.body, ">Tenet<") {
		t.Error("kind filter")
	}

	page := e.do(e.c, "GET", "/settings", nil, nil)
	if !strings.Contains(page.body, `name="days" type="number" min="7" max="3650" step="1" required value="180"`) {
		t.Error("cleanup setting not shown")
	}
	tok := csrfFrom(t, page.body)
	for _, bad := range []string{"x", "3", "99999"} {
		if r := e.do(e.c, "POST", "/settings/cleanup", url.Values{"csrf_token": {tok}, "days": {bad}}, nil); r.status != http.StatusUnprocessableEntity {
			t.Errorf("days=%s: %d", bad, r.status)
		}
	}
	r = e.do(e.c, "POST", "/settings/cleanup", url.Values{"csrf_token": {tok}, "days": {"120"}}, map[string]string{"HX-Request": "true"})
	if r.status != http.StatusOK || !strings.Contains(r.body, "Enregistré.") || !strings.Contains(r.body, `value="120"`) {
		t.Fatalf("save: %d %s", r.status, r.body)
	}
	// A shorter delay brings The Expanse (left 140 days ago) in, immediately.
	r = e.do(e.c, "GET", "/cleanup", nil, nil)
	if !strings.Contains(candidates(r.body), ">The Expanse<") || !strings.Contains(r.body, "depuis 120 jours") {
		t.Error("new rule not applied")
	}
	if home := e.do(e.c, "GET", "/", nil, nil); !strings.Contains(home.body, "non regardés depuis 120 jours") {
		t.Error("dashboard tile must follow the rule")
	}
}

func TestHealthPage(t *testing.T) {
	e := newLibEnv(t, 0)
	r := e.do(e.c, "GET", "/health", nil, nil)
	if r.status != http.StatusOK {
		t.Fatalf("status %d", r.status)
	}
	for _, want := range []string{`hx-get="/health/services" hx-trigger="load"`, "Tautulli (historique)",
		"Dans Plex mais pas dans Sonarr/Radarr", ">Home Videos<", "Rapprochements approximatifs", ">Shōgun<",
		`href="/health" aria-current="page"`} {
		if !strings.Contains(r.body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if !strings.Contains(r.body, `hx-get="/health/trackers" hx-trigger="load"`) {
		t.Error("trackers must load separately")
	}
	r = e.do(e.c, "GET", "/health/services", nil, map[string]string{"HX-Request": "true"})
	if strings.Contains(r.body, "<html") || strings.Count(r.body, "badge-ok") != 4 ||
		!strings.Contains(r.body, "Version 4.0.0.0-mock") || strings.Contains(r.body, "Indexers unavailable due to failures") {
		t.Errorf("services fragment (indexer alerts go with the trackers):\n%s", r.body)
	}
	// Trackers: failing first, disabled ones flagged, indexer alerts here.
	r = e.do(e.c, "GET", "/health/trackers", nil, map[string]string{"HX-Request": "true"})
	for _, want := range []string{`id="health-trackers"`, "3 indexeurs", "En échec",
		"Désactivé", "Indexers unavailable due to failures", "<td>Nyaa</td>", "Torrent", "Usenet"} {
		if !strings.Contains(r.body, want) {
			t.Errorf("trackers: missing %q", want)
		}
	}
	if strings.Contains(r.body, "<html") || strings.Index(r.body, "NZBgeek") > strings.Index(r.body, "DrunkenSlug") {
		t.Errorf("trackers fragment, failing first:\n%s", r.body)
	}
	// Without JavaScript: the whole page with the services checked.
	r = e.do(e.c, "GET", "/health/services", nil, nil)
	if !strings.Contains(r.body, "<html") || strings.Contains(r.body, `hx-trigger="load"`) || !strings.Contains(r.body, "badge-ok") ||
		!strings.Contains(r.body, "<td>NZBgeek</td>") {
		t.Error("no-JS page must include the checked services and trackers")
	}
}

func TestCleanupSortableHeaders(t *testing.T) {
	e := newLibEnv(t, 0)
	r := e.do(e.c, "GET", "/cleanup?kind=movie", nil, nil)
	for _, want := range []string{`aria-sort="descending"><a class="sort-link" href="/cleanup?dir=asc&amp;kind=movie&amp;sort=size">Taille`,
		`href="/cleanup?dir=asc&amp;kind=movie&amp;sort=title">Titre`, `href="/cleanup?dir=desc&amp;kind=movie&amp;sort=plays">Lectures`} {
		if !strings.Contains(r.body, want) {
			t.Errorf("missing %q", want)
		}
	}
	r = e.do(e.c, "GET", "/cleanup?sort=title&dir=asc", nil, nil)
	if i, j := strings.Index(r.body, ">Interstellar<"), strings.Index(r.body, ">Tenet<"); i < 0 || j < 0 || i > j {
		t.Error("title ascending: Interstellar before Tenet")
	}
	r = e.do(e.c, "GET", "/cleanup?sort=title&dir=desc", nil, nil)
	if i, j := strings.Index(r.body, ">Interstellar<"), strings.Index(r.body, ">Tenet<"); i < 0 || j < 0 || i < j {
		t.Error("title descending: Tenet before Interstellar")
	}
	if !strings.Contains(r.body, `aria-sort="descending"><a class="sort-link" href="/cleanup?dir=asc&amp;sort=title">`) {
		t.Error("clicking the current column must reverse the direction")
	}
}

func TestCleanupFilters(t *testing.T) {
	e := newLibEnv(t, 0)
	// Tenet was never watched; Interstellar was.
	r := e.do(e.c, "GET", "/cleanup?state=never", nil, nil)
	if !strings.Contains(r.body, ">Tenet<") || strings.Contains(r.body, ">Interstellar<") {
		t.Error("state filter")
	}
	if !strings.Contains(r.body, `name="state" value="never" checked`) || !strings.Contains(r.body, "correspondant aux filtres") {
		t.Error("the form must show the active filter")
	}
	if strings.Contains(r.body, "Saisons jamais vues") {
		t.Error("never-watched seasons have no state: hidden while filtering by state")
	}
	// Sort links keep the filters.
	if !strings.Contains(r.body, `href="/cleanup?dir=asc&amp;sort=title&amp;state=never">Titre`) {
		t.Error("sort links must keep the filters")
	}
	r = e.do(e.c, "GET", "/cleanup?plays_min=1", nil, nil)
	if strings.Contains(r.body, ">Tenet<") || !strings.Contains(r.body, ">Interstellar<") || !strings.Contains(r.body, `name="plays_min" min="0" step="1" value="1"`) {
		t.Error("plays filter")
	}
	r = e.do(e.c, "GET", "/cleanup?plays_max=0", nil, nil)
	if !strings.Contains(r.body, ">Tenet<") || strings.Contains(r.body, ">Interstellar<") {
		t.Error("plays_max=0 keeps never-played items only")
	}
	r = e.do(e.c, "GET", "/cleanup?size_min=99999", nil, nil)
	if !strings.Contains(r.body, "Aucun candidat ne correspond à ces filtres.") {
		t.Error("empty result with filters")
	}
	// Invalid values are ignored.
	if r := e.do(e.c, "GET", "/cleanup?state=bogus&plays_max=-3&size_min=x", nil, nil); r.status != http.StatusOK ||
		!strings.Contains(r.body, ">Tenet<") || !strings.Contains(r.body, ">Interstellar<") {
		t.Error("invalid filters must be ignored")
	}
}

func TestCleanupInactivityChoice(t *testing.T) {
	e := newLibEnv(t, 0)
	// Rule: 180 days. Only longer delays are offered; shorter ones are ignored.
	r := e.do(e.c, "GET", "/cleanup", nil, nil)
	if !strings.Contains(r.body, ">180 jours ou plus</option>") || !strings.Contains(r.body, `<option value="365">1 an</option>`) ||
		strings.Contains(r.body, `<option value="180">`) {
		t.Errorf("choices:\n%s", r.body)
	}
	if r := e.do(e.c, "GET", "/cleanup?inactive=180", nil, nil); !strings.Contains(r.body, ">Interstellar<") || strings.Contains(r.body, "correspondant aux filtres") {
		t.Error("a delay not longer than the rule is ignored")
	}
	// Interstellar was last watched ~200 days ago: gone with 1 year.
	r = e.do(e.c, "GET", "/cleanup?inactive=365", nil, nil)
	if strings.Contains(r.body, ">Interstellar<") || !strings.Contains(r.body, `<option value="365" selected>`) {
		t.Error("1 year")
	}
}
