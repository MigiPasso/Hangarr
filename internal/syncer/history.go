package syncer

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// DefaultHistoryDebounce groups the history syncs requested by the Tautulli
// webhook: the first request starts the delay, later ones join it.
const DefaultHistoryDebounce = 30 * time.Second

// RequestHistorySync schedules a history-only sync (Tautulli webhook). It
// reports whether a new sync was scheduled (false: one is already pending).
// ctx must outlive the request.
func (s *Syncer) RequestHistorySync(ctx context.Context) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.historyPending {
		return false
	}
	s.historyPending = true
	delay := s.HistoryDebounce
	if delay <= 0 {
		delay = DefaultHistoryDebounce
	}
	time.AfterFunc(delay, func() {
		s.mu.Lock()
		s.historyPending = false
		s.mu.Unlock()
		if ctx.Err() != nil || !s.addBackground() {
			return
		}
		defer s.background.Done()
		if _, err := s.RunHistory(ctx); errors.Is(err, ErrAlreadyRunning) {
			// A full sync is running and may have read the history before
			// the event: try again after it.
			s.RequestHistorySync(ctx)
		} else if err != nil && ctx.Err() == nil && !errors.Is(err, ErrNotConfigured) {
			s.Log.Error("history sync failed", "err", err)
		}
	})
	return true
}

// RunHistory fetches the new history only (Tautulli, or Plex when it is the
// history source), links it and recomputes the statistics. It returns the
// number of sessions read.
func (s *Syncer) RunHistory(ctx context.Context) (int, error) {
	up := s.Upstreams.Get()
	if !up.Ready() {
		return 0, ErrNotConfigured
	}
	if !s.running.CompareAndSwap(false, true) {
		return 0, ErrAlreadyRunning
	}
	defer s.running.Store(false)

	start := s.now()
	n := 0
	var sources []string
	var failed []error
	// A failing source does not hold back the other one (merged mode):
	// what was read is still linked and counted.
	for _, r := range s.syncHistories(ctx, up) {
		if r.Err != nil {
			failed = append(failed, fmt.Errorf("%s: %w", r.Source, r.Err))
			continue
		}
		n += r.Items
		sources = append(sources, r.Source)
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := s.linkEvents(ctx); err != nil {
		return 0, err
	}
	if err := s.mergeHistory(ctx, up); err != nil {
		return 0, err
	}
	if err := s.recompute(ctx); err != nil {
		return 0, err
	}
	s.Log.Info("history sync finished", "sources", sources, "sessions", n, "duration_ms", s.now().Sub(start).Milliseconds())
	return n, errors.Join(failed...)
}
