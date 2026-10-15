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

	versionMu sync.Mutex // one version deletion at a time (DeleteVersion)
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
	Username          string // app user, for the journal
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
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func isNotFound(err error) bool {
	var se *httpx.StatusError
	return errors.As(err, &se) && se.Status == http.StatusNotFound
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
	up := s.Upstreams.Get()
	m, err := s.Store.MediaByID(ctx, req.MediaID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
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

	err = s.execute(ctx, up, m, req, res)
	s.journal(ctx, m, req, res, err)
	if err != nil {
		return nil, err
	}
	if !s.DryRun {
		s.afterDelete(context.WithoutCancel(ctx), up, m, req, res)
	}
	return res, nil
}

// journal records a deletion attempt in action_log and in the logs.
func (s *Service) journal(ctx context.Context, m *store.Media, req Request, res *Result, err error) {
	entry := store.Action{
		At: s.now().Unix(), Action: res.Action, MediaKind: m.Kind, ArrID: m.ArrID, Title: m.Title,
		SeasonNumber: req.Season, SizeFreedBytes: res.SizeFreed, DeleteFiles: req.DeleteFiles,
		ImportExclusion: req.ImportExclusion, DryRun: s.DryRun, Success: err == nil, Username: req.Username,
	}
	if err != nil {
		entry.SizeFreedBytes = 0
		entry.Error = err.Error()
	}
	if lerr := s.Store.AddAction(context.WithoutCancel(ctx), entry); lerr != nil {
		s.Log.Error("action not journaled", "err", lerr)
	}
	s.Log.Info("deletion", "action", res.Action, "title", m.Title, "season", req.Season, "dry_run", s.DryRun,
		"delete_files", req.DeleteFiles, "success", err == nil, "err", err, "user", req.Username)
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
	req.DeleteFiles, req.ImportExclusion = true, false // the server always deletes the files
	res := &Result{Action: DeletePlex, Kind: m.Kind, MediaID: m.ID, Title: m.Title, SizeFreed: m.SizeBytes, DryRun: s.DryRun}
	denied := ErrPlexDeletionDenied
	del := func() error { return up.Plex.DeleteItem(ctx, m.PlexRatingKey) }
	configured := up.Plex != nil
	if up.MediaServer == upstream.Jellyfin {
		res.Action, denied, configured = DeleteJellyfin, ErrJellyfinDeletionDenied, up.Jellyfin != nil
		del = func() error { return up.Jellyfin.DeleteItem(ctx, m.JellyfinID) }
	}
	var err error
	switch {
	case m.ProtectedLocal:
		err = ErrProtectedLocal
	case !configured:
		err = fmt.Errorf("%w: %s", ErrUnconfigured, up.ServerLabel())
	case !s.DryRun:
		err = del()
		var se *httpx.StatusError
		if errors.As(err, &se) && (se.Status == http.StatusUnauthorized || se.Status == http.StatusForbidden) {
			err = denied
		} else if isNotFound(err) {
			err = fmt.Errorf("%w in %s", ErrNotFound, up.ServerLabel())
		}
	}
	s.journal(ctx, m, req, res, err)
	if err != nil {
		return nil, err
	}
	if !s.DryRun {
		ctx = context.WithoutCancel(ctx)
		err := s.Store.DeleteMedia(ctx, m.ID)
		if err == nil {
			err = s.Refresher.Recompute(ctx)
		}
		if err != nil {
			s.Log.Warn("local refresh after deletion failed", "media_id", m.ID, "err", err)
		}
	}
	return res, nil
}

// execute checks the live protection tags, then deletes unless dry-run.
func (s *Service) execute(ctx context.Context, up *upstream.Clients, m *store.Media, req Request, res *Result) error {
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
		return up.Radarr.DeleteMovie(ctx, m.ArrID, req.DeleteFiles, req.ImportExclusion)
	case DeleteSeries:
		return up.Sonarr.DeleteSeries(ctx, m.ArrID, req.DeleteFiles, req.ImportExclusion)
	default:
		return s.deleteSeason(ctx, up.Sonarr, m.ArrID, *req.Season, req.DeleteFiles)
	}
}

// deleteSeason deletes the season's files, THEN unmonitors the season, or
// Sonarr would download it again.
func (s *Service) deleteSeason(ctx context.Context, c *sonarr.Client, seriesID int64, season int, deleteFiles bool) error {
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
			if err := c.DeleteEpisodeFiles(ctx, ids); err != nil {
				return err
			}
		}
	}
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
	} else if err = s.Store.DeleteMedia(ctx, m.ID); err == nil {
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
