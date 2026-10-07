package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"hangarr/internal/clients/radarr"
	"hangarr/internal/clients/sonarr"
	"hangarr/internal/i18n"
	"hangarr/internal/upstream"
)

func TestCalendarPage(t *testing.T) {
	e := newLibEnv(t, 0)
	r := e.do(e.c, "GET", "/calendar?size=100", nil, nil)
	if r.status != http.StatusOK {
		t.Fatalf("status %d", r.status)
	}
	// The mock moves the fixtures so that 2026-10-01 is today; default range: 90 days.
	order := []string{"Hello, Ms. Cobel", "We Is Us", "Avatar: Fire and Ash", "S02E01–E03", "Project Hail Mary", "Cold Harbor"}
	last := -1
	for _, want := range order {
		i := strings.Index(r.body, want)
		if i < 0 {
			t.Errorf("missing %q", want)
			continue
		}
		if i < last {
			t.Errorf("%q out of order", want)
		}
		last = i
	}
	for _, want := range []string{`href="/calendar" title="À venir" aria-current="page"`, "Nouvelle série", "Nouvelle saison", "Fin de saison",
		"3 épisodes", "Au cinéma", `href="/series/` + e.mediaID("series", "Severance") + `"`,
		`href="/movies/` + e.mediaID("movie", "Avatar: Fire and Ash") + `"`, `src="/img/series/3/poster"`} {
		if !strings.Contains(r.body, want) {
			t.Errorf("missing %q", want)
		}
	}
	// Past episodes, later releases, and no link for items not synced yet.
	for _, unwanted := range []string{"What's for Dinner?", "The Lexington Letter", ">Pluribus</a>", ">Project Hail Mary</a>"} {
		if strings.Contains(r.body, unwanted) {
			t.Errorf("unexpected %q", unwanted)
		}
	}
	if e.mock.Hits("sonarr:calendar") != 1 || e.mock.Hits("radarr:calendar") != 1 {
		t.Error("calendar must be read live from Sonarr and Radarr")
	}

	// Last page of the year: the latest releases.
	year := e.do(e.c, "GET", "/calendar?days=365&size=100&page=999", nil, nil)
	for _, want := range []string{"The Lexington Letter", "S03E08", "Sortie physique", `days=365&amp;size=100" aria-current="page"`,
		`rel="prev"`, "page 2 sur 2"} {
		if !strings.Contains(year.body, want) {
			t.Errorf("1 year: missing %q", want)
		}
	}
	if strings.Contains(year.body, `rel="next"`) {
		t.Error("last page: no next link")
	}
	if odd := e.do(e.c, "GET", "/calendar?days=7&size=7", nil, nil); !strings.Contains(odd.body, `days=90&amp;size=25" aria-current="page"`) {
		t.Error("unknown range and size: 90 days, 25 per page expected")
	}
}

func TestCalendarPages(t *testing.T) {
	e := newLibEnv(t, 0)
	first := e.do(e.c, "GET", "/calendar?size=10", nil, nil)
	for _, want := range []string{`<option value="10" selected>10</option>`, "page 1 sur", `href="/calendar?days=90&amp;size=10&amp;page=2" rel="next"`,
		"Hello, Ms. Cobel"} {
		if !strings.Contains(first.body, want) {
			t.Errorf("page 1: missing %q", want)
		}
	}
	if strings.Contains(first.body, `rel="prev"`) || strings.Count(first.body, `class="cal-entry"`) != 10 {
		t.Errorf("page 1: 10 entries and no previous link expected")
	}
	second := e.do(e.c, "GET", "/calendar?size=10&page=2", nil, nil)
	if !strings.Contains(second.body, "page 2 sur") || !strings.Contains(second.body, `href="/calendar?days=90&amp;size=10" rel="prev"`) ||
		strings.Contains(second.body, "Hello, Ms. Cobel") {
		t.Error("page 2: next entries and a link back to page 1 expected")
	}
	if strings.Contains(first.body, "Ce mois-ci") {
		t.Error("no This month link")
	}
	if def := e.do(e.c, "GET", "/calendar", nil, nil); !strings.Contains(def.body, `<option value="25" selected>25</option>`) {
		t.Error("25 per page by default")
	}
}

func TestCalendarUnavailable(t *testing.T) {
	e := newLibEnv(t, 0)
	v := upstream.TestValues(map[string]string{"sonarr": "http://127.0.0.1:1"}, "k")
	delete(v, upstream.RadarrURL)
	e.srv.Upstreams.Set(upstream.Build(v, "auto"))
	r := e.do(e.c, "GET", "/calendar", nil, nil)
	if r.status != http.StatusOK {
		t.Fatalf("status %d", r.status)
	}
	for _, want := range []string{"Injoignable, sorties manquantes : Sonarr.", "Non configuré : Radarr."} {
		if !strings.Contains(r.body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(r.body, "Aucune sortie prévue") {
		t.Error("an unreachable service must not look like an empty calendar")
	}
}

// Radarr release dates are days: they must not move to the previous day west
// of UTC.
func TestMovieEntriesKeepTheDay(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	var movies []radarr.CalendarMovie
	if err := json.Unmarshal([]byte(`[{"id":1,"title":"M","inCinemas":"2026-12-17T00:00:00Z",
		"digitalRelease":"2027-03-01T00:00:00Z"}]`), &movies); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 12, 17, 0, 0, 0, 0, ny)
	got := movieEntries(i18n.For(i18n.EN), movies, ny, start, start.AddDate(0, 0, 30))
	if len(got) != 1 || got[0].What != "In cinemas" || got[0].at.Day() != 17 {
		t.Fatalf("entries %+v", got)
	}
}

func TestCalendarMonthView(t *testing.T) {
	e := newLibEnv(t, 0)
	loc := e.srv.Config.Location
	shogun := time.Now().In(loc).AddDate(0, 0, 20) // Shōgun S02E01–E03, see testdata/README.md
	month := shogun.Format("2006-01")
	r := e.do(e.c, "GET", "/calendar?view=month&month="+month, nil, nil)
	if r.status != http.StatusOK {
		t.Fatalf("status %d", r.status)
	}
	for _, want := range []string{`<table class="cal-month">`, "<th scope=\"col\">Lun</th>", "S02E01–E03",
		`href="/calendar?view=month" aria-current="true">Calendrier`,
		`month=` + shogun.AddDate(0, 0, -shogun.Day()+1).AddDate(0, 1, 0).Format("2006-01")} {
		if !strings.Contains(r.body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if odd := e.do(e.c, "GET", "/calendar?view=month&month=nope", nil, nil); !strings.Contains(odd.body, `aria-current="date"`) {
		t.Error("invalid month: the current month (with today) expected")
	}
}

func TestCalendarWeeks(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	first := time.Date(2026, 10, 1, 0, 0, 0, 0, paris) // a Thursday
	start := time.Date(2026, 9, 28, 0, 0, 0, 0, paris)
	end := time.Date(2026, 11, 2, 0, 0, 0, 0, paris)
	at := time.Date(2026, 10, 25, 21, 0, 0, 0, paris) // DST ends that night
	weeks := calendarWeeks([]calendarEntry{{Title: "X", at: at}}, first, start, end, time.Date(2026, 10, 7, 0, 0, 0, 0, paris))
	if len(weeks) != 5 {
		t.Fatalf("%d weeks", len(weeks))
	}
	for _, w := range weeks {
		if len(w) != 7 {
			t.Fatalf("week of %d days", len(w))
		}
	}
	if c := weeks[0][0]; c.Day != 28 || c.InMonth || !c.Past {
		t.Errorf("first cell %+v", c)
	}
	if c := weeks[1][2]; c.Day != 7 || !c.Today {
		t.Errorf("today %+v", c)
	}
	if c := weeks[3][6]; c.Day != 25 || len(c.Entries) != 1 {
		t.Errorf("Sunday 25 %+v", c)
	}
	if c := weeks[4][6]; c.Day != 1 || c.InMonth {
		t.Errorf("last cell %+v", c)
	}
}

func TestCalendarGridView(t *testing.T) {
	e := newLibEnv(t, 0)
	r := e.do(e.c, "GET", "/calendar?view=grid&size=100", nil, nil)
	if r.status != http.StatusOK {
		t.Fatalf("status %d", r.status)
	}
	for _, want := range []string{`<article class="media-card cal-card`, `<div class="grid cal-flow">`, `role="heading" aria-level="2"`,
		`<span class="cal-date-num" aria-hidden="true">`, `href="/calendar?view=grid&amp;days=90&amp;size=100" aria-current="true">Grille`,
		`<span class="badge state">S02E01–E03</span>`, `<div class="poster-fallback">Pluribus</div>`, `src="/img/series/3/poster"`,
		`<input type="hidden" name="view" value="grid">`, `<option value="100" selected>100</option>`,
		`href="/calendar?view=grid&amp;days=365&amp;size=100"`} {
		if !strings.Contains(r.body, want) {
			t.Errorf("missing %q", want)
		}
	}
	page2 := e.do(e.c, "GET", "/calendar?view=grid&size=10", nil, nil)
	if !strings.Contains(page2.body, `href="/calendar?view=grid&amp;days=90&amp;size=10&amp;page=2" rel="next"`) {
		t.Error("grid pages keep the view")
	}
}

func TestNewCalendarDay(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	today := time.Date(2026, 10, 24, 0, 0, 0, 0, paris)
	fr := i18n.For(i18n.FR)
	// The night of the 25th has 25 hours: still "in 3 days" on the 27th.
	d := newCalendarDay(fr, time.Date(2026, 10, 27, 21, 0, 0, 0, paris), today)
	if d.Weekday != "Mar" || d.Num != "27" || d.Rel != "dans 3 jours" || d.Label != "Mardi 27/10/2026" {
		t.Errorf("%+v", d)
	}
	if d := newCalendarDay(fr, today.Add(20*time.Hour), today); d.Rel != "Aujourd'hui" {
		t.Errorf("today: %+v", d)
	}
	if d := newCalendarDay(fr, today.AddDate(0, 0, 1), today); d.Rel != "Demain" || d.Label != "Demain" {
		t.Errorf("tomorrow: %+v", d)
	}
}

// A season released at once keeps both its premiere and its finale, in
// episode order, whatever the order of the API.
func TestSeasonDropNotes(t *testing.T) {
	var eps []sonarr.CalendarEpisode
	if err := json.Unmarshal([]byte(`[
		{"seriesId":7,"seasonNumber":2,"episodeNumber":3,"airDateUtc":"2026-11-02T08:00:00Z","finaleType":"season","series":{"title":"X"}},
		{"seriesId":7,"seasonNumber":2,"episodeNumber":1,"airDateUtc":"2026-11-02T08:00:00Z","series":{"title":"X"}},
		{"seriesId":7,"seasonNumber":2,"episodeNumber":2,"airDateUtc":"2026-11-02T08:00:00Z","series":{"title":"X"}}]`), &eps); err != nil {
		t.Fatal(err)
	}
	got := seriesEntries(i18n.For(i18n.EN), eps, time.UTC)
	if len(got) != 1 || got[0].What != "S02E01–E03" || strings.Join(got[0].Notes, "|") != "Season premiere|Season finale" {
		t.Fatalf("%+v", got)
	}
}

// Period tabs in increasing order; 3 months stays the default.
func TestCalendarRangeTabs(t *testing.T) {
	e := newLibEnv(t, 0)
	r := e.do(e.c, "GET", "/calendar", nil, nil)
	m30, m90, y1 := strings.Index(r.body, ">30 jours</a>"), strings.Index(r.body, ">3 mois</a>"), strings.Index(r.body, ">1 an</a>")
	if m30 < 0 || m90 < m30 || y1 < m90 {
		t.Errorf("tabs order: 30 days %d, 3 months %d, 1 year %d", m30, m90, y1)
	}
	if !strings.Contains(r.body, `days=90&amp;size=25" aria-current="page">3 mois`) {
		t.Error("3 months by default")
	}
}
