package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"hangarr/internal/i18n"
	"hangarr/internal/store"
)

// duplicateRow is the table row of the version whose body contains name.
func duplicateRow(t *testing.T, body, name string) string {
	t.Helper()
	for _, row := range strings.Split(body, "<tr>") {
		if strings.Contains(row, name) {
			return row
		}
	}
	t.Fatalf("no row for %q in:\n%s", name, body)
	return ""
}

// Dune has two versions on Plex (1021 Remux, Radarr's file; 1022 WEB-DL),
// group tmdb:438631 (testdata/README.md).
func TestDuplicatesPage(t *testing.T) {
	e := newLibEnv(t, 0)
	id := e.mediaID("movie", "Dune")
	r := e.do(e.c, "GET", "/duplicates", nil, nil)
	if r.status != http.StatusOK {
		t.Fatalf("status %d", r.status)
	}
	for _, want := range []string{"Dune", "Remux", "WEB-DL", "4K", "1080p", `href="/movies/` + id + `"`,
		"/duplicates/tmdb:438631/versions/1022/delete", `hx-trigger="revealed"`, "À revoir (1)"} {
		if !strings.Contains(r.body, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestDuplicatesLive(t *testing.T) {
	e := newLibEnv(t, 0)
	r := e.do(e.c, "GET", "/duplicates/tmdb:438631/live", nil, map[string]string{"HX-Request": "true"})
	if r.status != http.StatusOK {
		t.Fatalf("status %d\n%s", r.status, r.body)
	}
	remux := duplicateRow(t, r.body, "Remux")
	for _, want := range []string{"Français (AC3 5.1)", "Français Forced", "Radarr", "surveillé"} {
		if !strings.Contains(remux, want) {
			t.Errorf("Remux row misses %q:\n%s", want, remux)
		}
	}
	if strings.Contains(remux, "non surveillé") {
		t.Errorf("Remux row says not monitored:\n%s", remux)
	}
	if web := duplicateRow(t, r.body, "WEB-DL"); strings.Contains(web, "Radarr") {
		t.Errorf("WEB-DL row has the Radarr badge:\n%s", web)
	}
	if strings.Contains(r.body, "hx-trigger") {
		t.Error("the live fragment asks for itself again")
	}
}

// TestDuplicatesLiveSingleSnapshot: the tracks come from the snapshot the
// handler holds, not from the registry. The configuration changes after the
// snapshot is taken (Plex removed) and the tracks are still read from Plex.
// This does not reproduce the double read of duplicatesLive, which needs a
// concurrent change: it guards readLive against reading the registry itself.
func TestDuplicatesLiveSingleSnapshot(t *testing.T) {
	e := newLibEnv(t, 0)
	ctx := context.Background()
	up := e.srv.Upstreams.Get()
	if up.Plex == nil {
		t.Fatal("the test needs Plex configured")
	}
	g, err := e.srv.Store.DuplicateGroup(ctx, store.ServerPlex, "tmdb:438631", e.srv.Config.ProtectedTags)
	if err != nil {
		t.Fatal(err)
	}
	useJellyfin(t, e)
	if e.srv.Upstreams.Get().Plex != nil {
		t.Fatal("Plex still configured after the change")
	}
	tracks, _, ok := e.srv.readLive(ctx, i18n.For(i18n.FR), up, store.ServerPlex, g)
	if !ok {
		t.Fatal("tracks unavailable: the snapshot's Plex was not used")
	}
	remux := tracks["1021"]
	if !slices.Contains(remux.audio, "Français (AC3 5.1)") || !slices.Contains(remux.subtitles, "Français Forced") {
		t.Errorf("Remux tracks: %+v", remux)
	}
}

func TestDuplicatesAck(t *testing.T) {
	e := newLibEnv(t, 0)
	fp := store.Fingerprint([]string{"1021", "1022"})
	const ack, unack = "/duplicates/tmdb:438631/ack", "/duplicates/tmdb:438631/unack"
	page := e.do(e.c, "GET", "/duplicates", nil, nil)
	if !strings.Contains(page.body, ack) {
		t.Fatalf("no ack form:\n%s", page.body)
	}
	tok := csrfFrom(t, page.body)
	if r := e.do(e.c, "POST", ack, url.Values{"fingerprint": {fp}, "tab": {"review"}}, nil); r.status != http.StatusForbidden {
		t.Fatalf("ack without csrf: %d", r.status)
	}
	r := e.do(e.c, "POST", ack, url.Values{"csrf_token": {tok}, "fingerprint": {fp}, "tab": {"review"}}, nil)
	if r.status != http.StatusSeeOther || r.header.Get("Location") != "/duplicates" {
		t.Fatalf("ack: %d %s", r.status, r.header.Get("Location"))
	}
	if body := e.do(e.c, "GET", "/duplicates", nil, nil).body; strings.Contains(body, ack) {
		t.Error("acknowledged duplicate still to review")
	}
	acked := e.do(e.c, "GET", "/duplicates?tab=acked", nil, nil).body
	if !strings.Contains(acked, unack) || !strings.Contains(acked, "Dune") {
		t.Fatalf("acknowledged tab:\n%s", acked)
	}
	r = e.do(e.c, "POST", unack, url.Values{"csrf_token": {tok}, "fingerprint": {fp}, "tab": {"acked"}}, nil)
	if r.status != http.StatusSeeOther || r.header.Get("Location") != "/duplicates?tab=acked" {
		t.Fatalf("unack: %d %s", r.status, r.header.Get("Location"))
	}
	if body := e.do(e.c, "GET", "/duplicates", nil, nil).body; !strings.Contains(body, ack) {
		t.Error("unacknowledged duplicate not back to review")
	}
	r = e.do(e.c, "POST", ack, url.Values{"csrf_token": {tok}, "fingerprint": {"1021"}, "tab": {"review"}}, nil)
	if r.status != http.StatusConflict || !strings.Contains(r.body, "Les versions ont changé") {
		t.Errorf("stale fingerprint: %d\n%s", r.status, r.body)
	}
}

func TestDuplicateVersionDelete(t *testing.T) {
	e := newLibEnv(t, 0) // dry run
	const form = "/duplicates/tmdb:438631/versions/%s/delete"
	fp := store.Fingerprint([]string{"1021", "1022"})
	hx := map[string]string{"HX-Request": "true"}
	r := e.do(e.c, "GET", fmt.Sprintf(form, "1022"), nil, hx)
	if r.status != http.StatusOK || !strings.Contains(r.body, "Simulation (DRY_RUN)") || strings.Contains(r.body, "<html") ||
		!strings.Contains(r.body, `name="fingerprint"`) {
		t.Fatalf("form: %d\n%s", r.status, r.body)
	}
	tok := csrfFrom(t, r.body)
	if r := e.do(e.c, "POST", fmt.Sprintf(form, "1022"), url.Values{"fingerprint": {fp}}, hx); r.status != http.StatusForbidden {
		t.Fatalf("delete without csrf: %d", r.status)
	}
	r = e.do(e.c, "POST", fmt.Sprintf(form, "1022"), url.Values{"csrf_token": {tok}, "fingerprint": {fp}}, hx)
	if r.status != http.StatusOK || !strings.Contains(r.body, "Simulation effectuée") {
		t.Fatalf("dry run delete: %d\n%s", r.status, r.body)
	}
	if n := len(e.mock.Calls()); n != 0 {
		t.Errorf("dry run called upstream: %+v", e.mock.Calls())
	}
	// 1021 is Radarr's file and Radarr monitors Dune: refused.
	r = e.do(e.c, "POST", fmt.Sprintf(form, "1021"), url.Values{"csrf_token": {tok}, "fingerprint": {fp}}, hx)
	if !strings.Contains(r.body, "Radarr surveille le film") {
		t.Errorf("Radarr file not refused:\n%s", r.body)
	}
	if n := len(e.mock.Calls()); n != 0 {
		t.Errorf("refused deletion called upstream: %+v", e.mock.Calls())
	}
	if j := e.do(e.c, "GET", "/journal", nil, nil); !strings.Contains(j.body, "Version supprimée") {
		t.Error("journal misses the version deletion")
	}
}

func TestDuplicatesNoServer(t *testing.T) {
	h := newHarness(t, nil)
	c := h.loggedIn()
	r := h.do(c, "GET", "/duplicates", nil, nil)
	if r.status != http.StatusOK || !strings.Contains(r.body, "Aucun serveur multimédia configuré.") ||
		strings.Contains(r.body, "dup-card") {
		t.Fatalf("no server: %d\n%s", r.status, r.body)
	}
}
