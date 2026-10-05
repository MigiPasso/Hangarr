package syncer

import (
	"context"
	"database/sql"
	"encoding/json"
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

	"cleanarr/internal/clients/sonarr"
	"cleanarr/internal/mockarr"
	"cleanarr/internal/store"
	"cleanarr/internal/upstream"
)

const key = "test-key"

// fixtures copies testdata/ into a mutable in-memory FS.
func fixtures(t *testing.T) fstest.MapFS {
	t.Helper()
	out := fstest.MapFS{}
	src := os.DirFS("../../testdata")
	err := fs.WalkDir(src, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasPrefix(p, "captured/") {
			return err
		}
		b, err := fs.ReadFile(src, p)
		if err != nil {
			return err
		}
		out[p] = &fstest.MapFile{Data: b}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

type env struct {
	t     *testing.T
	mock  *mockarr.Mock
	fsys  fstest.MapFS
	store *store.Store
	s     *Syncer
	urls  map[string]string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	fsys := fixtures(t)
	m := mockarr.New(fsys, key)
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
	s := &Syncer{
		Store: st, Upstreams: upstream.NewRegistry(upstream.Build(upstream.TestValues(urls, key), "auto")),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), AbandonDays: 90,
		Now: func() time.Time { return time.Unix(mockarr.Anchor, 0) },
	}
	return &env{t: t, mock: m, fsys: fsys, store: st, s: s, urls: urls}
}

func (e *env) run() *Result {
	e.t.Helper()
	res, err := e.s.Run(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	return res
}

type row struct {
	ID              int64
	MatchState      string
	MatchMethod     sql.NullString
	Plays           int
	Completed       int
	Viewers         int
	WatchedEpisodes sql.NullInt64
	State           string
	LastBy          sql.NullInt64
	Tags            string
	PlexKey         sql.NullString
	AddedAt         sql.NullInt64
}

func (e *env) media(kind, title string) row {
	e.t.Helper()
	var r row
	err := e.store.DB().QueryRow(`SELECT id, match_state, match_method, play_count, completed_play_count,
		distinct_viewers, watched_episodes, watch_state, last_viewed_by, tags, plex_rating_key, added_at
		FROM media WHERE kind = ? AND title = ?`, kind, title).Scan(&r.ID, &r.MatchState, &r.MatchMethod, &r.Plays,
		&r.Completed, &r.Viewers, &r.WatchedEpisodes, &r.State, &r.LastBy, &r.Tags, &r.PlexKey, &r.AddedAt)
	if err != nil {
		e.t.Fatalf("media %s %q: %v", kind, title, err)
	}
	return r
}

func (e *env) count(query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.store.DB().QueryRow(query, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func TestFullSync(t *testing.T) {
	e := newEnv(t)
	res := e.run()
	if res.Failed() {
		t.Fatalf("sources failed: %+v", res.Sources)
	}
	if res.Matches["matched"] != 8 || res.Matches["arr_only"] != 2 || res.Matches["plex_only"] != 2 {
		t.Errorf("match counts: %v", res.Matches)
	}
	if res.Linked != 19 || res.Orphans != 1 {
		t.Errorf("linked=%d orphans=%d, want 19 and 1 (Lost)", res.Linked, res.Orphans)
	}
	// 22 rows: the live session (no id) and the music track are skipped.
	if n := e.count(`SELECT COUNT(*) FROM watch_events`); n != 20 {
		t.Errorf("watch_events: %d", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM plex_accounts`); n != 3 {
		t.Errorf("plex_accounts: %d (Local user must be skipped)", n)
	}

	type want struct {
		kind, title, match, method, state string
		plays, completed, viewers         int
		watchedEpisodes                   int64 // -1: NULL
	}
	for _, w := range []want{
		// Old rating key 555 resolved by title + year: alice and bob.
		{"movie", "Inception", "matched", "tmdb", "completed", 2, 2, 2, -1},
		// Two sessions with one reference_id = one play, not finished.
		{"movie", "Dune", "matched", "tmdb", "in_progress", 1, 0, 1, -1},
		{"movie", "Interstellar", "matched", "imdb", "abandoned", 1, 0, 1, -1},
		{"movie", "Tenet", "matched", "tmdb", "never", 0, 0, 0, -1},
		{"movie", "Avatar: Fire and Ash", "arr_only", "", "never", 0, 0, 0, -1},
		{"movie", "Vacances 2019", "plex_only", "", "never", 0, 0, 0, -1},
		{"series", "Breaking Bad", "matched", "tvdb", "completed", 7, 7, 2, 6},
		{"series", "The Expanse", "matched", "tvdb", "abandoned", 4, 3, 1, 3},
		{"series", "Shōgun", "matched", "fuzzy", "in_progress", 1, 1, 1, 1},
		{"series", "Severance", "matched", "tvdb", "never", 0, 0, 0, 0},
		{"series", "Arcane", "arr_only", "", "never", 0, 0, 0, 0},
		{"series", "Home Videos", "plex_only", "", "completed", 1, 1, 1, 1},
	} {
		r := e.media(w.kind, w.title)
		got := []any{r.MatchState, r.MatchMethod.String, r.State, r.Plays, r.Completed, r.Viewers}
		exp := []any{w.match, w.method, w.state, w.plays, w.completed, w.viewers}
		for i := range got {
			if got[i] != exp[i] {
				t.Errorf("%s: got %v, want %v", w.title, got, exp)
				break
			}
		}
		if w.watchedEpisodes >= 0 && r.WatchedEpisodes.Int64 != w.watchedEpisodes {
			t.Errorf("%s: watched episodes %d, want %d", w.title, r.WatchedEpisodes.Int64, w.watchedEpisodes)
		}
	}

	inception := e.media("movie", "Inception")
	if inception.LastBy.Int64 != 1002 {
		t.Errorf("Inception last viewed by %d, want bob (1002)", inception.LastBy.Int64)
	}
	if sev := e.media("series", "Severance"); sev.Tags != `["keep"]` {
		t.Errorf("Severance tags: %s", sev.Tags)
	}
	if arcane := e.media("series", "Arcane"); arcane.AddedAt.Valid {
		t.Error("the .NET zero date must be stored as NULL")
	}
	bb := e.media("series", "Breaking Bad")
	for _, c := range []struct{ season, watched int }{{0, 0}, {1, 3}, {2, 3}} {
		if n := e.count(`SELECT watched_episodes FROM seasons WHERE media_id = ? AND season_number = ?`, bb.ID, c.season); n != c.watched {
			t.Errorf("Breaking Bad S%d watched %d, want %d", c.season, n, c.watched)
		}
	}
	var seasonKey string
	_ = e.store.DB().QueryRow(`SELECT plex_rating_key FROM seasons WHERE media_id = ? AND season_number = 1`, bb.ID).Scan(&seasonKey)
	if seasonKey != "2011" {
		t.Errorf("season plex key: %q", seasonKey)
	}
	expanse := e.media("series", "The Expanse")
	if n := e.count(`SELECT watched_episodes FROM seasons WHERE media_id = ? AND season_number = 2`, expanse.ID); n != 0 {
		t.Errorf("Expanse S2 should be a never-watched season, got %d", n)
	}
	runs, _ := e.store.LatestSyncRuns(context.Background())
	if len(runs) != 5 { // sonarr, radarr, plex, tautulli, plex_history (merged)
		t.Fatalf("sync runs: %+v", runs)
	}
	for _, r := range runs {
		if r.Status != "success" {
			t.Errorf("run %s: %s %s", r.Source, r.Status, r.Error)
		}
	}
}

func historyWithExtraRow(t *testing.T, fsys fstest.MapFS, row map[string]any) {
	t.Helper()
	var doc map[string]map[string]any
	if err := json.Unmarshal(fsys["tautulli/get_history.json"].Data, &doc); err != nil {
		t.Fatal(err)
	}
	data := doc["response"]["data"].(map[string]any)
	rows := data["data"].([]any)
	data["data"] = append([]any{row}, rows...)
	b, _ := json.Marshal(doc)
	fsys["tautulli/get_history.json"] = &fstest.MapFile{Data: b}
}

func TestIncrementalHistory(t *testing.T) {
	e := newEnv(t)
	e.s.HistoryPageSize = 5
	e.run()
	// 22 rows in pages of 5: 5 calls, the last one short.
	if n := e.mock.Hits("tautulli:get_history"); n != 5 {
		t.Fatalf("first sync: %d get_history calls, want 5", n)
	}
	cursor, _ := e.store.Cursor(context.Background(), cursorTautulli)
	if cursor != "1790730000" {
		t.Fatalf("cursor = %q", cursor)
	}

	// A new session appears; the next sync stops at the first page entirely
	// older than cursor - 24 h (page 2), and stays idempotent.
	historyWithExtraRow(t, e.fsys, map[string]any{
		"id": 23, "reference_id": 23, "date": 1790740000, "started": 1790740000, "stopped": 1790748000,
		"duration": 8000, "user_id": 1003, "media_type": "movie", "rating_key": 103, "title": "Tenet",
		"year": 2020, "percent_complete": 95, "watched_status": 1,
	})
	e.mock.Reset()
	e.run()
	if n := e.mock.Hits("tautulli:get_history"); n != 2 {
		t.Fatalf("incremental sync: %d get_history calls, want 2", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM watch_events`); n != 21 {
		t.Fatalf("watch_events after incremental sync: %d, want 21", n)
	}
	if tenet := e.media("movie", "Tenet"); tenet.State != "completed" || tenet.Plays != 1 {
		t.Fatalf("Tenet after new session: %+v", tenet)
	}
	cursor, _ = e.store.Cursor(context.Background(), cursorTautulli)
	if cursor != "1790740000" {
		t.Fatalf("cursor = %q", cursor)
	}
}

func TestSourceFailureIsolated(t *testing.T) {
	e := newEnv(t)
	e.run()
	// Sonarr goes down: its catalog is kept, the other sources still sync.
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer down.Close()
	up := *e.s.Upstreams.Get()
	up.Sonarr = sonarr.New(down.URL, key)
	up.Sonarr.HTTP.Backoff = time.Millisecond
	e.s.Upstreams.Set(&up)
	delete(e.fsys, "radarr/movie.json")
	e.fsys["radarr/movie.json"] = &fstest.MapFile{Data: []byte(`[]`)}

	res := e.run()
	var sonarrErr, radarrErr error
	for _, s := range res.Sources {
		switch s.Source {
		case SourceSonarr:
			sonarrErr = s.Err
		case SourceRadarr:
			radarrErr = s.Err
		}
	}
	if sonarrErr == nil || radarrErr != nil {
		t.Fatalf("sonarr err=%v radarr err=%v", sonarrErr, radarrErr)
	}
	if n := e.count(`SELECT COUNT(*) FROM media WHERE kind = 'series' AND arr_id IS NOT NULL`); n != 5 {
		t.Errorf("series must be kept when Sonarr fails, got %d", n)
	}
	// Radarr now returns no movie: movies vanish; those still in Plex
	// become plex_only and keep their history.
	if n := e.count(`SELECT COUNT(*) FROM media WHERE kind = 'movie' AND arr_id IS NOT NULL`); n != 0 {
		t.Errorf("movies removed from Radarr must be deleted, got %d", n)
	}
	if inc := e.media("movie", "Inception"); inc.MatchState != "plex_only" || inc.Plays != 2 {
		t.Errorf("Inception after Radarr removal: %+v", inc)
	}
	runs, _ := e.store.LatestSyncRuns(context.Background())
	for _, r := range runs {
		if r.Source == SourceSonarr && (r.Status != "error" || !strings.Contains(r.Error, "500")) {
			t.Errorf("sonarr run: %+v", r)
		}
	}
}

func TestPlexHistoryFallback(t *testing.T) {
	e := newEnv(t)
	vals := upstream.TestValues(e.urls, key)
	e.s.Upstreams.Set(upstream.Build(vals, "plex"))
	res := e.run()
	if res.Failed() {
		t.Fatalf("sources: %+v", res.Sources)
	}
	if n := e.count(`SELECT COUNT(*) FROM watch_events WHERE source = 'plex_history'`); n != 3 {
		t.Fatalf("plex history events: %d (track must be skipped)", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM plex_accounts`); n != 3 {
		t.Errorf("plex accounts: %d (nameless account 0 skipped)", n)
	}
	if inc := e.media("movie", "Inception"); inc.State != "completed" || inc.LastBy.Int64 != 1002 {
		t.Errorf("Inception: %+v", inc)
	}
	if sh := e.media("series", "Shōgun"); sh.State != "in_progress" {
		t.Errorf("Shogun: %+v", sh)
	}
}

// Merged mode: the Plex history says what was watched, Tautulli enriches the
// plays it knows. Plex numbers the owner 1, Tautulli uses its plex.tv id.
func TestMergedHistory(t *testing.T) {
	e := newEnv(t)
	e.fsys["plex/accounts.json"] = &fstest.MapFile{Data: []byte(`{"MediaContainer":{"Account":[
 {"id":0,"name":""},{"id":1,"name":"ALICE"},{"id":1002,"name":"bob"},{"id":2000,"name":"Lucie"}]}}`)}
	e.fsys["plex/history.json"] = &fstest.MapFile{Data: []byte(`{"MediaContainer":{"size":4,"totalSize":4,"offset":0,"Metadata":[
 {"historyKey":"/status/sessions/history/914","ratingKey":"103","title":"Tenet","type":"movie","viewedAt":1790800000,"accountID":2000},
 {"historyKey":"/status/sessions/history/913","ratingKey":"102","title":"Dune","type":"movie","viewedAt":1790714800,"accountID":1},
 {"historyKey":"/status/sessions/history/912","ratingKey":"104","title":"Interstellar","type":"movie","viewedAt":1790000000,"accountID":1},
 {"historyKey":"/status/sessions/history/911","ratingKey":"101","title":"Inception","type":"movie","viewedAt":1788301680,"accountID":1002}]}}`)}
	vals := upstream.TestValues(e.urls, key)
	// Plex alone first: entries stored under the Plex owner id.
	e.s.Upstreams.Set(upstream.Build(vals, "plex"))
	e.run()
	if n := e.count(`SELECT COUNT(*) FROM watch_events WHERE user_id = 1`); n != 2 {
		t.Fatalf("owner entries before the merge: %d", n)
	}

	e.s.Upstreams.Set(upstream.Build(vals, "auto")) // Tautulli configured: merged
	if res := e.run(); res.Failed() {
		t.Fatalf("sources: %+v", res.Sources)
	}
	if n := e.count(`SELECT COUNT(*) FROM watch_events WHERE user_id = 1`); n != 0 {
		t.Errorf("Plex owner id must be translated to Tautulli's: %d entries left", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM plex_accounts WHERE account_id = 1`); n != 0 {
		t.Error("the Plex owner account duplicates a Tautulli user")
	}
	// Viewers are the Tautulli users: the Plex account Tautulli does not
	// know ("Lucie") is left out, with its plays.
	if n := e.count(`SELECT COUNT(*) FROM plex_accounts`); n != 3 {
		t.Errorf("accounts: %d", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM plex_accounts WHERE account_id = 2000`) +
		e.count(`SELECT COUNT(*) FROM watch_events WHERE user_id = 2000`); n != 0 {
		t.Error("a Plex account unknown to Tautulli must not be a viewer")
	}
	// Dune and Inception are known to Tautulli: only Interstellar stays from Plex.
	var user, secs int64
	if err := e.store.DB().QueryRow(`SELECT user_id, play_seconds FROM watch_events WHERE source = 'plex_history'`).
		Scan(&user, &secs); err != nil {
		t.Fatalf("one Plex-only play expected: %v (%d plex rows)", err,
			e.count(`SELECT COUNT(*) FROM watch_events WHERE source = 'plex_history'`))
	}
	if user != 1001 || secs != 169*60 {
		t.Errorf("Plex-only play: user %d, %d s (want alice, runtime estimate)", user, secs)
	}
	if n := e.count(`SELECT watched FROM watch_events WHERE source = 'tautulli' AND stopped_at = 1790714700`); n != 1 {
		t.Error("the partial Dune session must be completed by the Plex view")
	}
	if got := e.media("movie", "Inception").Plays; got != 2 {
		t.Errorf("Inception plays: %d (Plex view merged, not added)", got)
	}
	if st := e.media("movie", "Interstellar").State; st != "completed" {
		t.Errorf("Interstellar: %s", st)
	}

	// Idempotent: a second sync neither duplicates nor loses anything.
	before := e.count(`SELECT COUNT(*) FROM watch_events`)
	e.run()
	if after := e.count(`SELECT COUNT(*) FROM watch_events`); after != before {
		t.Errorf("events %d → %d after a second sync", before, after)
	}
}

func TestConcurrentRunRejected(t *testing.T) {
	e := newEnv(t)
	e.s.running.Store(true)
	if _, err := e.s.Run(context.Background()); err != ErrAlreadyRunning { //nolint:errorlint // sentinel
		t.Fatalf("got %v", err)
	}
}

func TestHistorySyncDebounced(t *testing.T) {
	e := newEnv(t)
	e.run()
	ctx := context.Background()
	last, _ := e.store.LastSyncAt(ctx)
	hits := e.mock.Hits("tautulli:get_history")
	e.s.HistoryDebounce = 50 * time.Millisecond
	if !e.s.RequestHistorySync(ctx) {
		t.Fatal("first request must schedule a sync")
	}
	for range 2 {
		if e.s.RequestHistorySync(ctx) {
			t.Error("requests during the delay must join the pending one")
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for e.mock.Hits("tautulli:get_history") == hits {
		if time.Now().After(deadline) {
			t.Fatal("history sync did not run")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if n := e.mock.Hits("tautulli:get_history") - hits; n != 1 {
		t.Errorf("one history page expected (incremental), got %d requests", n)
	}
	if e.mock.Hits("sonarr:series") != 1 {
		t.Error("a history sync must not read the catalogs")
	}
	if after, _ := e.store.LastSyncAt(ctx); *after != *last {
		t.Error("a history sync must not count as a full sync (startup catch-up)")
	}
	if !e.s.RequestHistorySync(ctx) {
		t.Error("a new request after the run must schedule again")
	}
}

// Switching the history source from Plex (no durations) to Tautulli must not
// count every play twice: only one source stays in the database.
func TestSwitchHistorySourceKeepsOne(t *testing.T) {
	e := newEnv(t)
	vals := upstream.TestValues(e.urls, key)
	e.s.Upstreams.Set(upstream.Build(vals, "plex"))
	e.run()
	if n := e.count(`SELECT COUNT(*) FROM watch_events WHERE source = 'plex_history'`); n == 0 {
		t.Fatal("plex history expected first")
	}
	inceptionPlexPlays := e.media("movie", "Inception").Plays
	// The Plex server numbers its owner 1, Tautulli uses the plex.tv id:
	// the same person, recorded twice while Plex was the history source.
	if _, err := e.store.DB().Exec(`INSERT INTO plex_accounts(account_id, username, is_active) VALUES (1, 'owner', 1)`); err != nil {
		t.Fatal(err)
	}

	e.s.Upstreams.Set(upstream.Build(vals, "tautulli"))
	e.run()
	if n := e.count(`SELECT COUNT(*) FROM watch_events WHERE source = 'plex_history'`); n != 0 {
		t.Errorf("plex history must be dropped once Tautulli is the source: %d events left", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM sync_cursors WHERE source = 'plex_history'`); n != 0 {
		t.Error("the plex history cursor must be reset (a switch back re-imports everything)")
	}
	if n := e.count(`SELECT COALESCE(SUM(play_seconds), 0) FROM watch_events`); n == 0 {
		t.Error("Tautulli sessions carry durations")
	}
	if n := e.count(`SELECT COUNT(*) FROM plex_accounts WHERE account_id = 1`); n != 0 {
		t.Error("accounts of the dropped source must not stay (duplicated viewers)")
	}
	if n := e.count(`SELECT COUNT(*) FROM plex_accounts`); n != 3 {
		t.Errorf("Tautulli users expected after the switch, got %d accounts", n)
	}
	if got := e.media("movie", "Inception").Plays; got != 2 || got == inceptionPlexPlays+2 {
		t.Errorf("Inception plays after the switch: %d (Tautulli only, not added to Plex's)", got)
	}
}
