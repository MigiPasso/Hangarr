package store_test

import (
	"context"
	"math"
	"strings"
	"testing"

	"hangarr/internal/store"
)

const dayS = 24 * 60 * 60

func scope() store.CleanupScope {
	return store.CleanupScope{ProtectedTags: []string{"KEEP"}, Cutoff: now.Unix() - 120*dayS}
}

func TestOverview(t *testing.T) {
	st := synced(t)
	o, err := st.Overview(context.Background(), scope(), now.Unix()-30*dayS)
	if err != nil {
		t.Fatal(err)
	}
	// Nobody watched for 120 days: Interstellar (last view ~200 days ago),
	// Tenet (never, added in 2025), The Expanse (left 140 days ago).
	// Severance is protected; Arcane and Avatar have no file.
	want := store.Overview{Series: 5, Movies: 5, TotalBytes: 91_400_000_000, NeverWatchedBytes: 17_400_000_000,
		RecoverableBytes: 44_000_000_000, RecoverableCount: 3, WatchSeconds30d: 20080}
	if *o != want {
		t.Errorf("got %+v\nwant %+v", *o, want)
	}
}

func TestTopAndRanking(t *testing.T) {
	st := synced(t)
	ctx := context.Background()
	top, err := st.TopMedia(ctx, "series", 0, 0, false, 10)
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, x := range top {
		titles = append(titles, x.Title)
	}
	if len(top) != 4 || top[0].Title != "Breaking Bad" || top[0].Plays != 7 || top[0].Viewers != 2 || top[1].Title != "The Expanse" {
		t.Errorf("top series: %q %+v", titles, top[0])
	}
	bob, _ := st.TopMedia(ctx, "movie", 0, 1002, true, 10)
	if len(bob) != 1 || bob[0].Title != "Inception" || bob[0].WatchSeconds != 8880 {
		t.Errorf("bob's movies: %+v", bob)
	}
	recent, _ := st.TopMedia(ctx, "movie", now.Unix()-30*dayS, 0, false, 10)
	if len(recent) != 2 || recent[0].Title != "Dune" && recent[0].Title != "Inception" {
		t.Errorf("movies over 30 days: %+v", recent)
	}

	ranking, err := st.UserRanking(ctx, 0)
	if err != nil || len(ranking) != 2 || ranking[0].Name != "Alice" || ranking[1].Name != "Bob" {
		t.Fatalf("ranking: %+v %v", ranking, err)
	}
	if ranking[1].Plays != 4 || ranking[1].Titles != 4 {
		t.Errorf("bob: %+v", ranking[1])
	}

	cost, _ := st.CostPerPlay(ctx, 3)
	if len(cost) != 3 || cost[0].Title != "Dune" || cost[0].BytesPerPlay != 25_000_000_000 || cost[1].Title != "Interstellar" {
		t.Errorf("cost per play: %+v", cost)
	}

	recentViews, _ := st.RecentViews(ctx, 3)
	if len(recentViews) != 3 || recentViews[0].Title != "Dune" || recentViews[1].Episode != "S01E01 · Anjin" ||
		recentViews[1].UserName != "Bob" {
		t.Errorf("recent: %+v", recentViews)
	}
}

func TestGenresCompletionWho(t *testing.T) {
	st := synced(t)
	ctx := context.Background()
	genres, err := st.GenreShares(ctx, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var stored, plays float64
	for _, g := range genres {
		stored += g.StoredShare
		plays += g.PlaysShare
	}
	if math.Abs(stored-1) > 1e-9 || math.Abs(plays-1) > 1e-9 {
		t.Errorf("shares must sum to 1: %v %v", stored, plays)
	}

	c, err := st.MovieCompletion(ctx, 0, 0, 10)
	if err != nil || c.Started != 3 || c.Completed != 1 || len(c.Unfinished) != 2 || c.Unfinished[0].Title != "Dune" {
		t.Errorf("completion: %+v %v", c, err)
	}

	who, err := st.WhoWatchesWhat(ctx, 2)
	if err != nil || len(who) != 2 || who[0].Title != "The Expanse" || who[1].Title != "Breaking Bad" {
		t.Fatalf("who: %+v %v", who, err)
	}
	if v := who[1].Viewers; len(v) != 2 || v[0].Name != "Bob" || v[1].Name != "Alice" || v[1].Watched != 6 || v[0].Watched != 1 {
		t.Errorf("Breaking Bad viewers: %+v", v)
	}

	b, _ := st.Breakdown(ctx, "transcode_decision", 0, 0, 5)
	if len(b) == 0 || b[0].Label != "direct play" {
		t.Errorf("breakdown: %+v", b)
	}
	if b, _ := st.Breakdown(ctx, "user_id; DROP TABLE media", 0, 0, 5); b != nil {
		t.Error("unknown column must be refused")
	}
}

func TestCleanupLists(t *testing.T) {
	st := synced(t)
	ctx := context.Background()
	items, total, err := st.CleanupCandidates(ctx, scope(), store.CleanupFilter{}, "", false, 100)
	if err != nil {
		t.Fatal(err)
	}
	// Largest first.
	if total != 44_000_000_000 || len(items) != 3 || items[0].Title != "Interstellar" || items[1].Title != "Tenet" ||
		items[2].Title != "The Expanse" {
		t.Fatalf("candidates: total=%d %+v", total, items)
	}
	if tenet := items[1]; tenet.LastViewedAt != nil || tenet.AddedAt == nil || tenet.InactiveSince != *tenet.AddedAt {
		t.Errorf("never-watched Tenet must be inactive since it was added: %+v", tenet)
	}
	// A longer delay drops The Expanse (left 140 days ago).
	strict := scope()
	strict.Cutoff = now.Unix() - 150*dayS
	if items, _, _ := st.CleanupCandidates(ctx, strict, store.CleanupFilter{}, "", false, 100); len(items) != 2 {
		t.Errorf("150 days: %d candidates, want 2", len(items))
	}
	for _, it := range items {
		if it.Title == "Severance" || it.Title == "Avatar: Fire and Ash" || it.Title == "Vacances 2019" {
			t.Errorf("%s must be excluded", it.Title)
		}
	}
	series, _, _ := st.CleanupCandidates(ctx, scope(), store.CleanupFilter{Kind: "series"}, "", false, 100)
	for _, it := range series {
		if it.Kind != "series" {
			t.Errorf("kind filter: %+v", it)
		}
	}
	seasons, sum, err := st.UnwatchedSeasons(ctx, scope(), 10)
	if err != nil || len(seasons) != 1 || seasons[0].Title != "The Expanse" || seasons[0].Season != 2 || sum != 4_500_000_000 {
		t.Errorf("unwatched seasons: %+v %d %v", seasons, sum, err)
	}
	last, _ := st.LastEpisodes(ctx, 1001)
	if len(last) != 2 {
		t.Errorf("alice last episodes: %v", last)
	}
}

func TestDataHealth(t *testing.T) {
	st := synced(t)
	d, err := st.DataHealth(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	// Home Videos and Vacances 2019 are in Plex only; Arcane and Avatar have
	// no file; Shōgun ↔ Shogun is the fuzzy match; Lost's history is orphan.
	if d.PlexOnly.Count != 2 || len(d.PlexOnly.Items) != 1 || d.PlexOnly.Items[0].Title != "Home Videos" {
		t.Errorf("plex only: %+v", d.PlexOnly)
	}
	if d.NoFile.Count != 2 || d.NotInPlex.Count != 0 {
		t.Errorf("no file: %+v, not in plex: %+v", d.NoFile, d.NotInPlex)
	}
	if d.Fuzzy.Count != 1 || d.Fuzzy.Items[0].Title != "Shōgun" {
		t.Errorf("fuzzy: %+v", d.Fuzzy)
	}
	if d.Orphans == 0 {
		t.Error("orphan sessions expected (Lost)")
	}
	runs, err := st.RecentSyncRuns(context.Background(), 100)
	if err != nil || len(runs) == 0 || runs[0].ID < runs[len(runs)-1].ID {
		t.Errorf("recent runs (newest first): %v %v", runs, err)
	}
}

func TestCleanupCandidatesSort(t *testing.T) {
	st := synced(t)
	ctx := context.Background()
	titles := func(sort string, desc bool) string {
		t.Helper()
		items, _, err := st.CleanupCandidates(ctx, scope(), store.CleanupFilter{}, sort, desc, 100)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, it := range items {
			out = append(out, it.Title)
		}
		return strings.Join(out, ",")
	}
	for _, c := range []struct {
		sort string
		desc bool
		want string
	}{
		{"", false, "Interstellar,Tenet,The Expanse"},      // default: largest first
		{"bogus", false, "Interstellar,Tenet,The Expanse"}, // unknown key: default
		{"title", false, "The Expanse,Interstellar,Tenet"}, // title_norm ignores "The": "expanse" first
		{"title", true, "Tenet,Interstellar,The Expanse"},
		{"size", false, "The Expanse,Tenet,Interstellar"},
		{"plays", true, "The Expanse,Interstellar,Tenet"},      // 4, 1, 0 plays
		{"state", false, "Tenet,Interstellar,The Expanse"},     // never, then abandoned (size desc)
		{"inactivity", true, "Tenet,Interstellar,The Expanse"}, // added 2025-01, viewed ~200 / 140 days ago
	} {
		if got := titles(c.sort, c.desc); got != c.want {
			t.Errorf("sort=%q desc=%v: %s, want %s", c.sort, c.desc, got, c.want)
		}
	}
}

func TestCleanupCandidatesFilter(t *testing.T) {
	st := synced(t)
	ctx := context.Background()
	all, _, err := st.CleanupCandidates(ctx, scope(), store.CleanupFilter{}, "", false, 100)
	if err != nil || len(all) < 3 {
		t.Fatalf("candidates: %+v %v", all, err)
	}
	zero, one := 0, 1
	for name, c := range map[string]struct {
		f    store.CleanupFilter
		keep func(store.CleanupItem) bool
	}{
		"never":        {store.CleanupFilter{States: []string{"never"}}, func(it store.CleanupItem) bool { return it.State == "never" }},
		"never+compl.": {store.CleanupFilter{States: []string{"never", "completed"}}, func(it store.CleanupItem) bool { return it.State == "never" || it.State == "completed" }},
		"no plays":     {store.CleanupFilter{PlaysMax: &zero}, func(it store.CleanupItem) bool { return it.Plays == 0 }},
		"played":       {store.CleanupFilter{PlaysMin: 1}, func(it store.CleanupItem) bool { return it.Plays >= 1 }},
		"at most one":  {store.CleanupFilter{PlaysMax: &one}, func(it store.CleanupItem) bool { return it.Plays <= 1 }},
		"10 GB+":       {store.CleanupFilter{SizeMinGB: 10}, func(it store.CleanupItem) bool { return it.SizeBytes >= 10e9 }},
		"under 10 GB":  {store.CleanupFilter{SizeMaxGB: 10}, func(it store.CleanupItem) bool { return it.SizeBytes <= 10e9 }},
	} {
		items, total, err := st.CleanupCandidates(ctx, scope(), c.f, "", false, 100)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var want []string
		var wantTotal int64
		for _, it := range all {
			if c.keep(it) {
				want = append(want, it.Title)
				wantTotal += it.SizeBytes
			}
		}
		var got []string
		for _, it := range items {
			got = append(got, it.Title)
		}
		if strings.Join(got, ",") != strings.Join(want, ",") || total != wantTotal {
			t.Errorf("%s: got %v (%d), want %v (%d)", name, got, total, want, wantTotal)
		}
	}
	if items, _, _ := st.CleanupCandidates(ctx, scope(), store.CleanupFilter{States: []string{"never"}}, "", false, 100); len(items) == 0 || len(items) == len(all) {
		t.Errorf("the scenario must have never-watched and watched candidates: %d of %d", len(items), len(all))
	}
}
