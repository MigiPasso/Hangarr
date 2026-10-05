package web

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"cleanarr/internal/clients/sonarr"
	"cleanarr/internal/store"
)

func (e *libEnv) mediaID(kind, title string) string {
	e.t.Helper()
	var id int64
	if err := e.srv.Store.DB().QueryRow(`SELECT id FROM media WHERE kind = ? AND title = ?`, kind, title).Scan(&id); err != nil {
		e.t.Fatalf("%s: %v", title, err)
	}
	return strconv.FormatInt(id, 10)
}

func TestDetailPage(t *testing.T) {
	e := newLibEnv(t, 0)
	bb := e.mediaID("series", "Breaking Bad")
	r := e.do(e.c, "GET", "/series/"+bb, nil, nil)
	if r.status != http.StatusOK {
		t.Fatalf("status %d", r.status)
	}
	for _, want := range []string{"<html", "Spéciaux", "Saison 2", "Progression par spectateur", "Historique de lecture",
		"/series/breaking-bad", "app.plex.tv/desktop#!/server/mock/details?key=%2Flibrary%2Fmetadata%2F201",
		"imdb.com/title/tt0903747", "S02E03", `hx-get="/series/` + bb + `/delete?season=1"`, "Direct"} {
		if !strings.Contains(r.body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(r.body, "Sonarr injoignable") {
		t.Error("episodes should come from Sonarr")
	}
	// Modal partial.
	r = e.do(e.c, "GET", "/series/"+bb, nil, map[string]string{"HX-Request": "true", "HX-Target": "modal-body"})
	if strings.Contains(r.body, "<html") || !strings.Contains(r.body, "Ouvrir la page") {
		t.Error("modal partial expected")
	}
	if r := e.do(e.c, "GET", "/movies/"+bb, nil, nil); r.status != http.StatusNotFound {
		t.Errorf("kind mismatch: %d", r.status)
	}
	if r := e.do(e.c, "GET", "/series/abc", nil, nil); r.status != http.StatusNotFound {
		t.Errorf("bad id: %d", r.status)
	}
	sev := e.do(e.c, "GET", "/series/"+e.mediaID("series", "Severance"), nil, nil)
	if !strings.Contains(sev.body, `disabled title="Protégé par un tag"`) {
		t.Error("protected series: delete buttons must be disabled")
	}
	vac := e.do(e.c, "GET", "/movies/"+e.mediaID("movie", "Vacances 2019"), nil, nil)
	if strings.Contains(vac.body, "/delete") {
		t.Error("plex_only item must not offer deletion")
	}
}

func TestBuildProgress(t *testing.T) {
	ep := func(s, e int, file bool) sonarr.Episode {
		return sonarr.Episode{SeasonNumber: s, EpisodeNumber: e, HasFile: file}
	}
	eps := []sonarr.Episode{ep(0, 1, true), ep(1, 1, false), ep(1, 2, true), ep(1, 3, true), ep(2, 1, true), ep(2, 2, true)}
	watches := []store.EpisodeWatch{
		{UserID: 1, UserName: "Alice", Season: 1, Episode: 1, Watched: true, Plays: 1, LastAt: 10}, // file deleted since
		{UserID: 1, UserName: "Alice", Season: 1, Episode: 2, Watched: true, Plays: 1, LastAt: 20},
		{UserID: 1, UserName: "Alice", Season: 1, Episode: 3, Watched: false, Plays: 1, LastAt: 30},
		{UserID: 2, UserName: "Bob", Season: 2, Episode: 2, Watched: true, Plays: 2, LastAt: 5},
	}
	seasons := []store.Season{{Number: 0}, {Number: 1, Monitored: true}, {Number: 2}}
	rows, users := buildProgress(eps, watches, seasons)
	if rows[1].Files != 2 || rows[1].Total != 3 || rows[1].Watched != 2 || !rows[1].Deletable {
		t.Errorf("season 1: %+v", rows[1])
	}
	if rows[0].Files != 1 || !rows[0].Deletable {
		t.Errorf("specials: %+v", rows[0])
	}
	if len(users) != 2 || users[0].Name != "Alice" {
		t.Fatalf("users: %+v", users)
	}
	alice := users[0]
	// S01E01 has no file anymore: 1 watched among the 2 on disk.
	if alice.Seasons[0].Watched != 1 || alice.Seasons[0].Files != 2 || alice.Last != "S01E02" || alice.Next != "S01E03" || alice.LastAt != 30 {
		t.Errorf("alice: %+v", alice)
	}
	bob := users[1]
	// Bob's last complete play is S02E02, nothing after it on disk: up to date.
	if bob.Next != "" || !bob.UpToDate || bob.Plays != 2 {
		t.Errorf("bob: %+v", bob)
	}
	// Without Sonarr episodes: stored counts, no next episode.
	_, users = buildProgress(nil, watches, []store.Season{{Number: 1, EpisodeFileCount: 1}})
	if users[0].Seasons[0].Watched != 1 || users[0].Next != "" || users[0].UpToDate {
		t.Errorf("approximate: %+v", users[0])
	}
}

func TestDeleteFlowDryRun(t *testing.T) {
	e := newLibEnv(t, 0)
	id := e.mediaID("series", "The Expanse")
	r := e.do(e.c, "GET", "/series/"+id+"/delete?season=2", nil, map[string]string{"HX-Request": "true"})
	if !strings.Contains(r.body, "Mode simulation") || strings.Contains(r.body, "import_exclusion") ||
		!strings.Contains(r.body, "4,5 Go") || strings.Contains(r.body, "<html") {
		t.Fatalf("season form:\n%s", r.body)
	}
	tok := csrfFrom(t, r.body)
	form := url.Values{"season": {"2"}, "delete_files": {"1"}}
	if r := e.do(e.c, "POST", "/series/"+id+"/delete", form, map[string]string{"HX-Request": "true"}); r.status != http.StatusForbidden {
		t.Fatalf("without csrf: %d", r.status)
	}
	r = e.do(e.c, "POST", "/series/"+id+"/delete", form, map[string]string{"HX-Request": "true", csrfHeader: tok})
	trigger := r.header.Get("HX-Trigger")
	if r.status != http.StatusOK || r.header.Get("HX-Reswap") != "none" ||
		!strings.Contains(trigger, `"cleanarr:deleted"`) || !strings.Contains(trigger, `"dryRun":true`) {
		t.Fatalf("dry run: %d %s", r.status, trigger)
	}
	for _, c := range trigger {
		if c > 127 {
			t.Fatalf("HX-Trigger must be ASCII: %s", trigger)
		}
	}
	if len(e.mock.Calls()) != 0 {
		t.Errorf("dry run called upstream: %+v", e.mock.Calls())
	}
	j := e.do(e.c, "GET", "/journal", nil, nil)
	if !strings.Contains(j.body, "Saison supprimée") || !strings.Contains(j.body, "Simulation") {
		t.Errorf("journal:\n%s", j.body)
	}
	if s := e.do(e.c, "GET", "/journal?tab=security", nil, nil); !strings.Contains(s.body, "Connexion") || !strings.Contains(s.body, "Mot de passe changé") {
		t.Error("security journal")
	}
}

func TestDeleteFlowReal(t *testing.T) {
	e := newLibEnvDry(t, 0, false)
	id := e.mediaID("series", "Breaking Bad")
	r := e.do(e.c, "GET", "/series/"+id+"/delete", nil, nil)
	if r.status != http.StatusOK || !strings.Contains(r.body, `name="confirm_title"`) || !strings.Contains(r.body, "Supprimer définitivement") {
		t.Fatalf("series form: %d", r.status)
	}
	tok := csrfFrom(t, r.body)
	h := map[string]string{"HX-Request": "true", csrfHeader: tok}
	r = e.do(e.c, "POST", "/series/"+id+"/delete", url.Values{"delete_files": {"1"}, "confirm_title": {"nope"}}, h)
	if !strings.Contains(r.body, "Le titre saisi ne correspond pas.") || r.header.Get("HX-Trigger") != "" {
		t.Fatalf("wrong title: %s", r.body)
	}
	r = e.do(e.c, "POST", "/series/"+id+"/delete", url.Values{"delete_files": {"1"}, "confirm_title": {"Breaking Bad"}}, h)
	if !strings.Contains(r.header.Get("HX-Trigger"), `"dryRun":false`) {
		t.Fatalf("delete: %d %s %s", r.status, r.header.Get("HX-Trigger"), r.body)
	}
	if r := e.do(e.c, "GET", "/series/"+id, nil, nil); r.status != http.StatusNotFound {
		t.Errorf("deleted series still shown: %d", r.status)
	}
	sev := e.mediaID("series", "Severance")
	r = e.do(e.c, "POST", "/series/"+sev+"/delete", url.Values{"season": {"1"}, "delete_files": {"1"}}, h)
	if !strings.Contains(r.body, "Élément protégé par un tag.") {
		t.Errorf("protected: %s", r.body)
	}
	// Without HTMX: classic redirect.
	tenet := e.mediaID("movie", "Tenet")
	r = e.do(e.c, "POST", "/movies/"+tenet+"/delete", url.Values{"csrf_token": {tok}, "delete_files": {"1"}}, nil)
	if r.status != http.StatusSeeOther || r.header.Get("Location") != "/movies" {
		t.Errorf("non-htmx delete: %d %s", r.status, r.header.Get("Location"))
	}
	_ = context.Background()
}

func TestASCIIJSON(t *testing.T) {
	got := asciiJSON(map[string]string{"m": "Saison supprimée 🎬"})
	if want := "{\"m\":\"Saison supprim\\u00e9e \\ud83c\\udfac\"}"; got != want {
		t.Errorf("got %q", got)
	}
}

func TestProtectButton(t *testing.T) {
	e := newLibEnvDry(t, 0, false)
	bb := e.mediaID("series", "Breaking Bad")
	page := e.do(e.c, "GET", "/series/"+bb, nil, nil)
	if !strings.Contains(page.body, `action="/series/`+bb+`/protect"`) || !strings.Contains(page.body, ">Protéger</button>") {
		t.Fatal("missing « Protéger » button")
	}
	sev := e.do(e.c, "GET", "/series/"+e.mediaID("series", "Severance"), nil, nil)
	if !strings.Contains(sev.body, ">Ne plus protéger</button>") {
		t.Error("protected series must offer « Ne plus protéger »")
	}
	if vac := e.do(e.c, "GET", "/movies/"+e.mediaID("movie", "Vacances 2019"), nil, nil); strings.Contains(vac.body, "/protect") {
		t.Error("plex_only item cannot be protected")
	}
	tok := csrfFrom(t, page.body)
	if r := e.do(e.c, "POST", "/series/"+bb+"/protect", url.Values{"protect": {"1"}}, nil); r.status != http.StatusForbidden {
		t.Errorf("without csrf: %d", r.status)
	}
	r := e.do(e.c, "POST", "/series/"+bb+"/protect", url.Values{"csrf_token": {tok}, "protect": {"1"}},
		map[string]string{"HX-Request": "true"})
	trigger := r.header.Get("HX-Trigger")
	if r.status != http.StatusOK || !strings.Contains(trigger, "cleanarr:changed") || !strings.Contains(trigger, `"success"`) {
		t.Fatalf("protect: %d %s", r.status, trigger)
	}
	page = e.do(e.c, "GET", "/series/"+bb, nil, nil)
	if !strings.Contains(page.body, ">Ne plus protéger</button>") || !strings.Contains(page.body, `disabled title="Protégé par un tag"`) {
		t.Error("detail must show the protected state after protecting")
	}
	if j := e.do(e.c, "GET", "/journal", nil, nil); !strings.Contains(j.body, "<td>Protégé</td>") {
		t.Error("protection not in the actions journal")
	}
	// Without JavaScript: redirect to the detail page.
	r = e.do(e.c, "POST", "/series/"+bb+"/protect", url.Values{"csrf_token": {tok}, "protect": {"0"}}, nil)
	if r.status != http.StatusSeeOther || r.header.Get("Location") != "/series/"+bb {
		t.Errorf("unprotect without htmx: %d %s", r.status, r.header.Get("Location"))
	}
}

func TestProtectDryRun(t *testing.T) {
	e := newLibEnv(t, 0)
	bb := e.mediaID("series", "Breaking Bad")
	tok := csrfFrom(t, e.do(e.c, "GET", "/series/"+bb, nil, nil).body)
	r := e.do(e.c, "POST", "/series/"+bb+"/protect", url.Values{"csrf_token": {tok}, "protect": {"1"}},
		map[string]string{"HX-Request": "true"})
	trigger := r.header.Get("HX-Trigger")
	if !strings.Contains(trigger, "Simulation") || strings.Contains(trigger, "cleanarr:changed") {
		t.Errorf("dry-run: %s", trigger)
	}
}

func TestBatchDelete(t *testing.T) {
	e := newLibEnvDry(t, 0, false)
	page := e.do(e.c, "GET", "/cleanup", nil, nil)
	tenet, sev, inter := e.mediaID("movie", "Tenet"), e.mediaID("series", "Severance"), e.mediaID("movie", "Interstellar")
	if !strings.Contains(page.body, `id="batch-form"`) || !strings.Contains(page.body, `name="ids" value="`+tenet+`" form="batch-form" data-size="15000000000"`) {
		t.Fatal("cleanup page must offer the batch selection")
	}
	lib := e.do(e.c, "GET", "/series", nil, nil)
	if strings.Contains(lib.body, `value="`+sev+`" form="batch-form"`) {
		t.Error("protected items cannot be selected")
	}
	tok := csrfFrom(t, page.body)
	hx := map[string]string{"HX-Request": "true"}
	bb := e.mediaID("series", "Breaking Bad")
	r := e.do(e.c, "POST", "/batch/delete", url.Values{"csrf_token": {tok}, "ids": {tenet, sev, inter, bb}}, hx)
	for _, want := range []string{"3 éléments seront retirés", "Severance", "protégé par un tag", `name="confirm_count"`,
		"<strong>3</strong>", `name="step" value="run"`} {
		if !strings.Contains(r.body, want) {
			t.Errorf("confirmation: missing %q", want)
		}
	}
	run := url.Values{"csrf_token": {tok}, "step": {"run"}, "ids": {tenet, inter, bb}, "delete_files": {"1"}, "confirm_count": {"2"}}
	if r := e.do(e.c, "POST", "/batch/delete", run, hx); !strings.Contains(r.body, "ne correspond pas") {
		t.Error("a wrong count must be refused")
	}
	run.Set("confirm_count", "3")
	r = e.do(e.c, "POST", "/batch/delete", run, hx)
	if !strings.Contains(r.body, "3 réussites") || !strings.Contains(r.header.Get("HX-Trigger"), `"deleted":[`) {
		t.Fatalf("report: %s\n%s", r.header.Get("HX-Trigger"), r.body)
	}
	if after := e.do(e.c, "GET", "/cleanup", nil, nil); strings.Contains(after.body, ">Tenet<") {
		t.Error("deleted items must disappear")
	}
	if r := e.do(e.c, "POST", "/batch/delete", url.Values{"csrf_token": {tok}}, hx); !strings.Contains(r.body, "Aucun élément sélectionné") {
		t.Error("empty selection")
	}
	if r := e.do(e.c, "POST", "/batch/delete", url.Values{"csrf_token": {tok}, "ids": {"x"}}, hx); !strings.Contains(r.body, "Sélection invalide") {
		t.Error("invalid ids")
	}
}

func TestBatchProtect(t *testing.T) {
	e := newLibEnv(t, 0)
	tok := csrfFrom(t, e.do(e.c, "GET", "/movies", nil, nil).body)
	r := e.do(e.c, "POST", "/batch/protect", url.Values{"csrf_token": {tok},
		"ids": {e.mediaID("movie", "Dune"), e.mediaID("movie", "Inception")}}, map[string]string{"HX-Request": "true"})
	if !strings.Contains(r.body, "Protection de la sélection (simulation)") || !strings.Contains(r.body, "2 réussites") ||
		!strings.Contains(r.header.Get("HX-Trigger"), "Simulation") {
		t.Errorf("protect report: %s\n%s", r.header.Get("HX-Trigger"), r.body)
	}
}
