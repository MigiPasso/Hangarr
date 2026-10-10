package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

// serverEnv holds one arr movie (Inception) and one arr series (Breaking
// Bad), matched against Plex, plus a movie only Plex knows.
type serverEnv struct {
	t     *testing.T
	s     *Store
	ctx   context.Context
	now   time.Time
	movie int64 // Inception
	show  int64 // Breaking Bad
}

func newServerEnv(t *testing.T) *serverEnv {
	e := &serverEnv{t: t, s: openTemp(t), ctx: context.Background(), now: time.Unix(1790812800, 0)}
	if err := e.s.ReplaceArrCatalog(e.ctx, "movie", []CatalogItem{{Kind: "movie", ArrID: 1, Title: "Inception", Year: 2010}}, e.now); err != nil {
		t.Fatal(err)
	}
	if err := e.s.ReplaceArrCatalog(e.ctx, "series", []CatalogItem{{Kind: "series", ArrID: 1, Title: "Breaking Bad",
		Year: 2008, Seasons: []SeasonRecord{{Number: 1, EpisodeFileCount: 3}}}}, e.now); err != nil {
		t.Fatal(err)
	}
	e.movie, e.show = e.id("Inception"), e.id("Breaking Bad")
	e.apply(ServerPlex, []ServerLink{{MediaID: e.movie, Key: "101", Method: "tmdb"}, {MediaID: e.show, Key: "201", Method: "tvdb"}},
		[]ServerOnlyItem{{Kind: "movie", Key: "105", Title: "Vacances 2019", Year: 2019}},
		[]ServerSeason{{ShowKey: "201", Number: 1, Key: "2011"}})
	return e
}

func (e *serverEnv) apply(server string, links []ServerLink, only []ServerOnlyItem, seasons []ServerSeason) {
	e.t.Helper()
	if err := e.s.ApplyServerMatches(e.ctx, server, links, only, seasons, e.now); err != nil {
		e.t.Fatal(err)
	}
}

func (e *serverEnv) id(title string) int64 {
	e.t.Helper()
	var id int64
	if err := e.s.db.QueryRow(`SELECT id FROM media WHERE title = ?`, title).Scan(&id); err != nil {
		e.t.Fatalf("%s: %v", title, err)
	}
	return id
}

func (e *serverEnv) row(title string) (state, plexKey, jellyfinID string) {
	e.t.Helper()
	if err := e.s.db.QueryRow(`SELECT match_state, COALESCE(plex_rating_key, ''), COALESCE(jellyfin_id, '')
		FROM media WHERE title = ?`, title).Scan(&state, &plexKey, &jellyfinID); err != nil {
		e.t.Fatalf("%s: %v", title, err)
	}
	return state, plexKey, jellyfinID
}

func (e *serverEnv) libraryTitles(kind string) []string {
	e.t.Helper()
	items, err := e.s.Library(e.ctx, LibraryFilter{Kind: kind, Limit: 100}, e.now, 30)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []string
	for _, it := range items {
		out = append(out, it.Title)
	}
	return out
}

func toJellyfin(e *serverEnv) {
	e.apply(ServerJellyfin, []ServerLink{{MediaID: e.show, Key: "f0000000000000000000000000000201", Method: "tvdb"}},
		[]ServerOnlyItem{{Kind: "movie", Key: "f0000000000000000000000000000105", Title: "Vacances 2019 (JF)", Year: 2019}},
		[]ServerSeason{{ShowKey: "f0000000000000000000000000000201", Number: 1, Key: "f0000000000000000000000000002011"}})
}

// Switching to Jellyfin freezes and hides the Plex data instead of
// deleting it; switching back brings it back with its local protection.
func TestServerSwitchKeepsTheOtherServer(t *testing.T) {
	e := newServerEnv(t)
	if got := e.s.CatalogServer(e.ctx); got != ServerPlex {
		t.Fatalf("catalog server %q", got)
	}
	vacances := e.id("Vacances 2019")
	if err := e.s.SetLocalProtection(e.ctx, vacances, true); err != nil {
		t.Fatal(err)
	}

	toJellyfin(e)
	if got := e.s.CatalogServer(e.ctx); got != ServerJellyfin {
		t.Fatalf("catalog server %q", got)
	}
	// Inception is not in Jellyfin: arr_only, its Plex key kept for the history.
	if st, pk, jf := e.row("Inception"); st != "arr_only" || pk != "101" || jf != "" {
		t.Errorf("Inception: %s %q %q", st, pk, jf)
	}
	if st, pk, jf := e.row("Breaking Bad"); st != "matched" || pk != "201" || jf != "f0000000000000000000000000000201" {
		t.Errorf("Breaking Bad: %s %q %q", st, pk, jf)
	}
	if st, _, _ := e.row("Vacances 2019"); st != "hidden" {
		t.Errorf("Plex-only item: %s, want hidden", st)
	}
	if st, _, _ := e.row("Vacances 2019 (JF)"); st != "server_only" {
		t.Errorf("Jellyfin-only item: %s", st)
	}
	if got := e.libraryTitles("movie"); len(got) != 2 || got[0] != "Inception" || got[1] != "Vacances 2019 (JF)" {
		t.Errorf("library while Jellyfin is used: %v", got)
	}
	var plexSeason, jfSeason string
	_ = e.s.db.QueryRow(`SELECT COALESCE(plex_rating_key, ''), COALESCE(jellyfin_id, '') FROM seasons
		WHERE media_id = ? AND season_number = 1`, e.show).Scan(&plexSeason, &jfSeason)
	if plexSeason != "2011" || jfSeason != "f0000000000000000000000000002011" {
		t.Errorf("season keys: %q %q", plexSeason, jfSeason)
	}
	h, err := e.s.DataHealth(e.ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if h.ServerOnly.Count != 1 || h.ServerOnly.Items[0].Title != "Vacances 2019 (JF)" {
		t.Errorf("health server only: %+v", h.ServerOnly)
	}

	// Back to Plex: the same row is used again, still protected.
	e.apply(ServerPlex, []ServerLink{{MediaID: e.movie, Key: "101", Method: "tmdb"}, {MediaID: e.show, Key: "201", Method: "tvdb"}},
		[]ServerOnlyItem{{Kind: "movie", Key: "105", Title: "Vacances 2019", Year: 2019}}, nil)
	if id := e.id("Vacances 2019"); id != vacances {
		t.Errorf("Plex-only row recreated: %d, want %d", id, vacances)
	}
	m, err := e.s.MediaByID(e.ctx, vacances)
	if err != nil {
		t.Fatal(err)
	}
	if m.MatchState != "server_only" || !m.ProtectedLocal {
		t.Errorf("Plex-only item back: %s protected=%v", m.MatchState, m.ProtectedLocal)
	}
	if st, _, _ := e.row("Vacances 2019 (JF)"); st != "hidden" {
		t.Errorf("Jellyfin-only item after switching back: %s", st)
	}
	if st, pk, jf := e.row("Breaking Bad"); st != "matched" || pk != "201" || jf != "f0000000000000000000000000000201" {
		t.Errorf("Breaking Bad back on Plex: %s %q %q", st, pk, jf)
	}
}

// A stale item of the active server is deleted; the hidden items of the
// other server are not.
func TestServerMatchesDeleteOnlyTheirOwnStaleItems(t *testing.T) {
	e := newServerEnv(t)
	toJellyfin(e)
	toJellyfinWithout := func() {
		e.apply(ServerJellyfin, nil, nil, nil)
	}
	toJellyfinWithout()
	var n int
	_ = e.s.db.QueryRow(`SELECT COUNT(*) FROM media WHERE title = 'Vacances 2019 (JF)'`).Scan(&n)
	if n != 0 {
		t.Error("a Jellyfin item gone from Jellyfin must be deleted")
	}
	if st, _, _ := e.row("Vacances 2019"); st != "hidden" {
		t.Errorf("the hidden Plex item must stay: %s", st)
	}
	if err := e.s.ApplyServerMatches(e.ctx, "emby", nil, nil, nil, e.now); err == nil {
		t.Error("unknown server accepted")
	}
}

// Events are linked by the keys of their own server, whichever is used.
func TestLinkWatchEventsByServerKey(t *testing.T) {
	e := newServerEnv(t)
	toJellyfin(e)
	s1, e2 := 1, 2
	events := []WatchEvent{
		{Source: "plex_history", SourceID: 1, MediaType: "movie", RatingKey: "101", UserID: 1001, StoppedAt: 10, Watched: true},
		{Source: "jellyfin_history", SourceID: 2, MediaType: "episode", RatingKey: "f0000000000000000000000000020112",
			GrandparentRatingKey: "f0000000000000000000000000000201", SeasonNumber: &s1, EpisodeNumber: &e2, UserID: 1001, StoppedAt: 20, Watched: true},
		// A Jellyfin event never links through a Plex key.
		{Source: "jellyfin_history", SourceID: 3, MediaType: "movie", RatingKey: "101", UserID: 1001, StoppedAt: 30},
	}
	if err := e.s.UpsertWatchEvents(e.ctx, events); err != nil {
		t.Fatal(err)
	}
	if err := e.s.LinkWatchEventsByKey(e.ctx); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[int64]int64{1: e.movie, 2: e.show, 3: 0} {
		var got int64
		_ = e.s.db.QueryRow(`SELECT COALESCE(media_id, 0) FROM watch_events WHERE source_id = ?`, id).Scan(&got)
		if got != want {
			t.Errorf("event %d linked to %d, want %d", id, got, want)
		}
	}
}

// Plex history housekeeping never touches the Jellyfin history.
func TestPlexHistoryCleanupKeepsJellyfin(t *testing.T) {
	e := newServerEnv(t)
	events := []WatchEvent{
		{Source: "plex_history", SourceID: 1, MediaType: "movie", RatingKey: "101", UserID: 1, StoppedAt: 10},
		{Source: "tautulli", SourceID: 2, MediaType: "movie", RatingKey: "101", UserID: 9001, StoppedAt: 20},
		{Source: "jellyfin_history", SourceID: 3, MediaType: "movie", RatingKey: "f0000000000000000000000000000101", UserID: 1, StoppedAt: 30},
		{Source: "jellyfin_history", SourceID: 4, MediaType: "movie", RatingKey: "f0000000000000000000000000000101", UserID: 7, StoppedAt: 40},
	}
	if err := e.s.UpsertWatchEvents(e.ctx, events); err != nil {
		t.Fatal(err)
	}
	if err := e.s.UpsertPlexAccounts(e.ctx, []PlexAccount{{AccountID: 1, Username: "alice"}, {AccountID: 7, Username: "dave"},
		{AccountID: 9001, Username: "alice"}}); err != nil {
		t.Fatal(err)
	}
	count := func(source string) int {
		var n int
		_ = e.s.db.QueryRow(`SELECT COUNT(*) FROM watch_events WHERE source = ?`, source).Scan(&n)
		return n
	}

	// Merged mode: the owner (1 for Plex) becomes Tautulli's 9001, the
	// Jellyfin plays matched to that account follow.
	if err := e.s.RemapHistoryUsers(e.ctx, "plex_history", map[int64]int64{1: 9001}); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = e.s.db.QueryRow(`SELECT COUNT(*) FROM watch_events WHERE user_id = 9001`).Scan(&n)
	if n != 3 {
		t.Errorf("events of 9001 after remap: %d, want 3", n)
	}
	// Tautulli knows 9001 only: dave keeps his Jellyfin play and his account.
	if _, err := e.s.KeepViewers(e.ctx, []int64{9001}); err != nil {
		t.Fatal(err)
	}
	if count("jellyfin_history") != 2 {
		t.Error("KeepViewers deleted Jellyfin plays")
	}
	accs, _ := e.s.Accounts(e.ctx)
	if len(accs) != 2 {
		t.Errorf("accounts after KeepViewers: %+v", accs)
	}
	// Tautulli only: the Plex history goes, the Jellyfin history stays.
	if _, err := e.s.DropOtherHistory(e.ctx, "tautulli", "plex_history"); err != nil {
		t.Fatal(err)
	}
	if count("plex_history") != 0 || count("tautulli") != 1 || count("jellyfin_history") != 2 {
		t.Errorf("after DropOtherHistory: plex %d tautulli %d jellyfin %d", count("plex_history"), count("tautulli"),
			count("jellyfin_history"))
	}
}

func TestAccountIDsByName(t *testing.T) {
	e := newServerEnv(t)
	if err := e.s.UpsertPlexAccounts(e.ctx, []PlexAccount{
		{AccountID: 1001, Username: "Alice", FriendlyName: "Alice L."},
		{AccountID: 1002, Username: "bob", FriendlyName: "alice"}, // a display name never beats a user name
		{AccountID: 1003, Username: "carol"}, {AccountID: 1004, Username: "carol"},
	}); err != nil {
		t.Fatal(err)
	}
	names, err := e.s.AccountIDsByName(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]int64{"alice": 1001, "alice l.": 1001, "bob": 1002, "carol": 1003} {
		if names[name] != want {
			t.Errorf("%s: %d, want %d", name, names[name], want)
		}
	}
}

// The Jellyfin events that repeat a Plex or Tautulli play are merged into it.
func TestMergeServerHistories(t *testing.T) {
	e := newServerEnv(t)
	const day, plexAt = 86400, 1_790_000_000
	ev := func(source string, id, user int64, season, episode *int, at int64, watched bool) WatchEvent {
		mt := "movie"
		if season != nil {
			mt = "episode"
		}
		return WatchEvent{Source: source, SourceID: id, MediaType: mt, UserID: user, SeasonNumber: season,
			EpisodeNumber: episode, StartedAt: at - 3600, StoppedAt: at, Watched: watched}
	}
	events := []WatchEvent{
		// Movie seen on Plex by 1, copied to Jellyfin 2 h later: one play.
		ev("plex_history", 1, 1, nil, nil, plexAt, true),
		ev("jellyfin_history", 101, 1, nil, nil, plexAt+2*3600, true),
		// The same movie seen by 2 on Jellyfin only: a play of its own.
		ev("jellyfin_history", 102, 2, nil, nil, plexAt+2*3600, true),
		// Episode started on Tautulli, finished according to Jellyfin: one
		// play, now watched.
		ev("tautulli", 3, 1, intp(1), intp(1), plexAt, false),
		ev("jellyfin_history", 103, 1, intp(1), intp(1), plexAt+600, true),
		// Another episode on Jellyfin: not the same play.
		ev("jellyfin_history", 104, 1, intp(1), intp(2), plexAt+600, true),
		// Seen again on Jellyfin two days later: a new play.
		ev("plex_history", 5, 3, nil, nil, plexAt, true),
		ev("jellyfin_history", 105, 3, nil, nil, plexAt+2*day, true),
		// Left in one Tautulli session, finished in another: the Jellyfin
		// copy repeats the finished one, the left one stays unfinished.
		ev("tautulli", 6, 4, nil, nil, plexAt, false),
		ev("tautulli", 7, 4, nil, nil, plexAt+3*3600, true),
		ev("jellyfin_history", 106, 4, nil, nil, plexAt+4*3600, true),
	}
	if err := e.s.UpsertWatchEvents(e.ctx, events); err != nil {
		t.Fatal(err)
	}
	// Events are linked to media before the merge (LinkWatchEventsByKey).
	for _, x := range events {
		if _, err := e.s.db.Exec(`UPDATE watch_events SET media_id = ? WHERE source = ? AND source_id = ?`,
			map[bool]int64{true: e.show, false: e.movie}[x.SeasonNumber != nil], x.Source, x.SourceID); err != nil {
			t.Fatal(err)
		}
	}
	st, err := e.s.MergeServerHistories(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Merged != 3 || st.Completed != 1 {
		t.Errorf("stats: %+v, want 3 merged and 1 completed", st)
	}
	left := map[int64]bool{}
	err = e.s.queryEach(e.ctx, `SELECT source_id FROM watch_events WHERE source = 'jellyfin_history'`,
		func(rows *sql.Rows) error {
			var id int64
			err := rows.Scan(&id)
			left[id] = true
			return err
		})
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[int64]bool{101: false, 102: true, 103: false, 104: true, 105: true, 106: false} {
		if left[id] != want {
			t.Errorf("jellyfin event %d kept=%v, want %v", id, left[id], want)
		}
	}
	var watched bool
	_ = e.s.db.QueryRow(`SELECT watched FROM watch_events WHERE source = 'tautulli' AND source_id = 3`).Scan(&watched)
	if !watched {
		t.Error("the Tautulli session must be completed by Jellyfin")
	}
	_ = e.s.db.QueryRow(`SELECT watched FROM watch_events WHERE source = 'tautulli' AND source_id = 6`).Scan(&watched)
	if watched {
		t.Error("a session finished in another one must stay unfinished")
	}
	// Idempotent: the events come back at the next sync and merge again.
	if err := e.s.UpsertWatchEvents(e.ctx, events[1:2]); err != nil {
		t.Fatal(err)
	}
	_, _ = e.s.db.Exec(`UPDATE watch_events SET media_id = ? WHERE source = 'jellyfin_history' AND source_id = 101`, e.movie)
	if st, err := e.s.MergeServerHistories(e.ctx); err != nil || st.Merged != 1 || st.Completed != 0 {
		t.Errorf("second merge: %+v %v", st, err)
	}
}

// Items marked played in bulk (a migration from Plex, dated the day of the
// import) are merged with the Plex plays whatever their dates.
func TestMergeServerHistoriesBulkImport(t *testing.T) {
	e := newServerEnv(t)
	const importAt, plexAt = 1_790_000_030, 1_700_000_000
	var events []WatchEvent
	for i := range BulkImportSize {
		events = append(events, WatchEvent{Source: "jellyfin_history", SourceID: int64(200 + i), MediaType: "episode",
			UserID: 1, SeasonNumber: intp(1), EpisodeNumber: intp(i + 1), StartedAt: importAt + int64(i%3),
			StoppedAt: importAt + int64(i%3), Watched: true})
	}
	// Episode 1 was seen on Plex long ago; episode 2 too, but by someone else.
	events = append(events,
		WatchEvent{Source: "plex_history", SourceID: 1, MediaType: "episode", UserID: 1, SeasonNumber: intp(1),
			EpisodeNumber: intp(1), StartedAt: plexAt, StoppedAt: plexAt, Watched: true},
		WatchEvent{Source: "plex_history", SourceID: 2, MediaType: "episode", UserID: 2, SeasonNumber: intp(1),
			EpisodeNumber: intp(2), StartedAt: plexAt, StoppedAt: plexAt, Watched: true})
	if err := e.s.UpsertWatchEvents(e.ctx, events); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.db.Exec(`UPDATE watch_events SET media_id = ?`, e.show); err != nil {
		t.Fatal(err)
	}
	st, err := e.s.MergeServerHistories(e.ctx)
	if err != nil || st.Merged != 1 {
		t.Fatalf("bulk import: %+v %v, want 1 merged (episode 1 of viewer 1)", st, err)
	}
	// One item fewer than the threshold in that minute: no longer an import.
	if _, err := e.s.db.Exec(`DELETE FROM watch_events WHERE source = 'jellyfin_history' AND source_id = 210`); err != nil {
		t.Fatal(err)
	}
	if err := e.s.UpsertWatchEvents(e.ctx, events[:1]); err != nil {
		t.Fatal(err)
	}
	_, _ = e.s.db.Exec(`UPDATE watch_events SET media_id = ?`, e.show)
	if st, err := e.s.MergeServerHistories(e.ctx); err != nil || st.Merged != 0 {
		t.Errorf("below the threshold: %+v %v", st, err)
	}
}

func intp(n int) *int { return &n }

func TestServerItemTitle(t *testing.T) {
	e := newServerEnv(t)
	toJellyfin(e)
	for _, c := range []struct {
		server, key, title string
		ok                 bool
	}{
		{ServerPlex, "105", "Vacances 2019", true},
		{ServerJellyfin, "f0000000000000000000000000000105", "Vacances 2019 (JF)", true},
		{ServerJellyfin, "105", "", false},
		{"emby", "105", "", false},
		{ServerPlex, "", "", false},
	} {
		title, ok, err := e.s.ServerItemTitle(e.ctx, c.server, c.key)
		if err != nil || ok != c.ok || title != c.title {
			t.Errorf("%s %q: %q %v %v", c.server, c.key, title, ok, err)
		}
	}
}

// A database migrated by other code (an abandoned branch with its own
// migration 7) is refused instead of being used with a schema that differs.
func TestMigrationMismatchRefused(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE schema_migrations SET name = '0007_ratings.sql' WHERE version = 7`); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	_, err = Open(context.Background(), dir)
	if !errors.Is(err, ErrMigrationMismatch) || !strings.Contains(err.Error(), "0007_ratings.sql") ||
		!strings.Contains(err.Error(), "0007_jellyfin.sql") {
		t.Fatalf("got %v", err)
	}
}

// Upgrading a database at version 6: plex_only becomes server_only.
func TestMigration0007RenamesPlexOnly(t *testing.T) {
	s, ctx := openTemp(t), context.Background()
	for _, q := range []string{
		`DROP INDEX media_jellyfin_only`, `DROP INDEX media_jellyfin_id`,
		`ALTER TABLE media DROP COLUMN jellyfin_id`, `ALTER TABLE seasons DROP COLUMN jellyfin_id`,
		`DELETE FROM schema_migrations WHERE version = 7`,
		`INSERT INTO media(kind, plex_rating_key, title, match_state, protected_local) VALUES ('movie', '105', 'Vacances 2019', 'plex_only', 1)`,
	} {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var state string
	var protected bool
	if err := s.db.QueryRow(`SELECT match_state, protected_local FROM media WHERE plex_rating_key = '105'`).Scan(&state, &protected); err != nil {
		t.Fatal(err)
	}
	if state != "server_only" || !protected {
		t.Errorf("after 0007: %s protected=%v", state, protected)
	}
}
