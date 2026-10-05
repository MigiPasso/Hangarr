package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"cleanarr/internal/actions"
	"cleanarr/internal/config"
	"cleanarr/internal/images"
	"cleanarr/internal/mockarr"
	"cleanarr/internal/syncer"
	"cleanarr/internal/upstream"
)

type libEnv struct {
	*harness
	mock *mockarr.Mock
	c    *http.Client
}

func newLibEnv(t *testing.T, pageSize int) *libEnv { return newLibEnvDry(t, pageSize, true) }

func newLibEnvDry(t *testing.T, pageSize int, dryRun bool) *libEnv {
	t.Helper()
	m := mockarr.New(os.DirFS("../../testdata"), "k")
	m.ShiftToNow(time.Now())
	urls := map[string]string{}
	for name, h := range map[string]http.Handler{
		"sonarr": m.Sonarr(), "radarr": m.Radarr(), "plex": m.Plex(), "tautulli": m.Tautulli(),
	} {
		ts := httptest.NewServer(h)
		t.Cleanup(ts.Close)
		urls[name] = ts.URL
	}
	h := newHarness(t, func(c *config.Config) { c.DryRun = dryRun }, func(d *Deps) {
		log := slog.New(slog.NewTextHandler(io.Discard, nil))
		sy := &syncer.Syncer{
			Store: d.Store, Upstreams: upstream.NewRegistry(upstream.Build(upstream.TestValues(urls, "k"), "auto")),
			Log: log, AbandonDays: 90,
		}
		if _, err := sy.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		lookup := func(ctx context.Context, kind string, id int64, variant string) (images.Info, bool, error) {
			title, remote, ok, err := d.Store.ImageInfo(ctx, kind, id, variant)
			return images.Info{Title: title, RemoteURL: remote}, ok, err
		}
		d.Images = images.New(t.TempDir(), sy.Upstreams.ArrHTTP, lookup, log)
		d.Sync = sy
		d.PageSize = pageSize
		d.Upstreams = sy.Upstreams
		d.Actions = &actions.Service{Store: d.Store, Upstreams: sy.Upstreams,
			Refresher: sy, DryRun: dryRun, ProtectedTags: d.Config.ProtectedTags, ConfirmByTyping: true,
			PlexRefresh: true, Log: log}
	})
	m.Reset()
	return &libEnv{harness: h, mock: m, c: h.loggedIn()}
}

var cardTitle = regexp.MustCompile(`<h3 title="([^"]+)">`)

func cardTitles(body string) []string {
	var out []string
	for _, m := range cardTitle.FindAllStringSubmatch(body, -1) {
		out = append(out, m[1])
	}
	return out
}

func TestLibraryPage(t *testing.T) {
	e := newLibEnv(t, 0)
	r := e.do(e.c, "GET", "/series", nil, nil)
	if r.status != http.StatusOK {
		t.Fatalf("status %d", r.status)
	}
	for _, want := range []string{"<strong>6</strong> séries", "19,4 Go", `name="genre" value="Crime"`,
		`src="/img/series/1/poster"`, `aria-label="Protégé"`, `aria-current="page">Séries`, `id="sync-status"`} {
		if !strings.Contains(r.body, want) {
			t.Errorf("missing %q", want)
		}
	}
	got := cardTitles(r.body)
	want := []string{"Arcane", "Breaking Bad", "The Expanse", "Home Videos", "Severance", "Shōgun"}
	if !slices.Equal(got, want) {
		t.Errorf("cards %q", got)
	}
	if r := e.do(e.c, "GET", "/movies?state=never&sort=size", nil, nil); !slices.Equal(cardTitles(r.body),
		[]string{"Tenet", "Avatar: Fire and Ash", "Vacances 2019"}) {
		t.Errorf("movies never watched by size: %q", cardTitles(r.body))
	}
}

func TestLibraryHTMXPartial(t *testing.T) {
	e := newLibEnv(t, 0)
	r := e.do(e.c, "GET", "/series?q=breaking", nil, map[string]string{"HX-Request": "true", "HX-Target": "results"})
	if r.status != http.StatusOK || strings.Contains(r.body, "<html") || strings.Contains(r.body, `id="library"`) {
		t.Fatalf("expected the results partial only, got %d:\n%s", r.status, r.body)
	}
	if !slices.Equal(cardTitles(r.body), []string{"Breaking Bad"}) || !strings.Contains(r.body, "<strong>1</strong> série") {
		t.Errorf("partial: %q", cardTitles(r.body))
	}
	if !strings.Contains(r.header.Get("Vary"), "HX-Request") {
		t.Error("Vary header missing")
	}
	// A history restore gets the full page even with HTMX headers.
	r = e.do(e.c, "GET", "/series", nil, map[string]string{"HX-Request": "true", "HX-Target": "results",
		"HX-History-Restore-Request": "true"})
	if !strings.Contains(r.body, "<html") {
		t.Error("history restore must render the full page")
	}
}

func TestLibraryInfiniteScroll(t *testing.T) {
	e := newLibEnv(t, 2)
	sentinel := regexp.MustCompile(`class="sentinel" hx-get="([^"]+)"`)
	var all []string
	path := "/series?sort=title"
	for page := 1; page <= 4; page++ {
		headers := map[string]string{}
		if page > 1 {
			headers = map[string]string{"HX-Request": "true"}
		}
		r := e.do(e.c, "GET", path, nil, headers)
		if page > 1 && (strings.Contains(r.body, "summary") || strings.Contains(r.body, "<html")) {
			t.Fatalf("page %d must only contain items:\n%s", page, r.body)
		}
		all = append(all, cardTitles(r.body)...)
		m := sentinel.FindStringSubmatch(r.body)
		if m == nil {
			break
		}
		path = strings.ReplaceAll(m[1], "&amp;", "&")
		if !strings.Contains(path, "page=") {
			t.Fatalf("sentinel without page: %s", path)
		}
	}
	want := []string{"Arcane", "Breaking Bad", "The Expanse", "Home Videos", "Severance", "Shōgun"}
	if !slices.Equal(all, want) {
		t.Errorf("scrolled through %q", all)
	}
}

func TestLibraryViewAndPerspective(t *testing.T) {
	e := newLibEnv(t, 0)
	r := e.do(e.c, "GET", "/movies?view=table", nil, nil)
	if !strings.Contains(r.body, `class="table library-table"`) {
		t.Fatal("table view not rendered")
	}
	// The choice is remembered in a cookie.
	if r := e.do(e.c, "GET", "/movies", nil, nil); !strings.Contains(r.body, `class="table library-table"`) {
		t.Error("view not remembered")
	}
	e.do(e.c, "GET", "/movies?view=grid", nil, nil)
	r = e.do(e.c, "GET", "/series?user=1001&state=completed", nil, nil)
	if !strings.Contains(r.body, "point de vue de <strong>Alice</strong>") ||
		!slices.Equal(cardTitles(r.body), []string{"Breaking Bad"}) {
		t.Errorf("alice perspective: %q", cardTitles(r.body))
	}
}

func TestParseFilterRoundTrip(t *testing.T) {
	q, _ := url.ParseQuery("q=dune&state=never&state=bogus&last=90&genre=Drama&year_min=2000&rating_min=7,5" +
		"&seen_by=1001&seen_by=x&unseen_by=1002&nobody=1&match=arr_only&user=1001&sort=size&size_min=10&page=3&status=ended")
	f, page := parseLibraryFilter(q, "series")
	if page != 3 || f.Query != "dune" || !slices.Equal(f.States, []string{"never"}) || f.LastViewed != "90" ||
		f.RatingMin != 7.5 || !slices.Equal(f.SeenBy, []int64{1001}) || !f.Nobody || f.User != 1001 ||
		f.Sort != "size" || !f.Desc || f.Status != "ended" || f.SizeMinGB != 10 {
		t.Fatalf("parsed: %+v", f)
	}
	again, _ := parseLibraryFilter(filterValues(f), "series")
	if filterValues(again).Encode() != filterValues(f).Encode() {
		t.Errorf("round trip: %s vs %s", filterValues(again).Encode(), filterValues(f).Encode())
	}
	if f, _ := parseLibraryFilter(url.Values{"sort": {"evil"}}, "movie"); f.Sort != "title" || f.Desc {
		t.Errorf("bad sort: %+v", f)
	}
}

func TestImageProxy(t *testing.T) {
	e := newLibEnv(t, 0)
	r := e.do(e.c, "GET", "/img/series/1/poster", nil, nil)
	if r.status != http.StatusOK || r.header.Get("Content-Type") != "image/png" || r.header.Get("ETag") == "" ||
		!strings.Contains(r.header.Get("Cache-Control"), "max-age=604800") {
		t.Fatalf("poster: %d %v", r.status, r.header)
	}
	etag := r.header.Get("ETag")
	if r := e.do(e.c, "GET", "/img/series/1/poster", nil, map[string]string{"If-None-Match": etag}); r.status != http.StatusNotModified {
		t.Errorf("conditional request: %d", r.status)
	}
	if n := e.mock.Hits("sonarr:mediacover"); n != 1 {
		t.Errorf("upstream hits after cache: %d, want 1", n)
	}

	// No image anywhere: SVG placeholder with the title, and no retry storm.
	for range 3 {
		r = e.do(e.c, "GET", "/img/series/5/poster", nil, nil)
	}
	if r.status != http.StatusOK || r.header.Get("Content-Type") != "image/svg+xml" || !strings.Contains(r.body, "Arcane") {
		t.Errorf("placeholder: %d %s", r.status, r.body)
	}
	if n := e.mock.Hits("sonarr:mediacover"); n != 3 {
		t.Errorf("hits after misses: %d, want 3 (poster-500 + poster once, then cached miss)", n)
	}

	for _, p := range []string{"/img/series/999/poster", "/img/books/1/poster", "/img/series/1/banner", "/img/series/x/poster"} {
		if r := e.do(e.c, "GET", p, nil, nil); r.status != http.StatusNotFound {
			t.Errorf("%s: %d", p, r.status)
		}
	}
	if r := e.do(e.client(), "GET", "/img/series/1/poster", nil, nil); r.status != http.StatusSeeOther {
		t.Errorf("anonymous image access: %d", r.status)
	}

	// Concurrent first loads hit the upstream once.
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodGet, e.ts.URL+"/img/movie/1/poster", nil)
			res, err := e.c.Do(req)
			if err == nil {
				_, _ = io.Copy(io.Discard, res.Body)
				_ = res.Body.Close()
			}
		}()
	}
	wg.Wait()
	if n := e.mock.Hits("radarr:mediacover"); n != 1 {
		t.Errorf("concurrent loads: %d upstream hits, want 1", n)
	}
}

func TestSyncEndpoints(t *testing.T) {
	e := newLibEnv(t, 0)
	if r := e.do(e.c, "POST", "/sync", url.Values{}, map[string]string{"HX-Request": "true"}); r.status != http.StatusForbidden {
		t.Fatalf("sync without csrf: %d", r.status)
	}
	tok := csrfFrom(t, e.do(e.c, "GET", "/series", nil, nil).body)
	r := e.do(e.c, "POST", "/sync", url.Values{}, map[string]string{"HX-Request": "true", csrfHeader: tok})
	if r.status != http.StatusOK || !strings.Contains(r.body, `id="sync-status"`) {
		t.Fatalf("sync start: %d %s", r.status, r.body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		r = e.do(e.c, "GET", "/sync/status", nil, map[string]string{"HX-Request": "true"})
		if !strings.Contains(r.body, "every 2s") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("sync did not finish")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(r.body, "Synchro à l") {
		t.Errorf("status after sync: %s", r.body)
	}
}

func TestSettingsSyncTime(t *testing.T) {
	e := newLibEnv(t, 0)
	r := e.do(e.c, "GET", "/settings", nil, nil)
	if r.status != http.StatusOK || !strings.Contains(r.body, `<select id="sync_hour" name="sync_hour" required>`) ||
		!strings.Contains(r.body, `<option value="00" selected>00</option>`) || !strings.Contains(r.body, `<option value="59">59</option>`) ||
		!strings.Contains(r.body, "Prochaine synchronisation") {
		t.Fatalf("settings page: %d", r.status)
	}
	tok := csrfFrom(t, r.body)
	if r := e.do(e.c, "POST", "/settings", url.Values{"sync_hour": {"6"}, "sync_minute": {"15"}}, nil); r.status != http.StatusForbidden {
		t.Errorf("without csrf: %d", r.status)
	}
	for _, bad := range [][2]string{{"25", "00"}, {"6", "60"}, {"", "10"}, {"six", "00"}} {
		r := e.do(e.c, "POST", "/settings", url.Values{"csrf_token": {tok}, "sync_hour": {bad[0]}, "sync_minute": {bad[1]}}, nil)
		if r.status != http.StatusUnprocessableEntity || !strings.Contains(r.body, "Heure invalide") {
			t.Errorf("%v: %d", bad, r.status)
		}
	}
	// One digit is accepted too (forms posted without the page).
	r = e.do(e.c, "POST", "/settings", url.Values{"csrf_token": {tok}, "sync_hour": {"6"}, "sync_minute": {"7"}},
		map[string]string{"HX-Request": "true"})
	if r.status != http.StatusOK || !strings.Contains(r.body, `<option value="06" selected>`) ||
		!strings.Contains(r.body, `<option value="07" selected>`) ||
		!strings.Contains(r.body, "à 06:07") || strings.Contains(r.body, "<html") {
		t.Fatalf("save: %d %s", r.status, r.body)
	}
	if v, _ := e.srv.Store.Setting(context.Background(), "sync_time", ""); v != "06:07" {
		t.Errorf("stored %q", v)
	}
}
