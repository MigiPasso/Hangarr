// Package scoring computes watch_state (SPEC §6). Cleanup candidates are a
// plain inactivity rule evaluated in SQL (store.CleanupScope).
// Everything here is pure: inputs come from the store, now is injected.
package scoring

import "time"

// Watch states.
const (
	StateNever      = "never"
	StateInProgress = "in_progress"
	StateCompleted  = "completed"
	StateAbandoned  = "abandoned"
)

// Kinds.
const (
	KindMovie  = "movie"
	KindSeries = "series"
)

// SeasonProgress is the per-season data used for series.
type SeasonProgress struct {
	Number     int
	Downloaded int // episode files on disk
	Watched    int // distinct episodes with at least one complete play
}

// Input describes one media item.
type Input struct {
	Kind           string
	Status         string // series: continuing | ended
	SizeBytes      int64
	AddedAt        *time.Time
	Plays          int
	CompletedPlays int
	LastViewedAt   *time.Time
	Seasons        []SeasonProgress
}

// countedSeasons excludes specials (season 0): they are often partial and
// unmonitored, and would keep most series from ever being "completed".
func countedSeasons(in Input) []SeasonProgress {
	var out []SeasonProgress
	for _, s := range in.Seasons {
		if s.Number > 0 {
			out = append(out, s)
		}
	}
	return out
}

// WatchedEpisodes is the number of watched episodes shown as progress,
// capped per season by the downloaded count when files exist.
func WatchedEpisodes(in Input) int {
	n := 0
	for _, s := range countedSeasons(in) {
		w := s.Watched
		if s.Downloaded > 0 && w > s.Downloaded {
			w = s.Downloaded
		}
		n += w
	}
	return n
}

func recent(last *time.Time, now time.Time, days int) bool {
	return last != nil && now.Sub(*last) <= time.Duration(days)*24*time.Hour
}

// WatchState implements the table of SPEC §6 for all users combined.
func WatchState(in Input, now time.Time, abandonDays int) string {
	if in.Kind == KindMovie {
		switch {
		case in.CompletedPlays > 0:
			return StateCompleted
		case in.Plays == 0:
			return StateNever
		case recent(in.LastViewedAt, now, abandonDays):
			return StateInProgress
		default:
			return StateAbandoned
		}
	}

	seasons := countedSeasons(in)
	watched, downloaded := 0, 0
	complete := true
	for _, s := range seasons {
		watched += s.Watched
		downloaded += s.Downloaded
		if s.Downloaded > 0 && s.Watched < s.Downloaded {
			complete = false
		}
	}
	switch {
	case watched == 0:
		return StateNever
	case complete:
		// Every downloaded episode is watched (or nothing is left on disk).
		return StateCompleted
	case recent(in.LastViewedAt, now, abandonDays):
		return StateInProgress
	default:
		return StateAbandoned
	}
}
