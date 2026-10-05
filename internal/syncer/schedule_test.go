package syncer

import (
	"context"
	"testing"
	"time"
)

func TestParseClockTime(t *testing.T) {
	for in, want := range map[string]ClockTime{"04:00": {4, 0}, "23:59": {23, 59}, "00:05": {0, 5}} {
		got, err := ParseClockTime(in)
		if err != nil || got != want || got.String() != in {
			t.Errorf("%s: %v %v", in, got, err)
		}
	}
	for _, bad := range []string{"24:00", "4:00", "12:60", "ab:cd", "", "12:00:00", "-1:00"} {
		if _, err := ParseClockTime(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestNextDaily(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	at := ClockTime{4, 0}
	cases := []struct{ now, want string }{
		{"2026-10-04T03:59:00+02:00", "2026-10-04T04:00:00+02:00"}, // later today
		{"2026-10-04T04:00:00+02:00", "2026-10-05T04:00:00+02:00"}, // exactly now: tomorrow
		{"2026-10-04T21:00:00+02:00", "2026-10-05T04:00:00+02:00"},
		{"2026-10-24T21:00:00+02:00", "2026-10-25T04:00:00+01:00"}, // across the DST change
	}
	for _, c := range cases {
		now, _ := time.Parse(time.RFC3339, c.now)
		want, _ := time.Parse(time.RFC3339, c.want)
		if got := NextDaily(now, at, paris); !got.Equal(want) {
			t.Errorf("now %s: got %s want %s", c.now, got, c.want)
		}
	}
	// 02:30 does not exist on 2026-03-29 in Paris: normalized, still that day.
	now, _ := time.Parse(time.RFC3339, "2026-03-29T01:00:00+01:00")
	if got := NextDaily(now, ClockTime{2, 30}, paris); got.Day() != 29 {
		t.Errorf("DST gap: %s", got)
	}
}

func TestCatchUpDue(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	recent := now.Add(-23 * time.Hour).Unix()
	old := now.Add(-25 * time.Hour).Unix()
	if !CatchUpDue(nil, now) || CatchUpDue(&recent, now) || !CatchUpDue(&old, now) {
		t.Error("catch-up rule")
	}
}

func TestDailyCatchUpAndReschedule(t *testing.T) {
	e := newEnv(t)
	e.s.Now = time.Now
	e.s.SetDailyTime(ClockTime{3, 0}) // wake signal is buffered before Daily starts
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.s.Daily(ctx); close(done) }()

	// Empty database: the startup catch-up sync runs.
	deadline := time.Now().Add(5 * time.Second)
	for e.s.Status().Last == nil {
		if time.Now().After(deadline) {
			t.Fatal("startup sync did not run")
		}
		time.Sleep(20 * time.Millisecond)
	}
	e.s.SetDailyTime(ClockTime{5, 30})
	if got := e.s.DailyTime(); got != (ClockTime{5, 30}) {
		t.Errorf("daily time %v", got)
	}
	if next := e.s.NextRun(); next.Hour() != 5 || next.Minute() != 30 || !next.After(time.Now()) {
		t.Errorf("next run %s", next)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Daily did not stop")
	}

	// Restart right after a sync: no catch-up.
	e.mock.Reset()
	ctx, cancel = context.WithCancel(context.Background())
	go e.s.Daily(ctx)
	time.Sleep(200 * time.Millisecond)
	cancel()
	if n := e.mock.Hits("sonarr:series"); n != 0 {
		t.Errorf("unexpected catch-up sync (%d calls)", n)
	}
}
