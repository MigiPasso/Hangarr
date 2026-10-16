package actions

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"hangarr/internal/clients/radarr"
	"hangarr/internal/clients/sonarr"
	"hangarr/internal/store"
)

// slowRadarr replaces Radarr with the mock behind a proxy whose DELETE of a
// movie takes delay; the movie is gone (404) afterwards when gone is set.
type slowRadarr struct {
	deletes   atomic.Int32
	pendingOK atomic.Bool // the journal row existed when the DELETE arrived
	started   chan struct{}
}

func (e *env) slowRadarr(delay, deleteTimeout time.Duration, removed bool) *slowRadarr {
	e.t.Helper()
	sr := &slowRadarr{started: make(chan struct{}, 10)}
	inner := e.mock.Radarr()
	var goneIDs sync32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v3/movie/") {
			id := strings.TrimPrefix(r.URL.Path, "/api/v3/movie/")
			switch {
			case r.Method == http.MethodDelete:
				sr.deletes.Add(1)
				var n int
				_ = e.st.DB().QueryRow(`SELECT COUNT(*) FROM action_log WHERE state = 'pending'`).Scan(&n)
				sr.pendingOK.Store(n == 1)
				sr.started <- struct{}{}
				time.Sleep(delay)
				if removed {
					goneIDs.add(id)
				}
				w.WriteHeader(http.StatusOK)
				return
			case r.Method == http.MethodGet && goneIDs.has(id):
				http.Error(w, `{"message":"NotFound"}`, http.StatusNotFound)
				return
			}
		}
		inner.ServeHTTP(w, r)
	}))
	e.t.Cleanup(ts.Close)
	up := *e.svc.Upstreams.Get()
	up.Radarr = radarr.New(ts.URL, "k")
	up.Radarr.HTTP.DeleteTimeout, up.Radarr.HTTP.Backoff = deleteTimeout, time.Millisecond
	e.svc.Upstreams.Set(&up)
	e.svc.GoneChecks = []time.Duration{0, 100 * time.Millisecond, 300 * time.Millisecond}
	return sr
}

// sync32 is a tiny concurrent string set.
type sync32 struct{ v atomic.Value }

func (s *sync32) add(k string) {
	m, _ := s.v.Load().(map[string]bool)
	n := map[string]bool{k: true}
	for x := range m {
		n[x] = true
	}
	s.v.Store(n)
}

func (s *sync32) has(k string) bool {
	m, _ := s.v.Load().(map[string]bool)
	return m[k]
}

// BUG-04 / BUG-05: a DELETE answered after the timeout is sent once, the
// journal row is written before it, and a check (GET → 404) reports it done.
func TestSlowDeleteConfirmedGone(t *testing.T) {
	e := newEnv(t, false)
	sr := e.slowRadarr(150*time.Millisecond, 30*time.Millisecond, true)
	id := e.mediaID("movie", "Tenet")
	res, err := e.svc.Delete(context.Background(), Request{MediaID: id, DeleteFiles: true, Username: "admin"})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if sr.deletes.Load() != 1 || !sr.pendingOK.Load() {
		t.Errorf("deletes=%d journal written first=%v", sr.deletes.Load(), sr.pendingOK.Load())
	}
	if res.Note != NoteConfirmedGone {
		t.Errorf("note: %q", res.Note)
	}
	a := e.lastAction()
	if !a.Success || a.State != store.ActionDone || a.Error != NoteConfirmedGone || a.SizeFreedBytes != 15_000_000_000 {
		t.Errorf("journal: %+v", a)
	}
	if _, err := e.st.MediaByID(context.Background(), id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("local row kept: %v", err)
	}
}

// The check finds the movie still there: a failure, journaled as such.
func TestSlowDeleteStillThere(t *testing.T) {
	e := newEnv(t, false)
	e.slowRadarr(150*time.Millisecond, 30*time.Millisecond, false)
	_, err := e.svc.Delete(context.Background(), Request{MediaID: e.mediaID("movie", "Tenet"), DeleteFiles: true})
	if err == nil {
		t.Fatal("no error")
	}
	if a := e.lastAction(); a.Success || a.State != store.ActionDone || a.SizeFreedBytes != 0 {
		t.Errorf("journal: %+v", a)
	}
}

// BUG-05: Stop during a batch: the deletion in flight finishes and is
// journaled, the next items are not sent, Wait returns once it is done.
func TestStopDuringBatch(t *testing.T) {
	e := newEnv(t, false)
	sr := e.slowRadarr(200*time.Millisecond, time.Second, true)
	ids := []int64{e.mediaID("movie", "Tenet"), e.mediaID("movie", "Interstellar"), e.mediaID("movie", "Inception")}
	done := make(chan []BatchItem)
	go func() { done <- e.svc.DeleteBatch(context.Background(), ids, BatchOptions{DeleteFiles: true}) }()
	<-sr.started
	e.svc.Stop()
	waited := make(chan struct{})
	go func() { e.svc.Wait(); close(waited) }()
	report := <-done
	<-waited
	if !report[0].OK || !errors.Is(report[1].Err, ErrStopping) || !errors.Is(report[2].Err, ErrStopping) {
		t.Errorf("report: %+v", report)
	}
	if sr.deletes.Load() != 1 {
		t.Errorf("deletes after Stop: %d", sr.deletes.Load())
	}
	actions, _, _ := e.st.Actions(context.Background(), 0, 10)
	if len(actions) != 1 || !actions[0].Success || actions[0].State != store.ActionDone {
		t.Errorf("journal: %+v", actions)
	}
	if _, err := e.svc.Delete(context.Background(), Request{MediaID: ids[1]}); !errors.Is(err, ErrStopping) {
		t.Errorf("after Stop: %v", err)
	}
}

// BUG-05: a row left pending by a crash is marked interrupted at startup.
func TestPendingActionInterrupted(t *testing.T) {
	e := newEnv(t, false)
	ctx := context.Background()
	if _, err := e.st.BeginAction(ctx, store.Action{At: 1, Action: DeleteMovie, MediaKind: "movie", ArrID: 3, Title: "Tenet"}); err != nil {
		t.Fatal(err)
	}
	if n, err := e.st.InterruptPendingActions(ctx); err != nil || n != 1 {
		t.Fatalf("interrupted %d: %v", n, err)
	}
	if a := e.lastAction(); a.State != store.ActionInterrupted || a.Success || a.Error != store.ErrActionInterrupted {
		t.Errorf("journal: %+v", a)
	}
}

type countingRefresher struct {
	Refresher
	recomputes atomic.Int32
}

func (c *countingRefresher) Recompute(ctx context.Context) error {
	c.recomputes.Add(1)
	return c.Refresher.Recompute(ctx)
}

// BUG-06: a batch recomputes the statistics once, not once per item.
func TestBatchRecomputesOnce(t *testing.T) {
	e := newEnv(t, false)
	cr := &countingRefresher{Refresher: e.svc.Refresher}
	e.svc.Refresher = cr
	report := e.svc.DeleteBatch(context.Background(),
		[]int64{e.mediaID("movie", "Tenet"), e.mediaID("movie", "Interstellar"), e.mediaID("series", "Breaking Bad")},
		BatchOptions{DeleteFiles: true})
	for _, it := range report {
		if !it.OK {
			t.Fatalf("report: %+v", report)
		}
	}
	if n := cr.recomputes.Load(); n != 1 {
		t.Errorf("recomputes: %d, want 1", n)
	}
}

// BUG-10: season files deleted but the season not unmonitored: a partial
// success (size freed, warning), and the series is refreshed locally.
func TestPartialSeasonDeletion(t *testing.T) {
	e := newEnv(t, false)
	inner := e.mock.Sonarr()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	up := *e.svc.Upstreams.Get()
	up.Sonarr = sonarr.New(ts.URL, "k")
	up.Sonarr.HTTP.Backoff = time.Millisecond
	e.svc.Upstreams.Set(&up)

	res, err := e.svc.Delete(context.Background(), Request{MediaID: e.mediaID("series", "The Expanse"),
		Season: intp(2), DeleteFiles: true})
	if err != nil {
		t.Fatalf("partial deletion must not fail: %v", err)
	}
	if !errors.Is(res.Warning, ErrUnmonitorAfterDelete) || res.SizeFreed != 4_500_000_000 {
		t.Errorf("result: %+v", res)
	}
	if a := e.lastAction(); !a.Success || a.SizeFreedBytes != 4_500_000_000 || !strings.Contains(a.Error, "not unmonitored") {
		t.Errorf("journal: %+v", a)
	}
}
