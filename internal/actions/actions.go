// Package actions performs deletions through Sonarr/Radarr with
// server-side safeguards: dry-run, protection tags checked on live *arr
// data, typed confirmation for whole series, and a journal entry for every
// attempt. Items that only the media server (Plex or Jellyfin) still knows
// are deleted through it.
package actions

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"hangarr/internal/clients/httpx"
	"hangarr/internal/clients/plex"
	"hangarr/internal/clients/sonarr"
	"hangarr/internal/store"
	"hangarr/internal/upstream"
)

// Action names stored in action_log.
const (
	DeleteSeries   = "delete_series"
	DeleteSeason   = "delete_season"
	DeleteMovie    = "delete_movie"
	DeletePlex     = "delete_plex"     // movie or series known by Plex only
	DeleteJellyfin = "delete_jellyfin" // movie or series known by Jellyfin only
)

// Errors explained to the user (the web layer translates them).
var (
	ErrNotFound        = errors.New("item not found")
	ErrUnconfigured    = errors.New("service not configured")
	ErrNotDeletable    = errors.New("not managed by Sonarr/Radarr: cannot be deleted")
	ErrProtected       = errors.New("protected by a tag")
	ErrProtectedLocal  = errors.New("protected in Hangarr")
	ErrConfirmMismatch = errors.New("the typed title does not match")
	ErrNoSuchSeason    = errors.New("season not found")
	// ErrUnmonitorAfterDelete: the season files were deleted but the season
	// could not be unmonitored (Sonarr may download it again).
	ErrUnmonitorAfterDelete = errors.New("files deleted but the season was not unmonitored")
	// ErrPlexDeletionDenied: the Plex server does not allow media deletion
	// (Settings > Library > Allow media deletion).
	ErrPlexDeletionDenied = errors.New("deletion refused by Plex: media deletion is not allowed on the server")
	// ErrJellyfinDeletionDenied: the Jellyfin API key may not delete media.
	ErrJellyfinDeletionDenied = errors.New("deletion refused by Jellyfin")
	// ErrInactiveServer: only the media server not in use knows the item
	// (hidden until that server is used again).
	ErrInactiveServer = errors.New("known by the media server not in use")
	// ErrServerConflict: Plex and Jellyfin are both configured, so which
	// one owns the item is not settled; nothing is done through either.
	ErrServerConflict = errors.New("both media servers are configured")
	// ErrArrCatalogUnhealthy: the last Sonarr/Radarr catalog sync failed or
	// was refused, so an item they manage (with its protection tags) may
	// look like one only the media server knows: nothing is deleted through
	// the media server until it succeeds.
	ErrArrCatalogUnhealthy = errors.New("the last Sonarr/Radarr catalog sync failed")
	// ErrStopping: Hangarr is shutting down; nothing more is sent.
	ErrStopping = errors.New("Hangarr is stopping")
)

// serverKey returns the item's key on the media server in use ("" when that
// server does not know it).
func serverKey(up *upstream.Clients, m *store.Media) string {
	if up.MediaServer == upstream.Jellyfin {
		return m.JellyfinID
	}
	return m.PlexRatingKey
}

// serverOnly checks that an item Sonarr/Radarr does not manage is known by
// the media server in use: the items of the other one are hidden, nothing is
// done to them, nor to any while both servers are configured.
func serverOnly(up *upstream.Clients, m *store.Media, notManaged error) error {
	switch {
	case up.ServerConflict:
		return ErrServerConflict
	case m.MatchState == "hidden" || (serverKey(up, m) == "" && (m.PlexRatingKey != "" || m.JellyfinID != "")):
		return ErrInactiveServer
	case serverKey(up, m) == "":
		return notManaged
	}
	return nil
}

// Refresher updates local data after a deletion (implemented by the syncer).
type Refresher interface {
	RefreshSeries(ctx context.Context, arrID int64) error
	Recompute(ctx context.Context) error
	// BeginDeletion is held while a deletion runs: the sync does not write
	// the catalogs it read meanwhile, and skips what was deleted since it
	// started reading them.
	BeginDeletion() (end func())
}

// Service deletes media.
type Service struct {
	Store           *store.Store
	Upstreams       *upstream.Registry
	Refresher       Refresher
	DryRun          bool
	ProtectedTags   []string
	ConfirmByTyping bool
	// PlexRefresh (PLEX_REFRESH_AFTER_DELETE) rescans the media server in
	// use, Plex or Jellyfin, after a deletion of files.
	PlexRefresh bool
	Log         *slog.Logger
	Now         func() time.Time
	// GoneChecks overrides DefaultGoneChecks (tests).
	GoneChecks []time.Duration

	versionMu sync.Mutex // one version deletion at a time (DeleteVersion)

	lifeMu   sync.Mutex
	stopping bool
	inflight sync.WaitGroup // actions running, awaited by Wait
}

// enter registers an action; false once Stop was called.
func (s *Service) enter() bool {
	s.lifeMu.Lock()
	defer s.lifeMu.Unlock()
	if s.stopping {
		return false
	}
	s.inflight.Add(1)
	return true
}

func (s *Service) leave() { s.inflight.Done() }

// Stop refuses new actions; a batch stops before its next item.
func (s *Service) Stop() {
	s.lifeMu.Lock()
	s.stopping = true
	s.lifeMu.Unlock()
}

// Stopping reports whether Stop was called.
func (s *Service) Stopping() bool {
	s.lifeMu.Lock()
	defer s.lifeMu.Unlock()
	return s.stopping
}

// Wait stops the service and waits for the running actions (at most one
// upstream call each), so that they are journaled before the database closes.
func (s *Service) Wait() {
	s.Stop()
	s.inflight.Wait()
}

// Request describes a deletion.
type Request struct {
	MediaID         int64
	Season          *int // set for a season deletion (series only)
	DeleteFiles     bool
	ImportExclusion bool
	ConfirmTitle    string // required for a whole series when ConfirmByTyping
	// Confirmed: the batch confirmation replaces ConfirmTitle.
	Confirmed bool
	// SkipServerRefresh: the caller refreshes the media server once (batch).
	SkipServerRefresh bool
	// SkipRecompute: the caller recomputes the statistics once (batch).
	SkipRecompute bool
	Username      string // app user, for the journal
}

// Result describes what was (or would have been) deleted.
type Result struct {
	Action    string
	Kind      string
	MediaID   int64
	Title     string
	Season    *int
	SizeFreed int64
	DryRun    bool
	// Warning: done, but not entirely (season files deleted, season still
	// monitored: ErrUnmonitorAfterDelete).
	Warning error
	// Note: how the outcome was established, when it is not the plain
	// answer of the upstream (no answer in time, the item checked gone).
	Note string
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func isNotFound(err error) bool { return httpx.IsNotFound(err) }

// NoteConfirmedGone is the journal note of a deletion that got no answer in
// time but that a later check found done.
const NoteConfirmedGone = "no answer in time, but the item is gone: deleted"

// confirmGone checks, after a deletion that ended without an answer (timeout,
// connection cut), whether the item is gone anyway: the upstream may finish
// a slow deletion after Hangarr stopped waiting. It returns nil and a note
// when gone is confirmed, err otherwise.
func (s *Service) confirmGone(ctx context.Context, err error, gone func(context.Context) (bool, error)) (string, error) {
	if !httpx.Ambiguous(err) {
		return "", err
	}
	ctx = context.WithoutCancel(ctx)
	delays := s.GoneChecks
	if delays == nil {
		delays = DefaultGoneChecks
	}
	var cerr error
	for _, d := range delays {
		time.Sleep(d)
		var ok bool
		if ok, cerr = gone(ctx); ok {
			s.Log.Warn("deletion without answer, but the item is gone", "err", err)
			return NoteConfirmedGone, nil
		}
	}
	s.Log.Warn("deletion without answer, item still there or unknown", "err", err, "check_err", cerr)
	return "", err
}

// DefaultGoneChecks are the delays before each check of confirmGone: the
// upstream may still be deleting when Hangarr stops waiting.
var DefaultGoneChecks = []time.Duration{0, 2 * time.Second, 10 * time.Second}

// goneIf404 is a confirmGone check: a 404 means gone.
func goneIf404(get func(context.Context) error) func(context.Context) (bool, error) {
	return func(ctx context.Context) (bool, error) {
		err := get(ctx)
		if isNotFound(err) {
			return true, nil
		}
		return false, err
	}
}

func (s *Service) protected(labels []string) bool {
	return store.HasProtectedTag(labels, s.ProtectedTags)
}

// liveTags reads the item's current tags from Sonarr/Radarr; the local copy
// may be a day old and a tag added since must still protect the item.
func (s *Service) liveTags(ctx context.Context, up *upstream.Clients, kind string, arrID int64) ([]string, error) {
	var ids []int64
	var tags []sonarr.Tag
	var err error
	if kind == "series" {
		se, _, gerr := up.Sonarr.SeriesByID(ctx, arrID)
		if gerr != nil {
			return nil, gerr
		}
		ids = se.Tags
		tags, err = up.Sonarr.Tags(ctx)
	} else {
		m, gerr := up.Radarr.MovieByID(ctx, arrID)
		if gerr != nil {
			return nil, gerr
		}
		ids = m.Tags
		tags, err = up.Radarr.Tags(ctx)
	}
	if err != nil {
		return nil, err
	}
	byID := map[int64]string{}
	for _, t := range tags {
		byID[t.ID] = t.Label
	}
	var out []string
	for _, id := range ids {
		out = append(out, byID[id])
	}
	return out, nil
}

// Delete performs (or simulates) a deletion. Every attempt that reaches
// Sonarr/Radarr is journaled, successful or not.
func (s *Service) Delete(ctx context.Context, req Request) (*Result, error) {
	if !s.enter() {
		return nil, ErrStopping
	}
	defer s.leave()
	up := s.Upstreams.Get()
	m, err := s.Store.MediaByID(ctx, req.MediaID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if m.Kind == "movie" && req.Season != nil {
		// A movie has no season: never fall through to a whole-movie deletion.
		return nil, ErrNoSuchSeason
	}
	if m.ArrID == 0 {
		if err := serverOnly(up, m, ErrNotDeletable); err != nil {
			return nil, err
		}
		if req.Season != nil {
			return nil, ErrNotDeletable
		}
		return s.deleteFromServer(ctx, up, m, req)
	}
	res := &Result{Kind: m.Kind, MediaID: m.ID, Title: m.Title, SizeFreed: m.SizeBytes, DryRun: s.DryRun}
	switch {
	case m.Kind == "movie":
		res.Action = DeleteMovie
	case req.Season != nil:
		res.Action, res.Season = DeleteSeason, req.Season
		req.ImportExclusion = false // not applicable to a season
		seasons, err := s.Store.Seasons(ctx, m.ID)
		if err != nil {
			return nil, err
		}
		found := false
		for _, se := range seasons {
			if se.Number == *req.Season {
				res.SizeFreed, found = se.SizeBytes, true
			}
		}
		if !found {
			return nil, ErrNoSuchSeason
		}
	default:
		res.Action = DeleteSeries
		if s.ConfirmByTyping && !req.Confirmed && !strings.EqualFold(strings.TrimSpace(req.ConfirmTitle), strings.TrimSpace(m.Title)) {
			return nil, ErrConfirmMismatch
		}
	}
	if !req.DeleteFiles {
		res.SizeFreed = 0
	}

	end := s.Refresher.BeginDeletion()
	defer end()
	id, err := s.journalBegin(ctx, m, req, res, "")
	if err != nil {
		return nil, err // nothing sent: a deletion is never made without its journal entry
	}
	err = s.execute(ctx, up, m, req, res)
	s.journalEnd(ctx, id, m, req, res, err)
	if err != nil {
		return nil, err
	}
	if !s.DryRun {
		s.afterDelete(context.WithoutCancel(ctx), up, m, req, res)
	}
	return res, nil
}

// journalBegin writes a deletion attempt to action_log (pending) before
// anything is sent; serverKey names the item on the media server.
func (s *Service) journalBegin(ctx context.Context, m *store.Media, req Request, res *Result, serverKey string) (int64, error) {
	id, err := s.Store.BeginAction(context.WithoutCancel(ctx), store.Action{
		At: s.now().Unix(), Action: res.Action, MediaKind: m.Kind, ArrID: m.ArrID, Title: m.Title,
		SeasonNumber: req.Season, DeleteFiles: req.DeleteFiles, ImportExclusion: req.ImportExclusion,
		DryRun: s.DryRun, Username: req.Username, ServerKey: serverKey,
	})
	if err != nil {
		s.Log.Error("action not journaled: nothing sent", "title", m.Title, "err", err)
		return 0, fmt.Errorf("journal: %w", err)
	}
	return id, nil
}

// journalEnd records the outcome of a deletion attempt in action_log and in
// the logs.
func (s *Service) journalEnd(ctx context.Context, id int64, m *store.Media, req Request, res *Result, err error) {
	size, msg := res.SizeFreed, res.Note
	if res.Warning != nil {
		msg = strings.TrimSpace(res.Warning.Error() + ". " + msg)
	}
	if err != nil {
		size, msg = 0, err.Error()
	}
	if lerr := s.Store.FinishAction(context.WithoutCancel(ctx), id, err == nil, size, msg); lerr != nil {
		s.Log.Error("action outcome not journaled", "err", lerr)
	}
	s.Log.Info("deletion", "action", res.Action, "title", m.Title, "season", req.Season, "dry_run", s.DryRun,
		"delete_files", req.DeleteFiles, "success", err == nil, "err", err, "warning", res.Warning, "note", res.Note,
		"user", req.Username)
}

// deleteFromServer deletes an item that only the media server in use still
// knows (removed from Sonarr/Radarr, or never managed): the server deletes it
// with its files. No Sonarr/Radarr tag applies, but the item may be
// protected in Hangarr. Whole series still need the typed title.
func (s *Service) deleteFromServer(ctx context.Context, up *upstream.Clients, m *store.Media, req Request) (*Result, error) {
	if m.Kind == "series" && s.ConfirmByTyping && !req.Confirmed &&
		!strings.EqualFold(strings.TrimSpace(req.ConfirmTitle), strings.TrimSpace(m.Title)) {
		return nil, ErrConfirmMismatch
	}
	if (m.Kind == "movie" && up.Radarr != nil) || (m.Kind == "series" && up.Sonarr != nil) {
		healthy, err := s.Store.ArrCatalogHealthy(ctx, m.Kind)
		if err != nil {
			return nil, err
		}
		if !healthy {
			return nil, fmt.Errorf("%w (%s)", ErrArrCatalogUnhealthy, arrName(m.Kind))
		}
	}
	req.DeleteFiles, req.ImportExclusion = true, false // the server always deletes the files
	res := &Result{Action: DeletePlex, Kind: m.Kind, MediaID: m.ID, Title: m.Title, SizeFreed: m.SizeBytes, DryRun: s.DryRun}
	denied, key := ErrPlexDeletionDenied, m.PlexRatingKey
	configured := up.Plex != nil
	del := func(ctx context.Context) error { return up.Plex.DeleteItem(ctx, m.PlexRatingKey) }
	gone := goneIf404(func(ctx context.Context) error { _, err := up.Plex.Metadata(ctx, m.PlexRatingKey); return err })
	if up.MediaServer == upstream.Jellyfin {
		res.Action, denied, configured, key = DeleteJellyfin, ErrJellyfinDeletionDenied, up.Jellyfin != nil, m.JellyfinID
		del = func(ctx context.Context) error { return up.Jellyfin.DeleteItem(ctx, m.JellyfinID) }
		gone = func(ctx context.Context) (bool, error) {
			items, err := up.Jellyfin.Versions(ctx, []string{m.JellyfinID})
			return err == nil && len(items) == 0, err
		}
	}
	end := s.Refresher.BeginDeletion()
	defer end()
	id, err := s.journalBegin(ctx, m, req, res, key)
	if err != nil {
		return nil, err
	}
	switch {
	case m.ProtectedLocal:
		err = ErrProtectedLocal
	case !configured:
		err = fmt.Errorf("%w: %s", ErrUnconfigured, up.ServerLabel())
	case !s.DryRun:
		err = del(ctx)
		var se *httpx.StatusError
		if errors.As(err, &se) && (se.Status == http.StatusUnauthorized || se.Status == http.StatusForbidden) {
			err = denied
		} else if isNotFound(err) {
			err = fmt.Errorf("%w in %s", ErrNotFound, up.ServerLabel())
		} else {
			res.Note, err = s.confirmGone(ctx, err, gone)
		}
	}
	s.journalEnd(ctx, id, m, req, res, err)
	if err != nil {
		return nil, err
	}
	if !s.DryRun {
		ctx = context.WithoutCancel(ctx)
		err := s.Store.DeleteMedia(ctx, m.ID)
		if err == nil && !req.SkipRecompute {
			err = s.Refresher.Recompute(ctx)
		}
		if err != nil {
			s.Log.Warn("local refresh after deletion failed", "media_id", m.ID, "err", err)
		}
	}
	return res, nil
}

// execute checks the local protection and the live protection tags, then
// deletes unless dry-run.
func (s *Service) execute(ctx context.Context, up *upstream.Clients, m *store.Media, req Request, res *Result) error {
	if m.ProtectedLocal {
		// Protected in Hangarr before Sonarr/Radarr took the item over.
		return ErrProtectedLocal
	}
	if (m.Kind == "movie" && up.Radarr == nil) || (m.Kind != "movie" && up.Sonarr == nil) {
		return fmt.Errorf("%w: %s", ErrUnconfigured, arrName(m.Kind))
	}
	labels, err := s.liveTags(ctx, up, m.Kind, m.ArrID)
	if isNotFound(err) {
		return fmt.Errorf("%w in %s", ErrNotFound, arrName(m.Kind))
	}
	if err != nil {
		return err
	}
	if s.protected(labels) {
		return ErrProtected
	}
	if s.DryRun {
		return nil
	}
	switch res.Action {
	case DeleteMovie:
		err = up.Radarr.DeleteMovie(ctx, m.ArrID, req.DeleteFiles, req.ImportExclusion)
		res.Note, err = s.confirmGone(ctx, err, goneIf404(func(ctx context.Context) error {
			_, err := up.Radarr.MovieByID(ctx, m.ArrID)
			return err
		}))
	case DeleteSeries:
		err = up.Sonarr.DeleteSeries(ctx, m.ArrID, req.DeleteFiles, req.ImportExclusion)
		res.Note, err = s.confirmGone(ctx, err, goneIf404(func(ctx context.Context) error {
			_, _, err := up.Sonarr.SeriesByID(ctx, m.ArrID)
			return err
		}))
	default:
		err = s.deleteSeason(ctx, up.Sonarr, m.ArrID, *req.Season, req.DeleteFiles, res)
	}
	return err
}

// deleteSeason deletes the season's files, THEN unmonitors the season, or
// Sonarr would download it again. Files deleted but the season left
// monitored is a partial success: res.Warning is ErrUnmonitorAfterDelete.
func (s *Service) deleteSeason(ctx context.Context, c *sonarr.Client, seriesID int64, season int, deleteFiles bool, res *Result) error {
	deleted := false
	if deleteFiles {
		files, err := c.EpisodeFiles(ctx, seriesID)
		if err != nil {
			return err
		}
		var ids []int64
		for _, f := range files {
			if f.SeasonNumber == season {
				ids = append(ids, f.ID)
			}
		}
		if len(ids) > 0 {
			err := c.DeleteEpisodeFiles(ctx, ids)
			res.Note, err = s.confirmGone(ctx, err, func(ctx context.Context) (bool, error) {
				left, err := c.EpisodeFiles(ctx, seriesID)
				if err != nil {
					return false, err
				}
				for _, f := range left {
					if slices.Contains(ids, f.ID) {
						return false, nil
					}
				}
				return true, nil
			})
			if err != nil {
				return err
			}
			deleted = true
		}
	}
	unmonitored := func() error {
		// Re-read the series after the deletion so the PUT carries fresh data.
		_, raw, err := c.SeriesByID(ctx, seriesID)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrUnmonitorAfterDelete, err)
		}
		updated, ok, err := sonarr.UnmonitorSeason(raw, season)
		if err != nil || !ok {
			return fmt.Errorf("%w: %w", ErrUnmonitorAfterDelete, errors.Join(err, ErrNoSuchSeason))
		}
		if err := c.UpdateSeriesRaw(ctx, seriesID, updated); err != nil {
			return fmt.Errorf("%w: %w", ErrUnmonitorAfterDelete, err)
		}
		return nil
	}()
	if unmonitored != nil && deleted {
		res.Warning = unmonitored // the files are gone: report them freed
		return nil
	}
	return unmonitored
}

func arrName(kind string) string {
	if kind == "movie" {
		return "Radarr"
	}
	return "Sonarr"
}

// afterDelete updates local data and optionally asks the media server to
// rescan. Failures are logged: the deletion itself succeeded.
func (s *Service) afterDelete(ctx context.Context, up *upstream.Clients, m *store.Media, req Request, res *Result) {
	var err error
	if res.Action == DeleteSeason {
		err = s.Refresher.RefreshSeries(ctx, m.ArrID)
	} else if err = s.Store.DeleteMedia(ctx, m.ID); err == nil && !req.SkipRecompute {
		err = s.Refresher.Recompute(ctx)
	}
	if err != nil {
		s.Log.Warn("local refresh after deletion failed", "media_id", m.ID, "err", err)
	}
	if s.PlexRefresh && req.DeleteFiles && !req.SkipServerRefresh {
		s.refreshServer(ctx, up, []string{m.Kind})
	}
}

// refreshServer rescans the media server in use after a deletion of files of
// the given kinds: the Plex sections of those kinds, or every Jellyfin
// library (Jellyfin scans them all at once).
func (s *Service) refreshServer(ctx context.Context, up *upstream.Clients, kinds []string) {
	switch {
	case up.MediaServer == upstream.Jellyfin && up.Jellyfin != nil:
		if err := up.Jellyfin.RefreshLibrary(ctx); err != nil {
			s.Log.Warn("jellyfin refresh failed", "err", err)
		}
	case up.MediaServer == upstream.Plex && up.Plex != nil:
		for _, kind := range kinds {
			s.refreshPlex(ctx, up.Plex, kind)
		}
	}
}

// refreshPlex rescans the Plex sections of the media's type, then empties
// their trash. The scan is asynchronous: if the trash is emptied before it
// ends, Plex's "empty trash after every scan" setting finishes the job.
func (s *Service) refreshPlex(ctx context.Context, p *plex.Client, kind string) {
	sections, err := p.Sections(ctx)
	if err != nil {
		s.Log.Warn("plex refresh failed", "err", err)
		return
	}
	want := map[string]string{"movie": "movie", "series": "show"}[kind]
	for _, sec := range sections {
		if sec.Type != want {
			continue
		}
		if err := p.RefreshSection(ctx, string(sec.Key)); err != nil {
			s.Log.Warn("plex refresh failed", "section", sec.Key, "err", err)
			continue
		}
		if err := p.EmptyTrash(ctx, string(sec.Key)); err != nil {
			s.Log.Warn("plex empty trash failed", "section", sec.Key, "err", err)
		}
	}
}
