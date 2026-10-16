package syncer

import (
	"context"
	"errors"
	"fmt"
	"slices"
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

// EveryHours are the allowed sync intervals, in hours: divisors of 24, so
// the sync times are the same every day. 24 is once a day.
var EveryHours = []int{24, 12, 8, 6, 4, 3, 2, 1}

// ValidEvery reports whether h is an allowed interval.
func ValidEvery(h int) bool { return slices.Contains(EveryHours, h) }

// Schedule is when syncs run: at At, then every EveryHours hours, each day.
type Schedule struct {
	At         ClockTime
	EveryHours int // 24: once a day
}

func (sc Schedule) every() int {
	if ValidEvery(sc.EveryHours) {
		return sc.EveryHours
	}
	return 24
}

// Times returns the sync times of a day, in order (04:00 every 6 h → 04:00,
// 10:00, 16:00, 22:00).
func (sc Schedule) Times() []ClockTime {
	every := sc.every()
	out := make([]ClockTime, 0, 24/every)
	for k := range 24 / every {
		out = append(out, ClockTime{Hour: (sc.At.Hour + k*every) % 24, Minute: sc.At.Minute})
	}
	slices.SortFunc(out, func(a, b ClockTime) int { return a.Hour - b.Hour })
	return out
}

// Next returns the next sync time strictly after now, in loc. time.Date
// normalizes DST gaps (02:30 on a spring-forward day runs at 03:30).
func (sc Schedule) Next(now time.Time, loc *time.Location) time.Time {
	n := now.In(loc)
	times := sc.Times()
	for d := 0; ; d++ {
		for _, at := range times {
			if t := time.Date(n.Year(), n.Month(), n.Day()+d, at.Hour, at.Minute, 0, 0, loc); t.After(n) {
				return t
			}
		}
	}
}

// CatchUpDue reports whether a sync should run at startup: never synced, or
// the last sync is older than the interval (the app was down at a scheduled
// time).
func CatchUpDue(last *int64, now time.Time, sc Schedule) bool {
	return last == nil || now.Sub(time.Unix(*last, 0)) > time.Duration(sc.every())*time.Hour
}

// SetSchedule changes the sync schedule; a running Loop reschedules.
func (s *Syncer) SetSchedule(sc Schedule) {
	sc.EveryHours = sc.every()
	s.mu.Lock()
	s.schedule = sc
	s.mu.Unlock()
	select {
	case s.wakeCh() <- struct{}{}:
	default:
	}
}

// Schedule returns the sync schedule.
func (s *Syncer) Schedule() Schedule {
	s.mu.Lock()
	defer s.mu.Unlock()
	sc := s.schedule
	sc.EveryHours = sc.every()
	return sc
}

// LocalNow returns the syncer's clock in the time zone of the schedule.
func (s *Syncer) LocalNow() time.Time {
	return s.now().In(s.location())
}

// NextRun returns when the next scheduled sync will start.
func (s *Syncer) NextRun() time.Time {
	return s.Schedule().Next(s.now(), s.location())
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

// Loop runs a catch-up sync at startup when due, then syncs on the
// schedule, until ctx ends. Changing the schedule reschedules immediately.
func (s *Syncer) Loop(ctx context.Context) {
	if last, err := s.Store.LastSyncAt(ctx); err != nil {
		s.Log.Error("reading last sync", "err", err)
	} else if CatchUpDue(last, s.now(), s.Schedule()) {
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
