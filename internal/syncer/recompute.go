package syncer

import (
	"context"
	"time"

	"hangarr/internal/scoring"
	"hangarr/internal/store"
)

// --- Statistics ----------------------------------------------------------------

func unixPtr(v *int64) *time.Time {
	if v == nil {
		return nil
	}
	t := time.Unix(*v, 0)
	return &t
}

// recompute refreshes play counts and watch_state.
func (s *Syncer) recompute(ctx context.Context) error {
	s.recomputeMu.Lock()
	defer s.recomputeMu.Unlock()
	stats, err := s.Store.LoadMediaStats(ctx)
	if err != nil {
		return err
	}
	now := s.now()
	out := make([]store.Derived, 0, len(stats))
	for _, m := range stats {
		in := scoring.Input{
			Kind: m.Kind, Status: m.Status, SizeBytes: m.SizeBytes, AddedAt: unixPtr(m.AddedAt),
			Plays: m.Plays, CompletedPlays: m.CompletedPlays, LastViewedAt: unixPtr(m.LastViewedAt),
		}
		for _, se := range m.Seasons {
			in.Seasons = append(in.Seasons, scoring.SeasonProgress{Number: se.Number, Downloaded: se.Downloaded, Watched: se.Watched})
		}
		d := store.Derived{MediaStats: m, WatchState: scoring.WatchState(in, now, s.AbandonDays)}
		if m.Kind == scoring.KindSeries {
			d.WatchedEpisodes = intPtr(scoring.WatchedEpisodes(in))
		}
		out = append(out, d)
	}
	return s.Store.SaveDerived(ctx, out)
}
