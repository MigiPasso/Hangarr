package actions

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"
)

// MaxBatch caps the number of items of a batch action.
const MaxBatch = 100

// BatchItem is the outcome of one item of a batch action.
type BatchItem struct {
	MediaID   int64
	Kind      string
	Title     string
	SizeFreed int64
	OK        bool
	Unchanged bool   // protection: already protected
	Error     string // French, shown in the report
}

// BatchOptions are the deletion options applied to every item.
type BatchOptions struct {
	DeleteFiles     bool
	ImportExclusion bool
	Username        string
}

// DeleteBatch deletes items one after the other (SPEC §7.4): each goes
// through Delete (live protection check, dry-run, journal); the typed
// confirmation was done once for the whole batch. Plex is refreshed once at
// the end. A failure does not stop the others.
func (s *Service) DeleteBatch(ctx context.Context, ids []int64, opt BatchOptions) []BatchItem {
	// Finish the batch even if the client goes away: half-done batches are
	// harder to understand than a complete journal.
	ctx = context.WithoutCancel(ctx)
	out := make([]BatchItem, 0, len(ids))
	kinds := map[string]bool{}
	for _, id := range ids {
		it := BatchItem{MediaID: id}
		if m, err := s.Store.MediaByID(ctx, id); err == nil {
			it.Kind, it.Title = m.Kind, m.Title
		}
		res, err := s.Delete(ctx, Request{MediaID: id, DeleteFiles: opt.DeleteFiles, ImportExclusion: opt.ImportExclusion,
			Confirmed: true, SkipPlexRefresh: true, Username: opt.Username})
		if err != nil {
			it.Error = Explain(err)
		} else {
			it.OK, it.SizeFreed, it.Kind, it.Title = true, res.SizeFreed, res.Kind, res.Title
			kinds[res.Kind] = true
		}
		out = append(out, it)
	}
	if up := s.Upstreams.Get(); !s.DryRun && s.PlexRefresh && opt.DeleteFiles && up.Plex != nil {
		for kind := range kinds {
			s.refreshPlex(ctx, up.Plex, kind)
		}
	}
	return out
}

// ProtectBatch protects items one after the other.
func (s *Service) ProtectBatch(ctx context.Context, ids []int64, username string) []BatchItem {
	ctx = context.WithoutCancel(ctx)
	out := make([]BatchItem, 0, len(ids))
	for _, id := range ids {
		it := BatchItem{MediaID: id}
		res, err := s.SetProtected(ctx, ProtectRequest{MediaID: id, Protect: true, Username: username})
		if err != nil {
			it.Error = Explain(err)
			if m, merr := s.Store.MediaByID(ctx, id); merr == nil {
				it.Kind, it.Title = m.Kind, m.Title
			}
		} else {
			it.OK, it.Unchanged, it.Kind, it.Title = true, res.Unchanged, res.Kind, res.Title
		}
		out = append(out, it)
	}
	return out
}

// Explain turns an error into a French sentence for the user.
func Explain(err error) string {
	for _, known := range []error{ErrNotFound, ErrNotDeletable, ErrProtected, ErrConfirmMismatch, ErrNoSuchSeason,
		ErrNotManaged, ErrNoProtectedTag, ErrUnconfigured} {
		if errors.Is(err, known) {
			return capitalize(err.Error()) + "."
		}
	}
	return "Échec : " + err.Error()
}

func capitalize(s string) string {
	r, size := utf8.DecodeRuneInString(s)
	return strings.ToUpper(string(r)) + s[size:]
}
