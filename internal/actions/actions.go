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

	"arrstats/internal/clients/httpx"
	"arrstats/internal/clients/plex"
	"arrstats/internal/clients/radarr"
	"arrstats/internal/clients/sonarr"
	"arrstats/internal/store"
)

// Action names stored in action_log.
const (
	DeleteSeries = "delete_series"
	DeleteSeason = "delete_season"
	DeleteMovie  = "delete_movie"
)

// Errors shown to the user.
var (
	ErrNotFound        = errors.New("élément introuvable")
	ErrNotDeletable    = errors.New("élément non géré par Sonarr/Radarr : suppression impossible")
	ErrProtected       = errors.New("élément protégé par un tag")
	ErrConfirmMismatch = errors.New("le titre saisi ne correspond pas")
	ErrNoSuchSeason    = errors.New("saison introuvable")
)

// Refresher updates local data after a deletion (implemented by the syncer).
type Refresher interface {
	RefreshSeries(ctx context.Context, arrID int64) error
	Recompute(ctx context.Context) error
}

// Service deletes media.
type Service struct {
	Store           *store.Store
	Sonarr          *sonarr.Client
	Radarr          *radarr.Client
	Plex            *plex.Client // nil disables the Plex refresh
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
func (s *Service) liveTags(ctx context.Context, kind string, arrID int64) ([]string, error) {
	var ids []int64
	var tags []sonarr.Tag
	var err error
	if kind == "series" {
		se, _, gerr := s.Sonarr.SeriesByID(ctx, arrID)
		if gerr != nil {
			return nil, gerr
		}
		ids = se.Tags
		tags, err = s.Sonarr.Tags(ctx)
	} else {
		m, gerr := s.Radarr.MovieByID(ctx, arrID)
		if gerr != nil {
			return nil, gerr
		}
		ids = m.Tags
		tags, err = s.Radarr.Tags(ctx)
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
		if s.ConfirmByTyping && !strings.EqualFold(strings.TrimSpace(req.ConfirmTitle), strings.TrimSpace(m.Title)) {
			return nil, ErrConfirmMismatch
		}
	}
	if !req.DeleteFiles {
		res.SizeFreed = 0
	}

	err = s.execute(ctx, m, req, res)
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
		s.afterDelete(context.WithoutCancel(ctx), m, req, res)
	}
	return res, nil
}

// execute checks the live protection tags, then deletes unless dry-run.
func (s *Service) execute(ctx context.Context, m *store.Media, req Request, res *Result) error {
	labels, err := s.liveTags(ctx, m.Kind, m.ArrID)
	if isNotFound(err) {
		return fmt.Errorf("%w dans %s", ErrNotFound, arrName(m.Kind))
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
		return s.Radarr.DeleteMovie(ctx, m.ArrID, req.DeleteFiles, req.ImportExclusion)
	case DeleteSeries:
		return s.Sonarr.DeleteSeries(ctx, m.ArrID, req.DeleteFiles, req.ImportExclusion)
	default:
		return s.deleteSeason(ctx, m.ArrID, *req.Season, req.DeleteFiles)
	}
}

// deleteSeason deletes the season's files, THEN unmonitors the season, or
// Sonarr would download it again (SPEC §3.1).
func (s *Service) deleteSeason(ctx context.Context, seriesID int64, season int, deleteFiles bool) error {
	if deleteFiles {
		files, err := s.Sonarr.EpisodeFiles(ctx, seriesID)
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
			if err := s.Sonarr.DeleteEpisodeFiles(ctx, ids); err != nil {
				return err
			}
		}
	}
	// Re-read the series after the deletion so the PUT carries fresh data.
	_, raw, err := s.Sonarr.SeriesByID(ctx, seriesID)
	if err != nil {
		return fmt.Errorf("fichiers supprimés mais saison non démonitorée : %w", err)
	}
	updated, ok, err := sonarr.UnmonitorSeason(raw, season)
	if err != nil || !ok {
		return fmt.Errorf("fichiers supprimés mais saison non démonitorée : %w", errors.Join(err, ErrNoSuchSeason))
	}
	if err := s.Sonarr.UpdateSeriesRaw(ctx, seriesID, updated); err != nil {
		return fmt.Errorf("fichiers supprimés mais saison non démonitorée : %w", err)
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
func (s *Service) afterDelete(ctx context.Context, m *store.Media, req Request, res *Result) {
	var err error
	if res.Action == DeleteSeason {
		err = s.Refresher.RefreshSeries(ctx, m.ArrID)
	} else if err = s.Store.DeleteMedia(ctx, m.ID); err == nil {
		err = s.Refresher.Recompute(ctx)
	}
	if err != nil {
		s.Log.Warn("local refresh after deletion failed", "media_id", m.ID, "err", err)
	}
	if s.PlexRefresh && s.Plex != nil && req.DeleteFiles {
		s.refreshPlex(ctx, m.Kind)
	}
}

// refreshPlex rescans the Plex sections of the media's type, then empties
// their trash. The scan is asynchronous: if the trash is emptied before it
// ends, Plex's "empty trash after every scan" setting finishes the job.
func (s *Service) refreshPlex(ctx context.Context, kind string) {
	sections, err := s.Plex.Sections(ctx)
	if err != nil {
		s.Log.Warn("plex refresh failed", "err", err)
		return
	}
	want := map[string]string{"movie": "movie", "series": "show"}[kind]
	for _, sec := range sections {
		if sec.Type != want {
			continue
		}
		if err := s.Plex.RefreshSection(ctx, string(sec.Key)); err != nil {
			s.Log.Warn("plex refresh failed", "section", sec.Key, "err", err)
			continue
		}
		if err := s.Plex.EmptyTrash(ctx, string(sec.Key)); err != nil {
			s.Log.Warn("plex empty trash failed", "section", sec.Key, "err", err)
		}
	}
}
