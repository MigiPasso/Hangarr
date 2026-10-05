// Package actions performs deletions through Sonarr/Radarr (SPEC §7.4) with
// server-side safeguards: dry-run, protection tags checked on live *arr
// data, typed confirmation for whole series, and a journal entry for every
// attempt.
package actions

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"cleanarr/internal/clients/httpx"
	"cleanarr/internal/clients/plex"
	"cleanarr/internal/clients/sonarr"
	"cleanarr/internal/store"
	"cleanarr/internal/upstream"
)

// Action names stored in action_log.
const (
	DeleteSeries = "delete_series"
	DeleteSeason = "delete_season"
	DeleteMovie  = "delete_movie"
)

// Errors explained to the user (the web layer translates them).
var (
	ErrNotFound        = errors.New("item not found")
	ErrUnconfigured    = errors.New("service not configured")
	ErrNotDeletable    = errors.New("not managed by Sonarr/Radarr: cannot be deleted")
	ErrProtected       = errors.New("protected by a tag")
	ErrConfirmMismatch = errors.New("the typed title does not match")
	ErrNoSuchSeason    = errors.New("season not found")
	// ErrUnmonitorAfterDelete: the season files were deleted but the season
	// could not be unmonitored (Sonarr may download it again).
	ErrUnmonitorAfterDelete = errors.New("files deleted but the season was not unmonitored")
)

// Refresher updates local data after a deletion (implemented by the syncer).
type Refresher interface {
	RefreshSeries(ctx context.Context, arrID int64) error
	Recompute(ctx context.Context) error
}

// Service deletes media.
type Service struct {
	Store           *store.Store
	Upstreams       *upstream.Registry // a nil Plex client disables the Plex refresh
	Refresher       Refresher
	DryRun          bool
	ProtectedTags   []string
	ConfirmByTyping bool
	PlexRefresh     bool
	Log             *slog.Logger
	Now             func() time.Time
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
	// SkipPlexRefresh: the caller refreshes Plex once (batch).
	SkipPlexRefresh bool
	Username        string // app user, for the journal
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
	for _, l := range labels {
		for _, p := range s.ProtectedTags {
			if strings.EqualFold(strings.TrimSpace(l), strings.TrimSpace(p)) {
				return true
			}
		}
	}
	return false
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
		return nil, ErrNotDeletable
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
	if err != nil {
		return nil, err
	}
	if !s.DryRun {
		s.afterDelete(context.WithoutCancel(ctx), up, m, req, res)
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
// Sonarr would download it again (SPEC §3.1).
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

// afterDelete updates local data and optionally asks Plex to rescan.
// Failures are logged: the deletion itself succeeded.
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
	if s.PlexRefresh && up.Plex != nil && req.DeleteFiles && !req.SkipPlexRefresh {
		s.refreshPlex(ctx, up.Plex, m.Kind)
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
