package scoring

import (
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
		{"last season finished, earlier ones seen elsewhere", Input{Kind: KindSeries, LastViewedAt: daysAgo(400),
			Seasons: []SeasonProgress{s(1, 10, 0), s(2, 8, 8), s(3, 0, 0)}}, StateCompleted},
		{"last season unfinished", Input{Kind: KindSeries, LastViewedAt: daysAgo(400),
			Seasons: []SeasonProgress{s(1, 10, 10), s(2, 8, 7)}}, StateAbandoned},
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
