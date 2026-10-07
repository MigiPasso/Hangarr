// Package syncer orchestrates synchronization (SPEC §6): upstream catalogs
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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"hangarr/internal/clients/plex"
	"hangarr/internal/clients/radarr"
	"hangarr/internal/clients/sonarr"
	"hangarr/internal/clients/tautulli"
	"hangarr/internal/matching"
	"hangarr/internal/scoring"
	"hangarr/internal/store"
	"hangarr/internal/upstream"
)

// Source names, as stored in sync_runs.
const (
	SourceSonarr       = "sonarr"
	SourceRadarr       = "radarr"
	SourcePlex         = "plex"
	SourceTautulli     = "tautulli"
	SourcePlexHistory  = "plex_history"
	cursorTautulli     = "tautulli_history"
	cursorPlexHistory  = "plex_history"
	historyOverlapSecs = 24 * 60 * 60
)

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

	historyPending bool
	running        atomic.Bool
	mu             sync.Mutex
	progress       string // English message key (translated by the UI)
	progressArgs   []any
	last           *Result
	schedule       Schedule
	wake           chan struct{}
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

// fetched holds in-memory results of the parallel fetch phase.
type fetched struct {
	series []store.CatalogItem
	movies []store.CatalogItem
	plex   *plexCatalog
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
	go func() {
		defer s.running.Store(false)
		if _, err := s.run(ctx, up); err != nil && ctx.Err() == nil {
			s.Log.Error("sync failed", "err", err)
		}
	}()
	return true
}

func (s *Syncer) run(ctx context.Context, up *upstream.Clients) (*Result, error) {
	start := s.now()
	s.setProgress("starting")
	s.Log.Info("sync started")

	res := &Result{StartedAt: start}
	var resMu sync.Mutex
	record := func(r SourceResult) {
		resMu.Lock()
		res.Sources = append(res.Sources, r)
		resMu.Unlock()
		if r.Err != nil {
			s.Log.Warn("sync source failed", "source", r.Source, "err", r.Err)
		} else {
			s.Log.Info("sync source done", "source", r.Source, "items", r.Items, "duration_ms", r.Duration.Milliseconds())
		}
	}

	var f fetched
	runIDs := map[string]int64{}
	sources := []string{}
	if up.Sonarr != nil {
		sources = append(sources, SourceSonarr)
	}
	if up.Radarr != nil {
		sources = append(sources, SourceRadarr)
	}
	if up.Plex != nil {
		sources = append(sources, SourcePlex)
	}
	for _, src := range sources {
		id, err := s.Store.StartSyncRun(ctx, src, start)
		if err != nil {
			return nil, err
		}
		runIDs[src] = id
	}
	finish := func(r SourceResult) {
		if err := s.Store.FinishSyncRun(context.WithoutCancel(ctx), runIDs[r.Source], s.now(), r.Items, r.Err); err != nil {
			s.Log.Error("sync run not recorded", "source", r.Source, "err", err)
		}
		record(r)
	}

	// Fetch phase: all sources in parallel. Goroutines never return an error
	// so that one failure does not cancel the others.
	var g errgroup.Group
	var seriesErr, moviesErr, plexErr error
	var seriesStart, moviesStart, plexStart time.Time
	if up.Sonarr != nil {
		g.Go(func() error {
			seriesStart = time.Now()
			f.series, seriesErr = s.fetchSeries(ctx, up.Sonarr)
			return nil
		})
	}
	if up.Radarr != nil {
		g.Go(func() error {
			moviesStart = time.Now()
			f.movies, moviesErr = s.fetchMovies(ctx, up.Radarr)
			return nil
		})
	}
	if up.Plex != nil {
		g.Go(func() error {
			plexStart = time.Now()
			f.plex, plexErr = s.fetchPlex(ctx, up.Plex)
			return nil
		})
	}
	g.Go(func() error {
		for _, r := range s.syncHistories(ctx, up) {
			record(r)
		}
		return nil
	})
	_ = g.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Store phase, sequential: catalogs first, then matching against them.
	s.setProgress("saving the catalog")
	if up.Sonarr != nil {
		err := seriesErr
		if err == nil {
			err = s.Store.ReplaceArrCatalog(ctx, "series", f.series, s.now())
		}
		finish(SourceResult{Source: SourceSonarr, Items: len(f.series), Duration: time.Since(seriesStart), Err: err})
	}
	if up.Radarr != nil {
		err := moviesErr
		if err == nil {
			err = s.Store.ReplaceArrCatalog(ctx, "movie", f.movies, s.now())
		}
		finish(SourceResult{Source: SourceRadarr, Items: len(f.movies), Duration: time.Since(moviesStart), Err: err})
	}
	if up.Plex != nil {
		err := plexErr
		items := 0
		if err == nil {
			items = len(f.plex.items)
			err = s.applyMatches(ctx, f.plex, matchHints(f.series, f.movies))
		}
		finish(SourceResult{Source: SourcePlex, Items: items, Duration: time.Since(plexStart), Err: err})
	}

	s.setProgress("linking the history")
	if err := s.linkEvents(ctx); err != nil {
		return nil, fmt.Errorf("link history: %w", err)
	}
	if err := s.mergeHistory(ctx, up); err != nil {
		return nil, fmt.Errorf("merge history: %w", err)
	}
	s.setProgress("computing statistics")
	if err := s.recompute(ctx); err != nil {
		return nil, fmt.Errorf("recompute: %w", err)
	}

	var err error
	if res.Matches, err = s.Store.MatchCounts(ctx); err != nil {
		return nil, err
	}
	if res.Linked, res.Orphans, err = s.Store.EventCounts(ctx); err != nil {
		return nil, err
	}
	res.Duration = s.now().Sub(start)
	s.mu.Lock()
	s.last, s.progress, s.progressArgs = res, "", nil
	s.mu.Unlock()
	s.Log.Info("sync finished", "duration_ms", res.Duration.Milliseconds(), "failed", res.Failed(),
		"linked_events", res.Linked, "orphan_events", res.Orphans)
	return res, nil
}

// --- Sonarr / Radarr ---------------------------------------------------------

func tagLabels(ids []int64, labels map[int64]string) []string {
	var out []string
	for _, id := range ids {
		if l, ok := labels[id]; ok {
			out = append(out, l)
		}
	}
	return out
}

func images(imgs []sonarr.Image) (poster, fanart string) {
	for _, im := range imgs {
		switch im.CoverType {
		case "poster":
			poster = im.RemoteURL
		case "fanart":
			fanart = im.RemoteURL
		}
	}
	return poster, fanart
}

// arrLookups fetches tag labels and quality profile names of an *arr app.
func arrLookups(ctx context.Context, tags func(context.Context) ([]sonarr.Tag, error),
	profiles func(context.Context) ([]sonarr.QualityProfile, error)) (labels, names map[int64]string, err error) {
	var ts []sonarr.Tag
	var ps []sonarr.QualityProfile
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() (err error) { ts, err = tags(gctx); return })
	g.Go(func() (err error) { ps, err = profiles(gctx); return })
	if err := g.Wait(); err != nil {
		return nil, nil, err
	}
	labels, names = map[int64]string{}, map[int64]string{}
	for _, t := range ts {
		labels[t.ID] = t.Label
	}
	for _, p := range ps {
		names[p.ID] = p.Name
	}
	return labels, names, nil
}

func seriesItem(se sonarr.Series, labels, names map[int64]string) store.CatalogItem {
	poster, fanart := images(se.Images)
	it := store.CatalogItem{
		Kind: "series", ArrID: se.ID, Title: se.Title, SortTitle: se.SortTitle, TitleSlug: se.TitleSlug,
		Year: int(se.Year), Overview: se.Overview, TvdbID: int64(se.TvdbID), TmdbID: int64(se.TmdbID),
		ImdbID: se.ImdbID, Status: se.Status, Genres: se.Genres, NetworkOrStudio: se.Network,
		Rating: float64(se.Ratings.Value), AddedAt: se.Added.UnixPtr(), Tags: tagLabels(se.Tags, labels),
		QualityProfile: names[se.QualityProfileID], Monitored: se.Monitored, Path: se.Path,
		RuntimeMinutes: int(se.Runtime), PosterURL: poster, FanartURL: fanart,
		AltTitles: altTitles("", se.AlternateTitles),
	}
	if se.Statistics != nil {
		it.SizeBytes = int64(se.Statistics.SizeOnDisk)
	}
	// Episode counts exclude specials, consistently with watch_state.
	total, downloaded := 0, 0
	for _, sn := range se.Seasons {
		rec := store.SeasonRecord{Number: sn.SeasonNumber, Monitored: sn.Monitored}
		if st := sn.Statistics; st != nil {
			rec.EpisodeFileCount, rec.TotalEpisodeCount = int(st.EpisodeFileCount), int(st.TotalEpisodeCount)
			rec.SizeBytes = int64(st.SizeOnDisk)
		}
		if sn.SeasonNumber > 0 {
			total += rec.TotalEpisodeCount
			downloaded += rec.EpisodeFileCount
		}
		it.Seasons = append(it.Seasons, rec)
	}
	it.TotalEpisodes, it.DownloadedEpisodes = &total, &downloaded
	it.HasFile = downloaded > 0
	return it
}

func movieItem(m radarr.Movie, labels, names map[int64]string) store.CatalogItem {
	poster, fanart := images(m.Images)
	return store.CatalogItem{
		Kind: "movie", ArrID: m.ID, Title: m.Title, SortTitle: m.SortTitle, TitleSlug: m.TitleSlug,
		Year: int(m.Year), Overview: m.Overview, TmdbID: int64(m.TmdbID), ImdbID: m.ImdbID, Status: m.Status,
		Genres: m.Genres, NetworkOrStudio: m.Studio, Rating: m.Ratings.Best(), SizeBytes: int64(m.SizeOnDisk),
		AddedAt: m.Added.UnixPtr(), Tags: tagLabels(m.Tags, labels), QualityProfile: names[m.QualityProfileID],
		Monitored: m.Monitored, Path: m.Path, HasFile: m.HasFile, RuntimeMinutes: int(m.Runtime),
		PosterURL: poster, FanartURL: fanart, AltTitles: altTitles(m.OriginalTitle, m.AlternateTitles),
		FileName: fileName(m),
	}
}

func fileName(m radarr.Movie) string {
	if m.MovieFile == nil {
		return ""
	}
	return m.MovieFile.RelativePath
}

// altTitles lists the original and alternate titles, without duplicates.
func altTitles(original string, alt []sonarr.AlternateTitle) []string {
	var out []string
	seen := map[string]bool{}
	add := func(t string) {
		if t != "" && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	add(original)
	for _, a := range alt {
		add(a.Title)
	}
	return out
}

func (s *Syncer) fetchSeries(ctx context.Context, c *sonarr.Client) ([]store.CatalogItem, error) {
	var series []sonarr.Series
	var labels, names map[int64]string
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() (err error) { series, err = c.Series(gctx); return })
	g.Go(func() (err error) {
		labels, names, err = arrLookups(gctx, c.Tags, c.QualityProfiles)
		return
	})
	if err := g.Wait(); err != nil {
		return nil, err
	}
	out := make([]store.CatalogItem, 0, len(series))
	for _, se := range series {
		out = append(out, seriesItem(se, labels, names))
	}
	return out, nil
}

func (s *Syncer) fetchMovies(ctx context.Context, c *radarr.Client) ([]store.CatalogItem, error) {
	var movies []radarr.Movie
	var labels, names map[int64]string
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() (err error) { movies, err = c.Movies(gctx); return })
	g.Go(func() (err error) {
		labels, names, err = arrLookups(gctx, c.Tags, c.QualityProfiles)
		return
	})
	if err := g.Wait(); err != nil {
		return nil, err
	}
	out := make([]store.CatalogItem, 0, len(movies))
	for _, m := range movies {
		out = append(out, movieItem(m, labels, names))
	}
	return out, nil
}

// RefreshSeries re-reads one series from Sonarr (targeted resync after a
// season deletion) and recomputes statistics.
func (s *Syncer) RefreshSeries(ctx context.Context, arrID int64) error {
	c := s.Upstreams.Get().Sonarr
	if c == nil {
		return upstream.ErrNotConfigured
	}
	se, _, err := c.SeriesByID(ctx, arrID)
	if err != nil {
		return err
	}
	labels, names, err := arrLookups(ctx, c.Tags, c.QualityProfiles)
	if err != nil {
		return err
	}
	if err := s.Store.UpsertCatalogItem(ctx, "series", seriesItem(*se, labels, names), s.now()); err != nil {
		return err
	}
	return s.Recompute(ctx)
}

// Recompute refreshes play counts and watch_state from the
// stored data (after a local change such as a deletion).
func (s *Syncer) Recompute(ctx context.Context) error {
	return s.recompute(ctx)
}

// --- Plex catalog and matching ----------------------------------------------

type plexCatalog struct {
	items   []matching.PlexItem
	seasons []store.PlexSeason
}

func (s *Syncer) fetchPlex(ctx context.Context, c *plex.Client) (*plexCatalog, error) {
	if id, err := c.Identity(ctx); err == nil && id != "" {
		if err := s.Store.SetSetting(ctx, store.SettingPlexMachineID, id, s.now()); err != nil {
			return nil, err
		}
	}
	sections, err := c.Sections(ctx)
	if err != nil {
		return nil, err
	}
	cat := &plexCatalog{}
	for _, sec := range sections {
		key := string(sec.Key)
		switch sec.Type {
		case "movie":
			items, err := c.Items(ctx, key, plex.TypeMovie)
			if err != nil {
				return nil, err
			}
			for _, it := range items {
				cat.items = append(cat.items, plexItem(matching.KindMovie, it))
			}
		case "show":
			shows, err := c.Items(ctx, key, plex.TypeShow)
			if err != nil {
				return nil, err
			}
			for _, it := range shows {
				cat.items = append(cat.items, plexItem(matching.KindSeries, it))
			}
			seasons, err := c.Items(ctx, key, plex.TypeSeason)
			if err != nil {
				return nil, err
			}
			for _, se := range seasons {
				cat.seasons = append(cat.seasons, store.PlexSeason{
					ShowRatingKey: string(se.ParentRatingKey), Number: int(se.Index), RatingKey: string(se.RatingKey)})
			}
		}
	}
	return cat, nil
}

func plexItem(kind string, it plex.Item) matching.PlexItem {
	tvdb, tmdb, imdb := it.ExternalIDs()
	p := matching.PlexItem{Kind: kind, RatingKey: string(it.RatingKey), Title: it.Title, Year: int(it.Year),
		TvdbID: tvdb, TmdbID: tmdb, ImdbID: imdb, AddedAt: int64(it.AddedAt), OriginalTitle: it.OriginalTitle,
		Files: it.Files()}
	for _, l := range it.Location {
		p.Folders = append(p.Folders, l.Path)
	}
	return p
}

// arrHints are the matching data fetched from Sonarr/Radarr but not stored.
type arrHints struct {
	titles       []string
	folder, file string
}

func matchHints(series, movies []store.CatalogItem) map[string]arrHints {
	out := map[string]arrHints{}
	for _, list := range [][]store.CatalogItem{series, movies} {
		for _, it := range list {
			out[it.Kind+"|"+strconv.FormatInt(it.ArrID, 10)] = arrHints{titles: it.AltTitles, folder: it.Path, file: it.FileName}
		}
	}
	return out
}

func (s *Syncer) applyMatches(ctx context.Context, cat *plexCatalog, hints map[string]arrHints) error {
	arr, err := s.Store.ListArrMedia(ctx)
	if err != nil {
		return err
	}
	items := make([]matching.ArrItem, 0, len(arr))
	for _, m := range arr {
		h := hints[m.Kind+"|"+strconv.FormatInt(m.ArrID, 10)]
		items = append(items, matching.ArrItem{MediaID: m.ID, Kind: m.Kind, TvdbID: m.TvdbID, TmdbID: m.TmdbID,
			ImdbID: m.ImdbID, Title: m.Title, Year: m.Year, AltTitles: h.titles, Folder: h.folder, File: h.file})
	}
	links, plexOnly := matching.Match(items, cat.items)
	storeLinks := make([]store.PlexLink, 0, len(links))
	for _, l := range links {
		storeLinks = append(storeLinks, store.PlexLink{MediaID: l.MediaID, RatingKey: l.RatingKey, Method: l.Method})
	}
	only := make([]store.PlexOnlyItem, 0, len(plexOnly))
	for _, p := range plexOnly {
		only = append(only, store.PlexOnlyItem{Kind: p.Kind, RatingKey: p.RatingKey, Title: p.Title, Year: p.Year,
			TvdbID: p.TvdbID, TmdbID: p.TmdbID, ImdbID: p.ImdbID, AddedAt: p.AddedAt})
	}
	return s.Store.ApplyPlexMatches(ctx, storeLinks, only, cat.seasons, s.now())
}

// --- History -----------------------------------------------------------------

func intPtr(n int) *int { return &n }

// TautulliEvent converts a history row; ok is false for rows that are not
// stored (no id yet, or not a movie/episode).
func TautulliEvent(r tautulli.HistoryRow) (store.WatchEvent, bool) {
	if r.ID <= 0 || (r.MediaType != "movie" && r.MediaType != "episode") {
		return store.WatchEvent{}, false
	}
	e := store.WatchEvent{
		Source: SourceTautulli, SourceID: int64(r.ID), MediaType: r.MediaType,
		RatingKey: string(r.RatingKey), ParentRatingKey: string(r.ParentRatingKey),
		GrandparentRatingKey: string(r.GrandparentRatingKey), UserID: int64(r.UserID),
		StartedAt: int64(r.Started), StoppedAt: int64(r.Stopped), PlaySeconds: r.PlaySeconds(),
		PercentComplete: int(r.PercentComplete), Watched: r.Watched(), Platform: r.Platform, Player: r.Player,
		TranscodeDecision: r.TranscodeDecision, Title: r.Title, GrandparentTitle: r.GrandparentTitle,
		Year: int(r.Year),
	}
	if e.StartedAt == 0 {
		e.StartedAt = int64(r.Date)
	}
	if e.StoppedAt == 0 {
		e.StoppedAt = e.StartedAt
	}
	if r.ReferenceID > 0 {
		ref := int64(r.ReferenceID)
		e.ReferenceID = &ref
	}
	if r.MediaType == "episode" {
		e.SeasonNumber, e.EpisodeNumber = intPtr(int(r.ParentMediaIndex)), intPtr(int(r.MediaIndex))
	}
	return e, true
}

// syncTautulliHistory pages from newest to oldest and stops once a whole
// page is older than the cursor minus 24 h; the overlap catches long
// sessions that finished late, and the upsert keeps it idempotent.
func (s *Syncer) syncTautulliHistory(ctx context.Context, c *tautulli.Client, users []tautulli.User, v *viewers) (int, error) {
	var accounts []store.PlexAccount
	if v != nil {
		accounts = v.accounts
	} else {
		for _, u := range users {
			if u.UserID == 0 {
				continue
			}
			accounts = append(accounts, store.PlexAccount{AccountID: int64(u.UserID), Username: u.Username,
				FriendlyName: u.FriendlyName, IsActive: u.IsActive == 1, ThumbURL: u.Thumb})
		}
	}
	if err := s.Store.UpsertPlexAccounts(ctx, accounts); err != nil {
		return 0, err
	}
	if v != nil {
		n, err := s.Store.KeepViewers(ctx, v.keep())
		if err != nil {
			return 0, err
		}
		if n > 0 {
			s.Log.Info("plays of users unknown to Tautulli dropped", "deleted_events", n)
		}
	}
	pageSize := tautulli.HistoryPageSize
	if s.HistoryPageSize > 0 {
		pageSize = s.HistoryPageSize
	}
	return s.pageHistory(ctx, cursorTautulli, func(start int) ([]store.WatchEvent, []int64, int, int, error) {
		p, err := c.History(ctx, start, pageSize)
		if err != nil {
			return nil, nil, 0, 0, err
		}
		var events []store.WatchEvent
		dates := make([]int64, 0, len(p.Rows))
		for _, r := range p.Rows {
			dates = append(dates, int64(r.Date))
			if e, ok := TautulliEvent(r); ok && v.allows(e.UserID) {
				events = append(events, e)
			}
		}
		return events, dates, len(p.Rows), int(p.RecordsFiltered), nil
	}, pageSize)
}

// viewers is the viewer list of merged mode: the Tautulli users. The Plex
// server accounts are matched to them by name (the server owner is 1 for
// Plex and its plex.tv id for Tautulli); Plex accounts Tautulli does not
// know are left out, with their plays.
type viewers struct {
	ids      map[int64]int64 // Plex account id → Tautulli user id
	accounts []store.PlexAccount
}

func newViewers(accs []plex.Account, users []tautulli.User) *viewers {
	v := &viewers{ids: map[int64]int64{}}
	byName := map[string]int64{}
	for _, u := range users {
		if u.UserID == 0 {
			continue
		}
		v.accounts = append(v.accounts, store.PlexAccount{AccountID: int64(u.UserID), Username: u.Username,
			FriendlyName: u.FriendlyName, IsActive: u.IsActive == 1, ThumbURL: u.Thumb})
		if u.FriendlyName != "" {
			byName[strings.ToLower(u.FriendlyName)] = int64(u.UserID)
		}
	}
	for _, u := range users { // the user name wins over a display name
		if u.UserID != 0 && u.Username != "" {
			byName[strings.ToLower(u.Username)] = int64(u.UserID)
		}
	}
	for _, a := range accs {
		if a.ID <= 0 || a.Name == "" {
			continue
		}
		if tid, ok := byName[strings.ToLower(a.Name)]; ok {
			v.ids[int64(a.ID)] = tid
		}
	}
	return v
}

func (v *viewers) keep() []int64 {
	out := make([]int64, 0, len(v.accounts))
	for _, a := range v.accounts {
		out = append(out, a.AccountID)
	}
	return out
}

// allows reports whether the plays of a stored user id are kept (always
// without a viewer list).
func (v *viewers) allows(id int64) bool {
	if v == nil {
		return true
	}
	for _, a := range v.accounts {
		if a.AccountID == id {
			return true
		}
	}
	return false
}

// syncHistories reads the history sources of up, one after the other:
// Tautulli, then the Plex history (merged mode: both, Plex ids translated to
// Tautulli's). In a single-source mode the other source is dropped first.
// Each source is recorded in sync_runs.
func (s *Syncer) syncHistories(ctx context.Context, up *upstream.Clients) []SourceResult {
	var out []SourceResult
	step := func(source string, fn func() (int, error)) {
		start := s.now()
		id, err := s.Store.StartSyncRun(ctx, source, start)
		if err != nil {
			out = append(out, SourceResult{Source: source, Err: err})
			return
		}
		t := time.Now()
		n, err := fn()
		if ferr := s.Store.FinishSyncRun(context.WithoutCancel(ctx), id, s.now(), n, err); ferr != nil {
			s.Log.Error("sync run not recorded", "source", source, "err", ferr)
		}
		out = append(out, SourceResult{Source: source, Items: n, Duration: time.Since(t), Err: err})
	}
	// Merged mode: the viewer list is Tautulli's, Plex ids are translated.
	var v *viewers
	if up.Merged() && up.HistoryClient() != nil && up.Plex != nil {
		users, err := up.HistoryClient().Users(ctx)
		var accs []plex.Account
		if err == nil {
			accs, err = up.Plex.Accounts(ctx)
		}
		if err != nil {
			return []SourceResult{{Source: SourceTautulli, Err: fmt.Errorf("viewers: %w", err)}}
		}
		v = newViewers(accs, users)
	}
	if c := up.HistoryClient(); c != nil {
		step(SourceTautulli, func() (int, error) {
			if !up.Merged() {
				if err := s.keepHistory(ctx, SourceTautulli, cursorPlexHistory); err != nil {
					return 0, err
				}
				users, err := c.Users(ctx)
				if err != nil {
					return 0, err
				}
				return s.syncTautulliHistory(ctx, c, users, nil)
			}
			return s.syncTautulliHistory(ctx, c, nil, v)
		})
	}
	if up.PlexHistory() {
		step(SourcePlexHistory, func() (int, error) {
			if !up.Merged() {
				if err := s.keepHistory(ctx, SourcePlexHistory, cursorTautulli); err != nil {
					return 0, err
				}
			}
			return s.syncPlexHistory(ctx, up.Plex, v)
		})
	}
	return out
}

// mergeHistory merges the Plex history into Tautulli's (merged mode), once
// the events are linked to media.
func (s *Syncer) mergeHistory(ctx context.Context, up *upstream.Clients) error {
	if !up.Merged() {
		return nil
	}
	st, err := s.Store.MergePlexHistory(ctx)
	if err != nil {
		return err
	}
	s.Log.Info("history merged", "plex_entries_merged", st.Merged, "sessions_completed_by_plex", st.Completed,
		"plex_only_plays_estimated", st.Estimated)
	return nil
}

// keepHistory makes source the only history in the database: the events of
// the other source are deleted (after switching from Plex history, which
// has no durations, to Tautulli, both would otherwise count every play).
func (s *Syncer) keepHistory(ctx context.Context, source, otherCursor string) error {
	n, err := s.Store.DropOtherHistory(ctx, source, otherCursor)
	if err != nil {
		return err
	}
	if n > 0 {
		s.Log.Info("history from the previous source dropped", "kept", source, "deleted_events", n)
	}
	return nil
}

// PlexEvent converts a Plex history entry (Plex only records watched items).
func PlexEvent(h plex.HistoryEntry) (store.WatchEvent, bool) {
	if h.ID() <= 0 || (h.Type != "movie" && h.Type != "episode") {
		return store.WatchEvent{}, false
	}
	e := store.WatchEvent{
		Source: SourcePlexHistory, SourceID: h.ID(), MediaType: h.Type, RatingKey: string(h.RatingKey),
		ParentRatingKey: plex.KeyID(h.ParentKey), GrandparentRatingKey: plex.KeyID(h.GrandparentKey),
		UserID: int64(h.AccountID), StartedAt: int64(h.ViewedAt), StoppedAt: int64(h.ViewedAt),
		PercentComplete: 100, Watched: true, Title: h.Title, GrandparentTitle: h.GrandparentTitle, Year: int(h.Year),
	}
	if h.Type == "episode" {
		e.SeasonNumber, e.EpisodeNumber = intPtr(int(h.ParentIndex)), intPtr(int(h.Index))
	}
	return e, true
}

// syncPlexHistory reads the Plex history. In merged mode (v set) the
// accounts are already stored and the Plex ids are translated to the stored
// ones.
func (s *Syncer) syncPlexHistory(ctx context.Context, c *plex.Client, v *viewers) (int, error) {
	ids := map[int64]int64{}
	if v != nil {
		ids = v.ids
	} else {
		accs, err := c.Accounts(ctx)
		if err != nil {
			return 0, err
		}
		accounts := make([]store.PlexAccount, 0, len(accs))
		for _, a := range accs {
			if a.ID > 0 && a.Name != "" {
				accounts = append(accounts, store.PlexAccount{AccountID: int64(a.ID), Username: a.Name, IsActive: true})
			}
		}
		if err := s.Store.UpsertPlexAccounts(ctx, accounts); err != nil {
			return 0, err
		}
	}
	// Entries stored before the translation existed (or before Tautulli).
	if err := s.Store.RemapHistoryUsers(ctx, SourcePlexHistory, ids); err != nil {
		return 0, err
	}
	return s.pageHistory(ctx, cursorPlexHistory, func(start int) ([]store.WatchEvent, []int64, int, int, error) {
		entries, total, err := c.History(ctx, start)
		if err != nil {
			return nil, nil, 0, 0, err
		}
		var events []store.WatchEvent
		dates := make([]int64, 0, len(entries))
		for _, h := range entries {
			dates = append(dates, int64(h.ViewedAt))
			if e, ok := PlexEvent(h); ok {
				tid, ok := ids[e.UserID]
				if ok {
					e.UserID = tid
				} else if v != nil {
					continue // a Plex account Tautulli does not know
				}
				events = append(events, e)
			}
		}
		return events, dates, len(entries), total, nil
	}, plex.HistoryPageSize)
}

// pageFunc fetches a page: events to store, the date of every row, the raw
// row count and the total reported by the server (0 if unknown).
type pageFunc func(start int) (events []store.WatchEvent, dates []int64, rows, total int, err error)

func (s *Syncer) pageHistory(ctx context.Context, cursorName string, fetch pageFunc, pageSize int) (int, error) {
	raw, err := s.Store.Cursor(ctx, cursorName)
	if err != nil {
		return 0, err
	}
	cursor, _ := strconv.ParseInt(raw, 10, 64)
	threshold := cursor - historyOverlapSecs
	maxDate := cursor
	stored, start := 0, 0
	for {
		events, dates, rows, total, err := fetch(start)
		if err != nil {
			return stored, err
		}
		if err := s.Store.UpsertWatchEvents(ctx, events); err != nil {
			return stored, err
		}
		stored += len(events)
		allOld := len(dates) > 0
		for _, d := range dates {
			maxDate = max(maxDate, d)
			if d >= threshold {
				allOld = false
			}
		}
		start += rows
		if cursor == 0 {
			if total > 0 {
				s.setProgress("history: %d / %d", start, total)
			} else {
				s.setProgress("history: %d", start)
			}
		}
		if (cursor > 0 && allOld) || rows < pageSize || (total > 0 && start >= total) {
			break
		}
	}
	if maxDate > cursor {
		if err := s.Store.SetCursor(ctx, cursorName, strconv.FormatInt(maxDate, 10), s.now()); err != nil {
			return stored, err
		}
	}
	return stored, nil
}

// linkEvents resolves media for every event: Plex rating keys first, then
// normalized title (+ year) for keys that no longer exist.
func (s *Syncer) linkEvents(ctx context.Context) error {
	if err := s.Store.LinkWatchEventsByKey(ctx); err != nil {
		return err
	}
	titles, err := s.Store.UnlinkedEventTitles(ctx)
	if err != nil || len(titles) == 0 {
		return err
	}
	media, err := s.Store.ListMediaTitles(ctx)
	if err != nil {
		return err
	}
	cands := make([]matching.TitleCandidate, 0, len(media))
	for _, m := range media {
		cands = append(cands, matching.TitleCandidate{MediaID: m.ID, Kind: m.Kind, Title: m.Title, Year: m.Year})
	}
	idx := matching.NewTitleIndex(cands)
	var links []store.TitleLink
	for _, t := range titles {
		var id int64
		if t.MediaType == "episode" {
			// Tautulli's year on episode rows is not reliably the show's year:
			// try the title alone first, the year only breaks ties.
			if id = idx.Find(matching.KindSeries, t.Title, 0); id == 0 {
				id = idx.Find(matching.KindSeries, t.Title, t.Year)
			}
		} else {
			id = idx.Find(matching.KindMovie, t.Title, t.Year)
		}
		if id != 0 {
			links = append(links, store.TitleLink{UnlinkedTitle: t, MediaID: id})
		}
	}
	return s.Store.LinkWatchEventsByTitle(ctx, links)
}

func unixPtr(v *int64) *time.Time {
	if v == nil {
		return nil
	}
	t := time.Unix(*v, 0)
	return &t
}

// recompute refreshes play counts and watch_state.
func (s *Syncer) recompute(ctx context.Context) error {
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
