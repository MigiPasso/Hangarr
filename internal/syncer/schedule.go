package syncer

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ClockTime is a time of day.
type ClockTime struct{ Hour, Minute int }

func (c ClockTime) String() string { return fmt.Sprintf("%02d:%02d", c.Hour, c.Minute) }

// ParseClockTime parses "HH:MM" (24 h).
func ParseClockTime(s string) (ClockTime, error) {
	var c ClockTime
	if len(s) != 5 || s[2] != ':' {
		return c, errors.New("expected HH:MM")
	}
	if _, err := fmt.Sscanf(s, "%02d:%02d", &c.Hour, &c.Minute); err != nil ||
		c.Hour < 0 || c.Hour > 23 || c.Minute < 0 || c.Minute > 59 {
		return ClockTime{}, errors.New("expected HH:MM")
	}
	return c, nil
}

// NextDaily returns the next occurrence of at strictly after now, in loc.
// time.Date normalizes DST gaps (02:30 on a spring-forward day runs at 03:30).
func NextDaily(now time.Time, at ClockTime, loc *time.Location) time.Time {
	n := now.In(loc)
	next := time.Date(n.Year(), n.Month(), n.Day(), at.Hour, at.Minute, 0, 0, loc)
	if !next.After(n) {
		next = time.Date(n.Year(), n.Month(), n.Day()+1, at.Hour, at.Minute, 0, 0, loc)
	}
	return next
}

// CatchUpDue reports whether a sync should run at startup: never synced, or
// the last sync is more than a day old (the app was down at the scheduled time).
func CatchUpDue(last *int64, now time.Time) bool {
	return last == nil || now.Sub(time.Unix(*last, 0)) > 24*time.Hour
}

// SetDailyTime changes the daily sync time; a running Daily loop reschedules.
func (s *Syncer) SetDailyTime(at ClockTime) {
	s.mu.Lock()
	s.daily = at
	s.mu.Unlock()
	select {
	case s.wakeCh() <- struct{}{}:
	default:
	}
}

// DailyTime returns the configured daily sync time.
func (s *Syncer) DailyTime() ClockTime {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.daily
}

// NextRun returns when the next scheduled sync will start.
func (s *Syncer) NextRun() time.Time {
	return NextDaily(s.now(), s.DailyTime(), s.location())
}

func (s *Syncer) location() *time.Location {
	if s.Location != nil {
		return s.Location
	}
	return time.Local
}

func (s *Syncer) wakeCh() chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.wake == nil {
		s.wake = make(chan struct{}, 1)
	}
	return s.wake
}

// Daily runs a catch-up sync at startup when due, then one sync per day at
// DailyTime, until ctx ends. Changing the time reschedules immediately.
func (s *Syncer) Daily(ctx context.Context) {
	if last, err := s.Store.LastSyncAt(ctx); err != nil {
		s.Log.Error("reading last sync", "err", err)
	} else if CatchUpDue(last, s.now()) {
		s.runLogged(ctx)
	}
	wake := s.wakeCh()
	for {
		next := s.NextRun()
		s.Log.Info("next sync scheduled", "at", next.Format(time.RFC3339))
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-wake:
			timer.Stop()
		case <-timer.C:
			s.runLogged(ctx)
		}
	}
}

func (s *Syncer) runLogged(ctx context.Context) {
	_, err := s.Run(ctx)
	switch {
	case err == nil, ctx.Err() != nil, errors.Is(err, ErrAlreadyRunning):
	case errors.Is(err, ErrNotConfigured):
		s.Log.Info("scheduled sync skipped: services not configured", "missing", s.Upstreams.Get().Missing)
	default:
		s.Log.Error("sync failed", "err", err)
	}
}
