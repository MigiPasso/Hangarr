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

// DeleteBatch deletes items one after the other: each goes
// through Delete (live protection check, dry-run, journal); the typed
// confirmation was done once for the whole batch. The media server is
// refreshed once at the end. A failure does not stop the others.
func (s *Service) DeleteBatch(ctx context.Context, ids []int64, opt BatchOptions) []BatchItem {
	out := make([]BatchItem, 0, len(ids))
	if !s.enter() {
		for _, id := range ids {
			out = append(out, s.batchItem(ctx, id, ErrStopping))
		}
		return out
	}
	defer s.leave()
	// Finish the batch even if the client goes away: half-done batches are
	// harder to understand than a complete journal. A shutdown (Stop) ends
	// it before the next item instead.
	ctx = context.WithoutCancel(ctx)
	kinds := map[string]bool{}
	for _, id := range ids {
		if s.Stopping() {
			out = append(out, s.batchItem(ctx, id, ErrStopping))
			continue
		}
		res, err := s.Delete(ctx, Request{MediaID: id, DeleteFiles: opt.DeleteFiles, ImportExclusion: opt.ImportExclusion,
			Confirmed: true, SkipServerRefresh: true, SkipRecompute: true, Username: opt.Username})
		if err != nil {
			out = append(out, s.batchItem(ctx, id, err))
			continue
		}
		out = append(out, BatchItem{MediaID: id, OK: true, SizeFreed: res.SizeFreed, Kind: res.Kind, Title: res.Title})
		kinds[res.Kind] = true
	}
	if !s.DryRun && len(kinds) > 0 {
		// Once for the whole batch, not after each item.
		if err := s.Refresher.Recompute(ctx); err != nil {
			s.Log.Warn("recompute after batch failed", "err", err)
		}
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

// batchItem is the report line of an item that failed with err.
func (s *Service) batchItem(ctx context.Context, id int64, err error) BatchItem {
	it := BatchItem{MediaID: id, Err: err}
	if m, merr := s.Store.MediaByID(ctx, id); merr == nil {
		it.Kind, it.Title = m.Kind, m.Title
	}
	return it
}

// ProtectBatch protects items one after the other.
func (s *Service) ProtectBatch(ctx context.Context, ids []int64, username string) []BatchItem {
	ctx = context.WithoutCancel(ctx)
	out := make([]BatchItem, 0, len(ids))
	for _, id := range ids {
		res, err := s.SetProtected(ctx, ProtectRequest{MediaID: id, Protect: true, Username: username})
		if err != nil {
			out = append(out, s.batchItem(ctx, id, err))
			continue
		}
		out = append(out, BatchItem{MediaID: id, OK: true, Unchanged: res.Unchanged, Kind: res.Kind, Title: res.Title})
	}
	return out
}
