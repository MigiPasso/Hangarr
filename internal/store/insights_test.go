package store_test

import (
	"context"
	"math"
	"testing"

	"arrstats/internal/store"
)

const dayS = 24 * 60 * 60

func scope() store.CleanupScope {
	return store.CleanupScope{ProtectedTags: []string{"KEEP"}, GraceCutoff: now.Unix() - 30*dayS, Threshold: 1}
}

func TestOverview(t *testing.T) {
	st := synced(t)
	o, err := st.Overview(context.Background(), scope(), now.Unix()-30*dayS)
	if err != nil {
		t.Fatal(err)
	}
	// Recoverable: Tenet (score 10.4) + Interstellar (1.2); Severance is
	// protected, Avatar inside the grace period, Expanse below 1.
	want := store.Overview{Series: 5, Movies: 5, TotalBytes: 91_400_000_000, NeverWatchedBytes: 17_400_000_000,
		RecoverableBytes: 35_000_000_000, RecoverableCount: 2, WatchSeconds30d: 20080}
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
	items, total, err := st.CleanupCandidates(ctx, scope(), "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if total != 35_000_000_000 || len(items) == 0 || items[0].Title != "Tenet" || items[1].Title != "Interstellar" {
		t.Fatalf("candidates: total=%d %+v", total, items)
	}
	for _, it := range items {
		if it.Title == "Severance" || it.Title == "Avatar: Fire and Ash" || it.Title == "Vacances 2019" {
			t.Errorf("%s must be excluded", it.Title)
		}
	}
	series, _, _ := st.CleanupCandidates(ctx, scope(), "series", 100)
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
