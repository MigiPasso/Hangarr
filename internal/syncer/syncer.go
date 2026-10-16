// Package syncer orchestrates synchronization: upstream catalogs
// and history are fetched in parallel, stored, matched, then watch
// statistics and watch_state are recomputed.
//
// The directory is named syncer rather than sync to avoid shadowing the
// standard library package.
package syncer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"hangarr/internal/clients/httpx"
	"hangarr/internal/matching"
	"hangarr/internal/store"
	"hangarr/internal/upstream"
)

// Source names, as stored in sync_runs.
const (
	SourceSonarr          = "sonarr"
	SourceRadarr          = "radarr"
	SourcePlex            = "plex"
	SourceJellyfin        = "jellyfin"
	SourceTautulli        = "tautulli"
	SourcePlexHistory     = "plex_history"
	SourceJellyfinHistory = "jellyfin_history"
	cursorTautulli        = "tautulli_history"
	cursorPlexHistory     = "plex_history"
	cursorJellyfinHistory = "jellyfin_history:"
	historyOverlapSecs    = 24 * 60 * 60
)

// DefaultSourceTimeout bounds the fetch of one source: a server that never
// ends a listing must not keep the sync running (and every next one refused).
const DefaultSourceTimeout = 30 * time.Minute

func (s *Syncer) sourceCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	d := s.SourceTimeout
	if d <= 0 {
		d = DefaultSourceTimeout
	}
	return context.WithTimeout(ctx, d)
}

// ErrAlreadyRunning is returned when a sync is requested while one runs.
var ErrAlreadyRunning = errors.New("a sync is already running")

// ErrNotConfigured is returned when Sonarr, Radarr or Plex (or Tautulli when
// it is the history source) is not configured.
var ErrNotConfigured = errors.New("upstream services are not fully configured")

// Syncer runs synchronizations with the clients of Upstreams at the time
// each sync starts; it refuses to run while the configuration is incomplete.
type Syncer struct {
	Store       *store.Store
	Upstreams   *upstream.Registry
	Log         *slog.Logger
	Now         func() time.Time
	AbandonDays int
	// HistoryPageSize overrides the Tautulli page size (tests).
	HistoryPageSize int
	// Location is the time zone of the schedule (TZ).
	Location *time.Location

	// HistoryDebounce overrides DefaultHistoryDebounce (tests).
	HistoryDebounce time.Duration
	// SourceTimeout overrides DefaultSourceTimeout (tests).
	SourceTimeout time.Duration

	historyPending bool
	running        atomic.Bool
	// background tracks the syncs started by Start and RequestHistorySync,
	// awaited by Wait; stopping refuses new ones.
	background   sync.WaitGroup
	stopping     bool
	mu           sync.Mutex
	progress     string // English message key (translated by the UI)
	progressArgs []any
	last         *Result
	schedule     Schedule
	wake         chan struct{}

	// catalogMu orders deletions and the store phase of a sync: a deletion
	// holds it for reading while it runs; a sync holds it while it writes
	// what it fetched, after dropping what was deleted since it started.
	catalogMu sync.RWMutex
	// recomputeMu: one statistics recomputation at a time (sync, deletion,
	// webhook).
	recomputeMu sync.Mutex
}

// BeginDeletion is held while a deletion runs (actions.Refresher): a sync
// does not write its catalogs meanwhile.
func (s *Syncer) BeginDeletion() (end func()) {
	s.catalogMu.RLock()
	return s.catalogMu.RUnlock
}

// SourceResult is the outcome of one source.
type SourceResult struct {
	Source   string
	Items    int
	Duration time.Duration
	Err      error
}

// Result summarizes a sync.
type Result struct {
	StartedAt time.Time
	Duration  time.Duration
	Sources   []SourceResult
	Matches   map[string]int
	Linked    int
	Orphans   int
}

// Failed reports whether any source failed.
func (r *Result) Failed() bool {
	for _, s := range r.Sources {
		if s.Err != nil {
			return true
		}
	}
	return false
}

// Status is the live state shown by the UI.
type Status struct {
	Running      bool
	Progress     string // English message key with fmt verbs
	ProgressArgs []any
	Last         *Result
}

// Status returns the current state.
func (s *Syncer) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Status{Running: s.running.Load(), Progress: s.progress, ProgressArgs: s.progressArgs, Last: s.last}
}

func (s *Syncer) setProgress(format string, args ...any) {
	s.mu.Lock()
	s.progress, s.progressArgs = format, args
	s.mu.Unlock()
}

func (s *Syncer) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// deletionWindow: a deletion journaled that long before a sync started may
// still have been running while the sync read the services (a DELETE may
// take httpx.DefaultDeleteTimeout); dropping an item deleted a bit earlier
// is harmless, it is gone anyway.
const deletionWindow = httpx.DefaultDeleteTimeout + 5*time.Minute

// fetched holds in-memory results of the parallel fetch phase.
type fetched struct {
	series []store.CatalogItem
	movies []store.CatalogItem
	server *serverCatalog
}

// drop removes the items deleted since the fetch started.
func (f *fetched) drop(d store.Deleted) {
	keep := func(items []store.CatalogItem, kind string) []store.CatalogItem {
		gone := d.ArrIDs[kind]
		if len(gone) == 0 {
			return items
		}
		return slices.DeleteFunc(items, func(it store.CatalogItem) bool { return gone[it.ArrID] })
	}
	f.series, f.movies = keep(f.series, "series"), keep(f.movies, "movie")
	if f.server == nil || len(d.ServerKeys) == 0 {
		return
	}
	f.server.items = slices.DeleteFunc(f.server.items, func(it matching.ServerItem) bool { return d.ServerKeys[it.Key] })
	f.server.versions = slices.DeleteFunc(f.server.versions, func(v store.MediaVersion) bool { return d.ServerKeys[v.ItemKey] })
}

// Run performs one full synchronization. A failing source does not stop the
// others; its error is recorded in sync_runs and in the result. The returned
// error is reserved for local failures (database).
func (s *Syncer) Run(ctx context.Context) (*Result, error) {
	up := s.Upstreams.Get()
	if !up.Ready() {
		return nil, ErrNotConfigured
	}
	if !s.running.CompareAndSwap(false, true) {
		return nil, ErrAlreadyRunning
	}
	defer s.running.Store(false)
	return s.run(ctx, up)
}

// Start launches a sync in the background unless one is running or the
// configuration is incomplete; it reports whether a new sync was started.
// ctx should outlive the HTTP request.
func (s *Syncer) Start(ctx context.Context) bool {
	up := s.Upstreams.Get()
	if !up.Ready() || !s.running.CompareAndSwap(false, true) {
		return false
	}
	if !s.addBackground() {
		s.running.Store(false)
		return false
	}
	go func() {
		defer s.background.Done()
		defer s.running.Store(false)
		if _, err := s.run(ctx, up); err != nil && ctx.Err() == nil {
			s.Log.Error("sync failed", "err", err)
		}
	}()
	return true
}

// addBackground registers a background sync; false once Wait was called.
func (s *Syncer) addBackground() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping {
		return false
	}
	s.background.Add(1)
	return true
}

// Wait refuses new background syncs and waits for the running ones (their
// context should be canceled first), so that they finish before the
// database is closed.
func (s *Syncer) Wait() {
	s.mu.Lock()
	s.stopping = true
	s.mu.Unlock()
	s.background.Wait()
}

func (s *Syncer) run(ctx context.Context, up *upstream.Clients) (*Result, error) {
	r := &syncRun{s: s, up: up, start: s.now(), runIDs: map[string]int64{}, finished: map[string]bool{}}
	r.res = &Result{StartedAt: r.start}
	s.setProgress("starting")
	s.Log.Info("sync started")
	// A run left open (shutdown during the fetch, local error) would show
	// as running forever.
	defer r.closeOpenRuns(ctx)
	if err := r.startRuns(ctx); err != nil {
		return nil, err
	}
	f := r.fetchAll(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// No deletion runs during the store phase (catalogMu).
	s.catalogMu.Lock()
	defer s.catalogMu.Unlock()
	if err := r.storeCatalogs(ctx, f); err != nil {
		return nil, err
	}
	if err := r.linkAndMerge(ctx); err != nil {
		return nil, err
	}
	return r.summary(ctx)
}

// syncRun is the state of one synchronization.
type syncRun struct {
	s     *Syncer
	up    *upstream.Clients
	start time.Time

	resMu    sync.Mutex
	res      *Result
	runIDs   map[string]int64 // source → sync_runs id
	finished map[string]bool
}

// fetchedSource is the outcome of the fetch of one catalog source.
type fetchedSource struct {
	start time.Time
	err   error
}

// serverSource is the source of the catalog of the media server in use,
// Plex or Jellyfin ("" when none is configured).
func (r *syncRun) serverSource() string {
	switch {
	case r.up.MediaServer == upstream.Plex && r.up.Plex != nil:
		return SourcePlex
	case r.up.MediaServer == upstream.Jellyfin && r.up.Jellyfin != nil:
		return SourceJellyfin
	}
	return ""
}

func (r *syncRun) record(sr SourceResult) {
	r.resMu.Lock()
	r.res.Sources = append(r.res.Sources, sr)
	r.resMu.Unlock()
	if sr.Err != nil {
		r.s.Log.Warn("sync source failed", "source", sr.Source, "err", sr.Err)
	} else {
		r.s.Log.Info("sync source done", "source", sr.Source, "items", sr.Items, "duration_ms", sr.Duration.Milliseconds())
	}
}

// startRuns records the start of every catalog source in sync_runs.
func (r *syncRun) startRuns(ctx context.Context) error {
	var sources []string
	if r.up.Sonarr != nil {
		sources = append(sources, SourceSonarr)
	}
	if r.up.Radarr != nil {
		sources = append(sources, SourceRadarr)
	}
	if src := r.serverSource(); src != "" {
		sources = append(sources, src)
	}
	for _, src := range sources {
		id, err := r.s.Store.StartSyncRun(ctx, src, r.start)
		if err != nil {
			return err
		}
		r.runIDs[src] = id
	}
	return nil
}

// finish records the outcome of a catalog source.
func (r *syncRun) finish(ctx context.Context, sr SourceResult) {
	r.finished[sr.Source] = true
	if err := r.s.Store.FinishSyncRun(context.WithoutCancel(ctx), r.runIDs[sr.Source], r.s.now(), sr.Items, sr.Err); err != nil {
		r.s.Log.Error("sync run not recorded", "source", sr.Source, "err", err)
	}
	r.record(sr)
}

// closeOpenRuns marks the runs not finished as interrupted.
func (r *syncRun) closeOpenRuns(ctx context.Context) {
	for src, id := range r.runIDs {
		if !r.finished[src] {
			if err := r.s.Store.FinishSyncRun(context.WithoutCancel(ctx), id, r.s.now(), 0, store.ErrSyncInterrupted); err != nil {
				r.s.Log.Error("sync run not recorded", "source", src, "err", err)
			}
		}
	}
}

// fetchResult is what fetchAll read, with the outcome of each catalog source.
type fetchResult struct {
	fetched
	seriesRead, moviesRead, serverRead fetchedSource
}

// fetchAll reads every source in parallel, each with its own deadline
// (sourceCtx). Goroutines never return an error so that one failure does not
// cancel the others; the histories are stored as they arrive.
func (r *syncRun) fetchAll(ctx context.Context) *fetchResult {
	s, up, f := r.s, r.up, &fetchResult{}
	var g errgroup.Group
	fetch := func(out *fetchedSource, read func(context.Context) error) {
		g.Go(func() error {
			out.start = time.Now()
			sctx, cancel := s.sourceCtx(ctx)
			defer cancel()
			out.err = read(sctx)
			return nil
		})
	}
	if up.Sonarr != nil {
		fetch(&f.seriesRead, func(ctx context.Context) (err error) { f.series, err = s.fetchSeries(ctx, up.Sonarr); return })
	}
	if up.Radarr != nil {
		fetch(&f.moviesRead, func(ctx context.Context) (err error) { f.movies, err = s.fetchMovies(ctx, up.Radarr); return })
	}
	switch r.serverSource() {
	case SourcePlex:
		fetch(&f.serverRead, func(ctx context.Context) (err error) { f.server, err = s.fetchPlex(ctx, up.Plex); return })
	case SourceJellyfin:
		fetch(&f.serverRead, func(ctx context.Context) (err error) {
			f.server, err = s.fetchJellyfin(ctx, up.Jellyfin)
			return
		})
	}
	g.Go(func() error {
		sctx, cancel := s.sourceCtx(ctx)
		defer cancel()
		for _, sr := range s.syncHistories(sctx, up) {
			r.record(sr)
		}
		return nil
	})
	_ = g.Wait()
	return f
}

// storeCatalogs writes the catalogs, sequentially: Sonarr and Radarr first,
// then the media server matched against them. What was deleted since the
// fetch started is dropped from what was fetched: it must not come back.
func (r *syncRun) storeCatalogs(ctx context.Context, f *fetchResult) error {
	s, up := r.s, r.up
	s.setProgress("saving the catalog")
	deleted, err := s.Store.DeletedSince(ctx, r.start.Add(-deletionWindow))
	if err != nil {
		return err
	}
	f.drop(deleted)
	if up.Sonarr != nil {
		err := f.seriesRead.err
		if err == nil {
			err = s.Store.ReplaceArrCatalog(ctx, "series", f.series, s.now())
		}
		r.finish(ctx, SourceResult{Source: SourceSonarr, Items: len(f.series), Duration: time.Since(f.seriesRead.start), Err: err})
	}
	if up.Radarr != nil {
		err := f.moviesRead.err
		if err == nil {
			err = s.Store.ReplaceArrCatalog(ctx, "movie", f.movies, s.now())
		}
		r.finish(ctx, SourceResult{Source: SourceRadarr, Items: len(f.movies), Duration: time.Since(f.moviesRead.start), Err: err})
	}
	if src := r.serverSource(); src != "" {
		err := f.serverRead.err
		items := 0
		if err == nil {
			items = len(f.server.items)
			err = s.applyMatches(ctx, f.server, matchHints(f.series, f.movies))
		}
		if err == nil && f.server.versionsOK {
			err = s.Store.ReplaceVersions(ctx, f.server.server, f.server.versions)
		}
		r.finish(ctx, SourceResult{Source: src, Items: items, Duration: time.Since(f.serverRead.start), Err: err})
	}
	return nil
}

// linkAndMerge links the history to the catalog, keeps a single history
// source of the Plex server, and recomputes the statistics.
func (r *syncRun) linkAndMerge(ctx context.Context) error {
	s := r.s
	s.setProgress("linking the history")
	if err := s.linkEvents(ctx); err != nil {
		return fmt.Errorf("link history: %w", err)
	}
	if err := s.mergeHistory(ctx, r.up); err != nil {
		return fmt.Errorf("merge history: %w", err)
	}
	s.setProgress("computing statistics")
	if err := s.recompute(ctx); err != nil {
		return fmt.Errorf("recompute: %w", err)
	}
	return nil
}

// summary completes the result and makes it the last one.
func (r *syncRun) summary(ctx context.Context) (*Result, error) {
	s, res := r.s, r.res
	var err error
	if res.Matches, err = s.Store.MatchCounts(ctx); err != nil {
		return nil, err
	}
	if res.Linked, res.Orphans, err = s.Store.EventCounts(ctx); err != nil {
		return nil, err
	}
	res.Duration = s.now().Sub(r.start)
	s.mu.Lock()
	s.last, s.progress, s.progressArgs = res, "", nil
	s.mu.Unlock()
	s.Log.Info("sync finished", "duration_ms", res.Duration.Milliseconds(), "failed", res.Failed(),
		"linked_events", res.Linked, "orphan_events", res.Orphans)
	return res, nil
}
