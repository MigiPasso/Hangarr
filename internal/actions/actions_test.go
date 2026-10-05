package actions

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"arrstats/internal/clients/sonarr"
	"arrstats/internal/mockarr"
	"arrstats/internal/store"
	"arrstats/internal/syncer"
	"arrstats/internal/upstream"
)

type env struct {
	t    *testing.T
	fsys fstest.MapFS
	mock *mockarr.Mock
	st   *store.Store
	svc  *Service
}

func newEnv(t *testing.T, dryRun bool) *env {
	t.Helper()
	fsys := fstest.MapFS{}
	src := os.DirFS("../../testdata")
	_ = fs.WalkDir(src, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := fs.ReadFile(src, p)
			fsys[p] = &fstest.MapFile{Data: b}
		}
		return err
	})
	m := mockarr.New(fsys, "k")
	urls := map[string]string{}
	for name, h := range map[string]http.Handler{
		"sonarr": m.Sonarr(), "radarr": m.Radarr(), "plex": m.Plex(), "tautulli": m.Tautulli(),
	} {
		ts := httptest.NewServer(h)
		t.Cleanup(ts.Close)
		urls[name] = ts.URL
	}
	st, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sy := &syncer.Syncer{
		Store: st, Upstreams: upstream.NewRegistry(upstream.Build(upstream.TestValues(urls, "k"), "auto")),
		Log: log, AbandonDays: 90,
		Now: func() time.Time { return time.Unix(mockarr.Anchor, 0) },
	}
	if _, err := sy.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.Reset()
	return &env{t: t, fsys: fsys, mock: m, st: st, svc: &Service{
		Store: st, Upstreams: sy.Upstreams, Refresher: sy,
		DryRun: dryRun, ProtectedTags: []string{"keep"}, ConfirmByTyping: true, PlexRefresh: true, Log: log,
	}}
}

func (e *env) mediaID(kind, title string) int64 {
	e.t.Helper()
	var id int64
	if err := e.st.DB().QueryRow(`SELECT id FROM media WHERE kind = ? AND title = ?`, kind, title).Scan(&id); err != nil {
		e.t.Fatalf("%s %s: %v", kind, title, err)
	}
	return id
}

func (e *env) destructive() []mockarr.Call {
	var out []mockarr.Call
	for _, c := range e.mock.Calls() {
		if c.Service == "sonarr" || c.Service == "radarr" {
			out = append(out, c)
		}
	}
	return out
}

func (e *env) lastAction() store.Action {
	e.t.Helper()
	actions, _, err := e.st.Actions(context.Background(), 0, 1)
	if err != nil || len(actions) == 0 {
		e.t.Fatalf("no action journaled: %v", err)
	}
	return actions[0]
}

func intp(n int) *int { return &n }

func TestDeleteSeasonOrder(t *testing.T) {
	e := newEnv(t, false)
	res, err := e.svc.Delete(context.Background(), Request{MediaID: e.mediaID("series", "The Expanse"),
		Season: intp(2), DeleteFiles: true, ImportExclusion: true, Username: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != DeleteSeason || res.SizeFreed != 4_500_000_000 || res.DryRun {
		t.Errorf("result: %+v", res)
	}
	calls := e.destructive()
	if len(calls) != 2 {
		t.Fatalf("calls: %+v", calls)
	}
	// 1. files of season 2 only, 2. then the season is unmonitored.
	del, put := calls[0], calls[1]
	if del.Method != http.MethodDelete || del.Path != "/api/v3/episodefile/bulk" ||
		strings.TrimSpace(del.Body) != `{"episodeFileIds":[20201,20202,20203]}` {
		t.Errorf("first call must delete season 2 files: %+v", del)
	}
	if put.Method != http.MethodPut || put.Path != "/api/v3/series/2" {
		t.Fatalf("second call must PUT the series: %+v", put)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(put.Body), &body); err != nil {
		t.Fatal(err)
	}
	monitored := map[int]bool{}
	for _, s := range body["seasons"].([]any) {
		se := s.(map[string]any)
		monitored[int(se["seasonNumber"].(float64))] = se["monitored"].(bool)
	}
	if monitored[2] || !monitored[1] {
		t.Errorf("seasons monitored: %v", monitored)
	}
	// Fields unknown to our structs are sent back untouched.
	if body["network"] != "Prime Video" || body["qualityProfileId"] != float64(4) || body["path"] != "/tv/The Expanse" {
		t.Errorf("series object not preserved: %v", body)
	}
	a := e.lastAction()
	if !a.Success || a.DryRun || a.Action != DeleteSeason || *a.SeasonNumber != 2 || a.ImportExclusion ||
		a.Username != "admin" || a.SizeFreedBytes != 4_500_000_000 {
		t.Errorf("journal: %+v", a)
	}
}

func TestDeleteSeasonWithoutFilesOnlyUnmonitors(t *testing.T) {
	e := newEnv(t, false)
	if _, err := e.svc.Delete(context.Background(), Request{MediaID: e.mediaID("series", "The Expanse"),
		Season: intp(1)}); err != nil {
		t.Fatal(err)
	}
	calls := e.destructive()
	if len(calls) != 1 || calls[0].Method != http.MethodPut {
		t.Fatalf("calls: %+v", calls)
	}
	if a := e.lastAction(); a.SizeFreedBytes != 0 || a.DeleteFiles {
		t.Errorf("journal: %+v", a)
	}
}

func TestDryRunDeletesNothing(t *testing.T) {
	e := newEnv(t, true)
	id := e.mediaID("series", "Breaking Bad")
	for _, req := range []Request{
		{MediaID: id, Season: intp(1), DeleteFiles: true},
		{MediaID: id, DeleteFiles: true, ConfirmTitle: "breaking bad"},
		{MediaID: e.mediaID("movie", "Tenet"), DeleteFiles: true},
	} {
		res, err := e.svc.Delete(context.Background(), req)
		if err != nil || !res.DryRun {
			t.Fatalf("dry run: %+v %v", res, err)
		}
		if a := e.lastAction(); !a.DryRun || !a.Success {
			t.Errorf("journal: %+v", a)
		}
	}
	for _, c := range e.mock.Calls() {
		t.Errorf("dry run made a destructive call: %+v", c)
	}
	if _, err := e.st.MediaByID(context.Background(), id); err != nil {
		t.Error("dry run removed the media locally")
	}
}

func TestProtectionTag(t *testing.T) {
	e := newEnv(t, false)
	_, err := e.svc.Delete(context.Background(), Request{MediaID: e.mediaID("series", "Severance"), Season: intp(1), DeleteFiles: true})
	if !errors.Is(err, ErrProtected) {
		t.Fatalf("got %v", err)
	}
	if a := e.lastAction(); a.Success || !strings.Contains(a.Error, "protégé") {
		t.Errorf("refusal must be journaled: %+v", a)
	}

	// Tag added in Sonarr after the last sync: the live value wins.
	var series []map[string]any
	_ = json.Unmarshal(e.fsys["sonarr/series.json"].Data, &series)
	series[0]["tags"] = []int{1} // Breaking Bad gets "keep"
	b, _ := json.Marshal(series)
	e.fsys["sonarr/series.json"] = &fstest.MapFile{Data: b}
	_, err = e.svc.Delete(context.Background(), Request{MediaID: e.mediaID("series", "Breaking Bad"),
		DeleteFiles: true, ConfirmTitle: "Breaking Bad"})
	if !errors.Is(err, ErrProtected) {
		t.Fatalf("live tag ignored: %v", err)
	}
	if calls := e.destructive(); len(calls) != 0 {
		t.Errorf("protected item touched: %+v", calls)
	}
}

func TestDeleteSeriesNeedsTitle(t *testing.T) {
	e := newEnv(t, false)
	id := e.mediaID("series", "Breaking Bad")
	if _, err := e.svc.Delete(context.Background(), Request{MediaID: id, DeleteFiles: true, ConfirmTitle: "Breaking"}); !errors.Is(err, ErrConfirmMismatch) {
		t.Fatalf("got %v", err)
	}
	if len(e.mock.Calls()) != 0 {
		t.Fatal("call made without confirmation")
	}
	res, err := e.svc.Delete(context.Background(), Request{MediaID: id, DeleteFiles: true, ImportExclusion: true,
		ConfirmTitle: "  breaking BAD "})
	if err != nil || res.SizeFreed != 6_000_000_000 {
		t.Fatalf("delete: %+v %v", res, err)
	}
	calls := e.destructive()
	if len(calls) != 1 || calls[0].Method != http.MethodDelete || calls[0].Path != "/api/v3/series/1" ||
		calls[0].Query != "addImportListExclusion=true&deleteFiles=true" {
		t.Fatalf("calls: %+v", calls)
	}
	if _, err := e.st.MediaByID(context.Background(), id); !errors.Is(err, store.ErrNotFound) {
		t.Error("deleted series still stored")
	}
	// History is kept as orphan rows.
	var orphans int
	_ = e.st.DB().QueryRow(`SELECT COUNT(*) FROM watch_events WHERE media_id IS NULL AND grandparent_title = 'Breaking Bad'`).Scan(&orphans)
	if orphans != 8 {
		t.Errorf("orphaned history rows: %d", orphans)
	}
	// Plex: series sections refreshed and trash emptied.
	var trash int
	for _, c := range e.mock.Calls() {
		if c.Service == "plex" && c.Path == "/library/sections/2/emptyTrash" {
			trash++
		}
	}
	if trash != 1 || e.mock.Hits("plex:refresh") != 1 {
		t.Errorf("plex refresh: trash=%d refresh=%d", trash, e.mock.Hits("plex:refresh"))
	}
}

func TestDeleteMovieAndErrors(t *testing.T) {
	e := newEnv(t, false)
	e.svc.ConfirmByTyping = false
	if _, err := e.svc.Delete(context.Background(), Request{MediaID: e.mediaID("movie", "Tenet")}); err != nil {
		t.Fatal(err)
	}
	calls := e.destructive()
	if len(calls) != 1 || calls[0].Path != "/api/v3/movie/3" || calls[0].Query != "addImportExclusion=false&deleteFiles=false" {
		t.Fatalf("calls: %+v", calls)
	}
	if _, err := e.svc.Delete(context.Background(), Request{MediaID: e.mediaID("movie", "Vacances 2019")}); !errors.Is(err, ErrNotDeletable) {
		t.Errorf("plex_only: %v", err)
	}
	if _, err := e.svc.Delete(context.Background(), Request{MediaID: e.mediaID("series", "Arcane"), Season: intp(7)}); !errors.Is(err, ErrNoSuchSeason) {
		t.Errorf("missing season: %v", err)
	}
	if _, err := e.svc.Delete(context.Background(), Request{MediaID: 99999}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown media: %v", err)
	}
}

func TestUnmonitorSeasonKeepsUnknownFields(t *testing.T) {
	raw := []byte(`{"id":2,"futureField":{"x":[1,2]},"seasons":[{"seasonNumber":1,"monitored":true,"extra":1},{"seasonNumber":2,"monitored":true}]}`)
	out, ok, err := sonarr.UnmonitorSeason(raw, 2)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	s := string(out)
	if !strings.Contains(s, `"futureField":{"x":[1,2]}`) || !strings.Contains(s, `"extra":1`) ||
		!strings.Contains(s, `{"monitored":false,"seasonNumber":2}`) {
		t.Errorf("got %s", s)
	}
	if _, ok, _ := sonarr.UnmonitorSeason(raw, 9); ok {
		t.Error("unknown season reported as found")
	}
}
