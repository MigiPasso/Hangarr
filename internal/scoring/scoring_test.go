package scoring

import (
	"math"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func daysAgo(d int) *time.Time {
	t := now.Add(-time.Duration(d) * 24 * time.Hour)
	return &t
}

func TestMovieStates(t *testing.T) {
	cases := []struct {
		name string
		in   Input
		want string
	}{
		{"never", Input{Kind: KindMovie}, StateNever},
		{"in progress", Input{Kind: KindMovie, Plays: 1, LastViewedAt: daysAgo(10)}, StateInProgress},
		{"abandoned", Input{Kind: KindMovie, Plays: 2, LastViewedAt: daysAgo(91)}, StateAbandoned},
		{"completed", Input{Kind: KindMovie, Plays: 1, CompletedPlays: 1, LastViewedAt: daysAgo(400)}, StateCompleted},
	}
	for _, c := range cases {
		if got := WatchState(c.in, now, 90); got != c.want {
			t.Errorf("%s: got %s want %s", c.name, got, c.want)
		}
	}
}

func TestSeriesStates(t *testing.T) {
	s := func(n, d, w int) SeasonProgress { return SeasonProgress{Number: n, Downloaded: d, Watched: w} }
	cases := []struct {
		name string
		in   Input
		want string
	}{
		{"never", Input{Kind: KindSeries, Seasons: []SeasonProgress{s(1, 10, 0)}}, StateNever},
		{"only specials watched", Input{Kind: KindSeries, Seasons: []SeasonProgress{s(0, 3, 3), s(1, 10, 0)}}, StateNever},
		{"completed", Input{Kind: KindSeries, Seasons: []SeasonProgress{s(0, 3, 0), s(1, 10, 10), s(2, 8, 8)}}, StateCompleted},
		{"completed, unaired season without files", Input{Kind: KindSeries, Seasons: []SeasonProgress{s(1, 10, 10), s(2, 0, 0)}}, StateCompleted},
		{"in progress", Input{Kind: KindSeries, LastViewedAt: daysAgo(5), Seasons: []SeasonProgress{s(1, 10, 10), s(2, 8, 2)}}, StateInProgress},
		{"abandoned", Input{Kind: KindSeries, LastViewedAt: daysAgo(120), Seasons: []SeasonProgress{s(1, 10, 3)}}, StateAbandoned},
		{"all files deleted after watching", Input{Kind: KindSeries, Seasons: []SeasonProgress{s(1, 0, 4)}}, StateCompleted},
	}
	for _, c := range cases {
		if got := WatchState(c.in, now, 90); got != c.want {
			t.Errorf("%s: got %s want %s", c.name, got, c.want)
		}
	}
	in := Input{Seasons: []SeasonProgress{s(0, 2, 2), s(1, 5, 7), s(2, 0, 3)}}
	if got := WatchedEpisodes(in); got != 8 {
		t.Errorf("WatchedEpisodes = %d, want 8 (5 capped + 3, specials excluded)", got)
	}
}

func TestCleanupScore(t *testing.T) {
	w := DefaultWeights
	// 365 days inactive, 3 GB (log2(4) = 2), 1 play: 1 × 2 / 2 = 1.
	in := Input{Kind: KindMovie, SizeBytes: 3e9, Plays: 1, LastViewedAt: daysAgo(365)}
	if got := CleanupScore(in, StateCompleted, now, w); got != 1 {
		t.Errorf("basic score = %v, want 1", got)
	}
	// Never watched: inactivity from added date, ×1.5.
	in = Input{Kind: KindMovie, SizeBytes: 3e9, AddedAt: daysAgo(365)}
	if got := CleanupScore(in, StateNever, now, w); got != 3 {
		t.Errorf("never watched = %v, want 3 (1 × 2 / 1 × 1.5)", got)
	}
	// Inactivity is capped at 730 days.
	in = Input{Kind: KindMovie, SizeBytes: 3e9, Plays: 1, LastViewedAt: daysAgo(5000)}
	if got := CleanupScore(in, StateCompleted, now, w); got != 2 {
		t.Errorf("capped = %v, want 2", got)
	}
	// Continuing series watched 30 days ago: ×0.5.
	in = Input{Kind: KindSeries, Status: "continuing", SizeBytes: 3e9, Plays: 1, LastViewedAt: daysAgo(30)}
	want := math.Round(30.0/365*2/2*0.5*10000) / 10000
	if got := CleanupScore(in, StateInProgress, now, w); got != want {
		t.Errorf("continuing = %v, want %v", got, want)
	}
	// Empty item scores 0.
	if got := CleanupScore(Input{Kind: KindMovie, AddedAt: daysAgo(100)}, StateNever, now, w); got != 0 {
		t.Errorf("no size = %v", got)
	}
}
