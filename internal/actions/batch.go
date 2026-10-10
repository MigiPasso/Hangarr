package actions

import (
	"context"
	"sort"
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
	Unchanged bool  // protection: already protected
	Err       error // explained by the web layer
}

// BatchOptions are the deletion options applied to every item.
type BatchOptions struct {
	DeleteFiles     bool
	ImportExclusion bool
	Username        string
}

// DeleteBatch deletes items one after the other (SPEC §7.4): each goes
// through Delete (live protection check, dry-run, journal); the typed
// confirmation was done once for the whole batch. The media server is
// refreshed once at the end. A failure does not stop the others.
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
			Confirmed: true, SkipServerRefresh: true, Username: opt.Username})
		if err != nil {
			it.Err = err
		} else {
			it.OK, it.SizeFreed, it.Kind, it.Title = true, res.SizeFreed, res.Kind, res.Title
			kinds[res.Kind] = true
		}
		out = append(out, it)
	}
	if !s.DryRun && s.PlexRefresh && opt.DeleteFiles && len(kinds) > 0 {
		list := make([]string, 0, len(kinds))
		for kind := range kinds {
			list = append(list, kind)
		}
		sort.Strings(list)
		s.refreshServer(ctx, s.Upstreams.Get(), list)
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
			it.Err = err
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
