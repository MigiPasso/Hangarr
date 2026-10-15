package web

import (
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"hangarr/internal/store"
	"hangarr/internal/upstream"
)

// useJellyfin makes Jellyfin the only media server of the environment (Plex is
// removed: one media server at a time): the registry is rebuilt, as the
// Settings › Services page does.
func useJellyfin(t *testing.T, e *libEnv) {
	t.Helper()
	jf := httptest.NewServer(e.mock.Jellyfin())
	t.Cleanup(jf.Close)
	v := maps.Clone(e.srv.Upstreams.Get().Values)
	delete(v, upstream.PlexURL)
	delete(v, upstream.PlexToken)
	v[upstream.JellyfinURL], v[upstream.JellyfinAPIKey] = jf.URL, "k"
	e.srv.Upstreams.Set(upstream.Build(v, "auto"))
}

func TestJellyfinDetailLink(t *testing.T) {
	e := newLibEnv(t, 0)
	useJellyfin(t, e)
	const jid = "f0000000000000000000000000000205"
	bb := e.mediaID("series", "Breaking Bad")
	if _, err := e.srv.Store.DB().Exec(`UPDATE media SET jellyfin_id = ? WHERE id = ?`, jid, bb); err != nil {
		t.Fatal(err)
	}
	if err := e.srv.Store.SetSetting(context.Background(), store.SettingJellyfinServerID, "srv1", time.Now()); err != nil {
		t.Fatal(err)
	}
	r := e.do(e.c, "GET", "/series/"+bb, nil, nil)
	if r.status != http.StatusOK {
		t.Fatalf("status %d", r.status)
	}
	if want := "/web/#/details?id=" + jid + "&amp;serverId=srv1"; !strings.Contains(r.body, want) {
		t.Errorf("missing the Jellyfin link %q", want)
	}
	if !strings.Contains(r.body, ">Jellyfin ↗</a>") {
		t.Error("the Jellyfin link must be labelled Jellyfin")
	}
	if strings.Contains(r.body, "app.plex.tv") || strings.Contains(r.body, "#!/server/") {
		t.Error("no Plex link while Jellyfin is the media server")
	}
}

func TestJellyfinOnlyDeleteFlow(t *testing.T) {
	e := newLibEnv(t, 0) // dry run
	const jid = "f0000000000000000000000000000105"
	vac := e.mediaID("movie", "Vacances 2019")
	if _, err := e.srv.Store.DB().Exec(`UPDATE media SET jellyfin_id = ?, plex_rating_key = NULL WHERE id = ?`, jid, vac); err != nil {
		t.Fatal(err)
	}
	useJellyfin(t, e)
	page := e.do(e.c, "GET", "/movies/"+vac+"/delete", nil, nil)
	if page.status != http.StatusOK || !strings.Contains(page.body, "Absent de Sonarr/Radarr : Jellyfin le supprime") {
		t.Fatalf("form: %d\n%s", page.status, page.body)
	}
	tok := csrfFrom(t, page.body)
	r := e.do(e.c, "POST", "/movies/"+vac+"/delete", url.Values{"csrf_token": {tok}, "delete_files": {"1"}},
		map[string]string{"HX-Request": "true"})
	if r.status != http.StatusOK || !strings.Contains(r.header.Get("HX-Trigger"), `"dryRun":true`) {
		t.Fatalf("dry run: %d %s\n%s", r.status, r.header.Get("HX-Trigger"), r.body)
	}
	var n int
	if err := e.srv.Store.DB().QueryRow(`SELECT COUNT(*) FROM action_log WHERE action = 'delete_jellyfin' AND dry_run = 1`).Scan(&n); err != nil || n != 1 {
		t.Errorf("delete_jellyfin not journaled: %d %v", n, err)
	}
	if calls := e.mock.Calls(); len(calls) != 0 {
		t.Errorf("dry run called upstream: %+v", calls)
	}
}

func TestHiddenItemNoActions(t *testing.T) {
	e := newLibEnv(t, 0)
	useJellyfin(t, e)
	hv := e.mediaID("series", "Home Videos")
	if _, err := e.srv.Store.DB().Exec(`UPDATE media SET match_state = 'hidden' WHERE id = ?`, hv); err != nil {
		t.Fatal(err)
	}
	page := e.do(e.c, "GET", "/series/"+hv, nil, nil)
	if page.status != http.StatusOK || !strings.Contains(page.body, "Plex (non utilisé)") {
		t.Fatalf("detail: %d", page.status)
	}
	if strings.Contains(page.body, `/delete"`) || strings.Contains(page.body, `/protect"`) {
		t.Error("a hidden item has neither a delete nor a protect button")
	}
	if r := e.do(e.c, "GET", "/series/"+hv+"/delete", nil, nil); r.status != http.StatusNotFound {
		t.Errorf("delete page of a hidden item: %d", r.status)
	}
	tok := csrfFrom(t, e.do(e.c, "GET", "/", nil, nil).body)
	r := e.do(e.c, "POST", "/series/"+hv+"/protect", url.Values{"csrf_token": {tok}, "protect": {"1"}}, nil)
	if r.status != http.StatusUnprocessableEntity || !strings.Contains(r.body, "Connu seulement du serveur multimédia qui n'est pas utilisé") {
		t.Errorf("protect a hidden item: %d %s", r.status, r.body)
	}
}
