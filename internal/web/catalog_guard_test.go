package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"hangarr/internal/store"
)

// BUG-02: a Radarr catalog refused by the last sync shows on Health with an
// "accept" button, and blocks deletions through the media server.
func TestRefusedCatalog(t *testing.T) {
	e := newLibEnvDry(t, 0, false)
	ctx := context.Background()
	id, err := e.srv.Store.StartSyncRun(ctx, "radarr", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := e.srv.Store.FinishSyncRun(ctx, id, time.Now(), 0,
		&store.CatalogShrinkError{Kind: "movie", Before: 5, After: 0, Removed: 5}); err != nil {
		t.Fatal(err)
	}
	h := e.do(e.c, "GET", "/health", nil, nil)
	if !strings.Contains(h.body, "Catalogue Radarr refusé") || !strings.Contains(h.body, `action="/health/catalog/radarr/accept"`) {
		t.Fatalf("health:\n%s", h.body)
	}
	tok := csrfFrom(t, h.body)

	vac := e.mediaID("movie", "Vacances 2019")
	r := e.do(e.c, "POST", "/movies/"+vac+"/delete", url.Values{"delete_files": {"1"}},
		map[string]string{"HX-Request": "true", csrfHeader: tok})
	if !strings.Contains(r.body, "rien n&#39;est supprimé via le serveur média") || r.header.Get("HX-Trigger") != "" {
		t.Fatalf("server deletion must be blocked: %d %s", r.status, r.body)
	}
	for _, c := range e.mock.Calls() {
		if c.Method == http.MethodDelete {
			t.Errorf("destructive call: %+v", c)
		}
	}

	if r := e.do(e.c, "POST", "/health/catalog/radarr/accept", url.Values{}, nil); r.status != http.StatusForbidden {
		t.Errorf("accept without csrf: %d", r.status)
	}
	if r := e.do(e.c, "POST", "/health/catalog/plex/accept", url.Values{"csrf_token": {tok}}, nil); r.status != http.StatusNotFound {
		t.Errorf("unknown source: %d", r.status)
	}
	r = e.do(e.c, "POST", "/health/catalog/radarr/accept", url.Values{"csrf_token": {tok}}, nil)
	if r.status != http.StatusSeeOther || r.header.Get("Location") != "/health" {
		t.Fatalf("accept: %d %s", r.status, r.header.Get("Location"))
	}
	// The accept starts a sync: wait for it, the Radarr catalog is healthy again.
	deadline := time.Now().Add(5 * time.Second)
	for e.srv.Sync.Status().Running || e.srv.Sync.Status().Last == nil {
		if time.Now().After(deadline) {
			t.Fatal("sync did not finish")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if ok, _ := e.srv.Store.ArrCatalogHealthy(ctx, "movie"); !ok {
		t.Error("catalog still unhealthy after the sync")
	}
}
