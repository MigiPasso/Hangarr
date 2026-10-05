// Package scoring computes watch_state and cleanup_score (SPEC §6, §7.5).
// Everything here is pure: inputs come from the store, now is injected.
package scoring

import (
	"math"
	"time"
)

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

// Weights tune the cleanup score.
type Weights struct {
	NeverWatched      float64 // multiplier when never watched
	ActiveContinuing  float64 // multiplier for continuing series watched recently
	ActiveWindowDays  int     // "recently" for ActiveContinuing
	MaxInactivityDays float64 // inactivity cap
}

// DefaultWeights follow SPEC §7.5.
var DefaultWeights = Weights{NeverWatched: 1.5, ActiveContinuing: 0.5, ActiveWindowDays: 60, MaxInactivityDays: 730}

// CleanupScore implements:
//
//	inactivity = days since last view (or since added if never watched), capped
//	score = (inactivity / 365) × log2(1 + size_GB) / (1 + plays)
//	        × NeverWatched if never watched
//	        × ActiveContinuing if continuing series viewed within the window
//
// Higher means a better cleanup candidate. Unknown dates count as the cap.
func CleanupScore(in Input, state string, now time.Time, w Weights) float64 {
	ref := in.LastViewedAt
	if ref == nil {
		ref = in.AddedAt
	}
	inactivity := w.MaxInactivityDays
	if ref != nil {
		inactivity = math.Max(0, math.Min(now.Sub(*ref).Hours()/24, w.MaxInactivityDays))
	}
	sizeGB := float64(in.SizeBytes) / 1e9
	score := inactivity / 365 * math.Log2(1+sizeGB) / float64(1+in.Plays)
	if state == StateNever {
		score *= w.NeverWatched
	}
	if in.Kind == KindSeries && in.Status == "continuing" && recent(in.LastViewedAt, now, w.ActiveWindowDays) {
		score *= w.ActiveContinuing
	}
	return math.Round(score*10000) / 10000
}
