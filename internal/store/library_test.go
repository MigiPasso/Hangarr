package store_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"arrstats/internal/clients/plex"
	"arrstats/internal/clients/radarr"
	"arrstats/internal/clients/sonarr"
	"arrstats/internal/clients/tautulli"
	"arrstats/internal/mockarr"
	"arrstats/internal/scoring"
	"arrstats/internal/store"
	"arrstats/internal/syncer"
)

var now = time.Unix(mockarr.Anchor, 0)

// synced returns a store populated by a full sync of the synthetic fixtures.
func synced(t *testing.T) *store.Store {
	t.Helper()
	m := mockarr.New(os.DirFS("../../testdata"), "k")
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
	s := &syncer.Syncer{
		Store: st, Sonarr: sonarr.New(urls["sonarr"], "k"), Radarr: radarr.New(urls["radarr"], "k"),
		Plex: plex.New(urls["plex"], "k"), Tautulli: tautulli.New(urls["tautulli"], "k"),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), AbandonDays: 90, Weights: scoring.DefaultWeights,
		Now: func() time.Time { return now },
	}
	if _, err := s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	return st
}

func titles(t *testing.T, st *store.Store, f store.LibraryFilter) []string {
	t.Helper()
	items, err := st.Library(context.Background(), f, now, 90)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Title)
	}
	return out
}

func states(t *testing.T, st *store.Store, f store.LibraryFilter) map[string]string {
	t.Helper()
	items, err := st.Library(context.Background(), f, now, 90)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, it := range items {
		out[it.Title] = it.State
	}
	return out
}

func TestLibraryFilters(t *testing.T) {
	st := synced(t)
	series := func(f store.LibraryFilter) store.LibraryFilter { f.Kind = "series"; return f }
	movies := func(f store.LibraryFilter) store.LibraryFilter { f.Kind = "movie"; return f }
	cases := []struct {
		name string
		f    store.LibraryFilter
		want []string
	}{
		{"all series by title", series(store.LibraryFilter{}),
			[]string{"Arcane", "Breaking Bad", "The Expanse", "Home Videos", "Severance", "Shōgun"}},
		{"search without accent", series(store.LibraryFilter{Query: "shogun"}), []string{"Shōgun"}},
		{"search upper case with accent", series(store.LibraryFilter{Query: "SHŌGUN"}), []string{"Shōgun"}},
		{"search ignores article", series(store.LibraryFilter{Query: "the expanse"}), []string{"The Expanse"}},
		{"wildcards are escaped", series(store.LibraryFilter{Query: "100%"}), []string{}},
		{"states", series(store.LibraryFilter{States: []string{"in_progress", "abandoned"}}),
			[]string{"The Expanse", "Shōgun"}},
		{"not watched for 90 days (never included)", series(store.LibraryFilter{LastViewed: "90"}),
			[]string{"Arcane", "The Expanse", "Severance"}},
		{"never watched", movies(store.LibraryFilter{LastViewed: "never"}),
			[]string{"Avatar: Fire and Ash", "Tenet", "Vacances 2019"}},
		{"added more than a year ago", movies(store.LibraryFilter{Added: "gt365"}),
			[]string{"Inception", "Interstellar", "Tenet", "Vacances 2019"}},
		{"genre", series(store.LibraryFilter{Genres: []string{"Crime", "History"}}), []string{"Breaking Bad", "Shōgun"}},
		{"tag", series(store.LibraryFilter{Tags: []string{"keep"}}), []string{"Severance"}},
		{"status", series(store.LibraryFilter{Status: "continuing"}), []string{"Severance", "Shōgun"}},
		{"size", series(store.LibraryFilter{SizeMinGB: 5}), []string{"Breaking Bad", "The Expanse"}},
		{"year range", movies(store.LibraryFilter{YearMin: 2014, YearMax: 2020}), []string{"Interstellar", "Tenet", "Vacances 2019"}},
		{"rating", movies(store.LibraryFilter{RatingMin: 8.5}), []string{"Inception", "Interstellar"}},
		{"profile", movies(store.LibraryFilter{Profile: "Ultra-HD"}), []string{"Avatar: Fire and Ash", "Dune"}},
		{"studio", movies(store.LibraryFilter{Network: "Syncopy"}), []string{"Tenet"}},
		{"seen by bob", series(store.LibraryFilter{SeenBy: []int64{1002}}), []string{"Breaking Bad", "Home Videos", "Shōgun"}},
		{"seen by alice and bob", series(store.LibraryFilter{SeenBy: []int64{1001, 1002}}), []string{"Breaking Bad"}},
		{"never seen by alice", movies(store.LibraryFilter{UnseenBy: []int64{1001}}),
			[]string{"Avatar: Fire and Ash", "Tenet", "Vacances 2019"}},
		{"seen by nobody", series(store.LibraryFilter{Nobody: true}), []string{"Arcane", "Severance"}},
		{"min viewers", movies(store.LibraryFilter{ViewersMin: 2}), []string{"Inception"}},
		{"match state", movies(store.LibraryFilter{Match: []string{"arr_only", "plex_only"}}),
			[]string{"Avatar: Fire and Ash", "Vacances 2019"}},
		{"sort by size desc", movies(store.LibraryFilter{Sort: "size", Desc: true}),
			[]string{"Dune", "Interstellar", "Tenet", "Inception", "Avatar: Fire and Ash", "Vacances 2019"}},
		{"sort by last view desc, nulls last", series(store.LibraryFilter{Sort: "last", Desc: true}),
			[]string{"Shōgun", "Breaking Bad", "Home Videos", "The Expanse", "Arcane", "Severance"}},
		{"pagination", series(store.LibraryFilter{Limit: 2, Offset: 2}), []string{"The Expanse", "Home Videos"}},
		{"unknown sort falls back to title", series(store.LibraryFilter{Sort: "1; DROP TABLE media", Limit: 1}),
			[]string{"Arcane"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := titles(t, st, c.f); !slices.Equal(got, c.want) {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestLibraryPerspective(t *testing.T) {
	st := synced(t)
	cases := []struct {
		user int64
		kind string
		want map[string]string
	}{
		// alice watched all of Breaking Bad; Shōgun and Home Videos only by bob.
		{1001, "series", map[string]string{"Breaking Bad": "completed", "The Expanse": "abandoned",
			"Shōgun": "never", "Home Videos": "never", "Severance": "never"}},
		// bob: one Breaking Bad episode 10 days ago.
		{1002, "series", map[string]string{"Breaking Bad": "in_progress", "The Expanse": "never",
			"Shōgun": "in_progress", "Home Videos": "completed"}},
		// alice's Inception play is under the old rating key, linked by title.
		{1001, "movie", map[string]string{"Inception": "completed", "Dune": "in_progress",
			"Interstellar": "abandoned", "Tenet": "never"}},
		{1002, "movie", map[string]string{"Inception": "completed", "Dune": "never"}},
	}
	for _, c := range cases {
		got := states(t, st, store.LibraryFilter{Kind: c.kind, User: c.user})
		for title, want := range c.want {
			if got[title] != want {
				t.Errorf("user %d %s: %s = %s, want %s", c.user, c.kind, title, got[title], want)
			}
		}
	}
	// Watch-state filters apply to the perspective.
	got := titles(t, st, store.LibraryFilter{Kind: "series", User: 1002, States: []string{"in_progress"}})
	if !slices.Equal(got, []string{"Breaking Bad", "Shōgun"}) {
		t.Errorf("bob in progress: %q", got)
	}
	items, _ := st.Library(context.Background(), store.LibraryFilter{Kind: "series", User: 1001, Query: "breaking"}, now, 90)
	if len(items) != 1 || items[0].LastViewedBy != "Alice" || items[0].WatchedEpisodes != 6 || items[0].Plays != 6 {
		t.Errorf("alice's Breaking Bad: %+v", items)
	}
	// Without perspective: last viewer is bob, 7 plays.
	items, _ = st.Library(context.Background(), store.LibraryFilter{Kind: "series", Query: "breaking"}, now, 90)
	if items[0].LastViewedBy != "Bob" || items[0].Plays != 7 || items[0].DownloadedEpisodes != 6 {
		t.Errorf("global Breaking Bad: %+v", items[0])
	}
}

func TestLibrarySummaryAndFacets(t *testing.T) {
	st := synced(t)
	n, size, err := st.LibrarySummary(context.Background(), store.LibraryFilter{Kind: "series"}, now, 90)
	if err != nil || n != 6 || size != 19_400_000_000 {
		t.Fatalf("summary: n=%d size=%d err=%v", n, size, err)
	}
	n, _, _ = st.LibrarySummary(context.Background(), store.LibraryFilter{Kind: "movie", Nobody: true}, now, 90)
	if n != 3 {
		t.Errorf("filtered summary: %d", n)
	}
	f, err := st.Facets(context.Background(), "series")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(f.Genres, "Crime") || !slices.Equal(f.Tags, []string{"anime", "keep"}) ||
		!slices.Contains(f.Networks, "AMC") || len(f.Users) != 3 || f.Users[0].Name != "Alice" {
		t.Errorf("facets: %+v", f)
	}
}

func TestLibraryQueryIsParameterized(t *testing.T) {
	evil := `x' OR 1=1 --`
	q, args := store.LibraryQuery(store.LibraryFilter{Kind: "movie", Query: evil, Genres: []string{evil},
		Tags: []string{evil}, Profile: evil, Network: evil, Status: evil, Match: []string{evil}}, now, 90)
	if strings.Contains(q, "OR 1=1") {
		t.Fatalf("user input concatenated into SQL:\n%s", q)
	}
	if len(args) < 8 {
		t.Fatalf("expected bound args, got %v", args)
	}
}
