package syncer

import (
	"context"
	"errors"
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
		if ctx.Err() != nil {
			return
		}
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

	source := SourcePlexHistory
	if up.HistoryClient() != nil {
		source = SourceTautulli
	}
	start := s.now()
	id, err := s.Store.StartSyncRun(ctx, source, start)
	if err != nil {
		return 0, err
	}
	var n int
	if source == SourceTautulli {
		n, err = s.syncTautulliHistory(ctx, up.HistoryClient())
	} else {
		n, err = s.syncPlexHistory(ctx, up.Plex)
	}
	if ferr := s.Store.FinishSyncRun(context.WithoutCancel(ctx), id, s.now(), n, err); ferr != nil {
		s.Log.Error("sync run not recorded", "source", source, "err", ferr)
	}
	if err != nil {
		return 0, err
	}
	if err := s.linkEvents(ctx); err != nil {
		return 0, err
	}
	if err := s.recompute(ctx); err != nil {
		return 0, err
	}
	s.Log.Info("history sync finished", "source", source, "sessions", n, "duration_ms", s.now().Sub(start).Milliseconds())
	return n, nil
}
