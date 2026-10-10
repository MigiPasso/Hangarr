package images

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"hangarr/internal/clients/httpx"
)

func TestJellyfinUpstreamRequests(t *testing.T) {
	const id = "f0000000000000000000000000000105"
	cases := map[string]struct{ path, query string }{
		"poster": {"/Items/" + id + "/Images/Primary", "maxWidth=500&quality=90"},
		"fanart": {"/Items/" + id + "/Images/Backdrop/0", "maxWidth=1280&quality=90"},
	}
	for variant, want := range cases {
		reqs := upstreamRequests("jellyfin", id, variant)
		if len(reqs) != 1 || reqs[0].Path != want.path || reqs[0].Query.Encode() != want.query {
			t.Errorf("%s: %+v, want %s?%s", variant, reqs, want.path, want.query)
		}
	}
}

func TestServeImageRejectsUnsafeIDs(t *testing.T) {
	dir := t.TempDir()
	lookups := 0
	lookup := func(context.Context, string, string, string) (Info, bool, error) {
		lookups++
		return Info{Title: "X"}, true, nil
	}
	p := New(dir, func(string) *httpx.Client { return nil }, lookup, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, id := range []string{"../x", "a/b", "", "ABC", strings.Repeat("a", 65)} {
		rec := httptest.NewRecorder()
		p.ServeImage(rec, httptest.NewRequest(http.MethodGet, "/img/jellyfin/x/poster", nil), "jellyfin", id, "poster")
		if rec.Code != http.StatusNotFound {
			t.Errorf("id %q: status %d, want 404", id, rec.Code)
		}
	}
	if lookups != 0 {
		t.Fatalf("unsafe ids reached the lookup %d times", lookups)
	}
	// Control: a well-formed id does reach the lookup (and gets the placeholder,
	// as no client or remote URL can supply an image here).
	rec := httptest.NewRecorder()
	p.ServeImage(rec, httptest.NewRequest(http.MethodGet, "/img/jellyfin/f0000000000000000000000000000105/poster", nil),
		"jellyfin", "f0000000000000000000000000000105", "poster")
	if lookups != 1 || rec.Code != http.StatusOK {
		t.Errorf("valid id: %d lookups, status %d", lookups, rec.Code)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("cache directory not empty: %d entries", len(entries))
	}
}
