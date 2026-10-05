package actions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"arrstats/internal/clients/sonarr"
	"arrstats/internal/store"
	"arrstats/internal/upstream"
)

// Protection actions stored in action_log.
const (
	ProtectAction   = "protect"
	UnprotectAction = "unprotect"
)

// Protection errors shown to the user.
var (
	ErrNoProtectedTag = errors.New("aucun tag de protection n'est configuré (PROTECTED_TAGS)")
	ErrNotManaged     = errors.New("élément non géré par Sonarr/Radarr : protection impossible")
)

// ProtectRequest adds (Protect) or removes the protection of an item.
type ProtectRequest struct {
	MediaID  int64
	Protect  bool
	Username string
}

// ProtectResult describes what was (or would have been) changed.
type ProtectResult struct {
	Title     string
	Kind      string
	Protect   bool
	Tag       string // tag added; for a removal, the protection tags removed
	Unchanged bool   // the item was already in the requested state
	DryRun    bool
}

// SetProtected adds the first PROTECTED_TAGS tag to the item in Sonarr/Radarr
// (creating the tag when it does not exist yet), or removes every protection
// tag. The object is read live and sent back whole (PUT), like the season
// unmonitoring. Every attempt that reaches Sonarr/Radarr is journaled.
func (s *Service) SetProtected(ctx context.Context, req ProtectRequest) (*ProtectResult, error) {
	m, err := s.Store.MediaByID(ctx, req.MediaID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if m.ArrID == 0 {
		return nil, ErrNotManaged
	}
	if len(s.ProtectedTags) == 0 || strings.TrimSpace(s.ProtectedTags[0]) == "" {
		return nil, ErrNoProtectedTag
	}
	res := &ProtectResult{Title: m.Title, Kind: m.Kind, Protect: req.Protect, DryRun: s.DryRun}
	labels, err := s.applyProtection(ctx, s.Upstreams.Get(), m, res)
	if err == nil && res.Unchanged {
		// Nothing sent, not journaled; the local copy may be a day old,
		// align it with the live tags.
		if err := s.Store.SetMediaTags(context.WithoutCancel(ctx), m.ID, labels); err != nil {
			s.Log.Warn("local tags not updated", "media_id", m.ID, "err", err)
		}
		return res, nil
	}
	action := UnprotectAction
	if req.Protect {
		action = ProtectAction
	}
	entry := store.Action{At: s.now().Unix(), Action: action, MediaKind: m.Kind, ArrID: m.ArrID, Title: m.Title,
		DryRun: s.DryRun, Success: err == nil, Username: req.Username}
	if err != nil {
		entry.Error = err.Error()
	}
	if lerr := s.Store.AddAction(context.WithoutCancel(ctx), entry); lerr != nil {
		s.Log.Error("action not journaled", "err", lerr)
	}
	s.Log.Info("protection", "action", action, "title", m.Title, "tag", res.Tag, "dry_run", s.DryRun,
		"success", err == nil, "err", err, "user", req.Username)
	if err != nil {
		return nil, err
	}
	if !s.DryRun {
		if err := s.Store.SetMediaTags(context.WithoutCancel(ctx), m.ID, labels); err != nil {
			s.Log.Warn("local tags not updated", "media_id", m.ID, "err", err)
		}
	}
	return res, nil
}

// arrItem is what protection needs from Sonarr or Radarr.
type arrItem struct {
	tags      func(context.Context) ([]sonarr.Tag, error)
	createTag func(context.Context, string) (*sonarr.Tag, error)
	get       func(context.Context, int64) ([]int64, json.RawMessage, error)
	put       func(context.Context, int64, json.RawMessage) error
}

func arrFor(up *upstream.Clients, kind string) (*arrItem, bool) {
	if kind == "movie" {
		c := up.Radarr
		if c == nil {
			return nil, false
		}
		return &arrItem{tags: c.Tags, createTag: c.CreateTag, put: c.UpdateMovieRaw,
			get: func(ctx context.Context, id int64) ([]int64, json.RawMessage, error) {
				mv, raw, err := c.MovieByIDRaw(ctx, id)
				if err != nil {
					return nil, nil, err
				}
				return mv.Tags, raw, nil
			}}, true
	}
	c := up.Sonarr
	if c == nil {
		return nil, false
	}
	return &arrItem{tags: c.Tags, createTag: c.CreateTag, put: c.UpdateSeriesRaw,
		get: func(ctx context.Context, id int64) ([]int64, json.RawMessage, error) {
			se, raw, err := c.SeriesByID(ctx, id)
			if err != nil {
				return nil, nil, err
			}
			return se.Tags, raw, nil
		}}, true
}

// applyProtection computes and (unless dry-run) sends the new tags; it
// returns the item's new tag labels.
func (s *Service) applyProtection(ctx context.Context, up *upstream.Clients, m *store.Media, res *ProtectResult) ([]string, error) {
	arr, ok := arrFor(up, m.Kind)
	if !ok {
		return nil, fmt.Errorf("%w : %s", ErrUnconfigured, arrName(m.Kind))
	}
	ids, raw, err := arr.get(ctx, m.ArrID)
	if isNotFound(err) {
		return nil, fmt.Errorf("%w dans %s", ErrNotFound, arrName(m.Kind))
	}
	if err != nil {
		return nil, err
	}
	all, err := arr.tags(ctx)
	if err != nil {
		return nil, err
	}
	byID := map[int64]string{}
	var want *sonarr.Tag
	wantLabel := strings.TrimSpace(s.ProtectedTags[0])
	for i, t := range all {
		byID[t.ID] = t.Label
		if want == nil && strings.EqualFold(t.Label, wantLabel) {
			want = &all[i]
		}
	}
	isProtection := func(id int64) bool { return s.protected([]string{byID[id]}) }
	labelsOf := func(ids []int64) []string {
		out := make([]string, 0, len(ids))
		for _, id := range ids {
			out = append(out, byID[id])
		}
		return out
	}

	var next []int64
	var removed []string
	if res.Protect {
		if slices.ContainsFunc(ids, isProtection) {
			res.Unchanged = true
			return labelsOf(ids), nil
		}
		res.Tag = wantLabel
		if want == nil {
			if s.DryRun {
				return nil, nil // the tag would be created
			}
			if want, err = arr.createTag(ctx, wantLabel); err != nil {
				return nil, fmt.Errorf("création du tag %q : %w", wantLabel, err)
			}
			byID[want.ID] = want.Label
		}
		res.Tag = want.Label
		next = append(slices.Clone(ids), want.ID)
	} else {
		for _, id := range ids {
			if isProtection(id) {
				removed = append(removed, byID[id])
			} else {
				next = append(next, id)
			}
		}
		if len(removed) == 0 {
			res.Unchanged = true
			return labelsOf(ids), nil
		}
		res.Tag = strings.Join(removed, ", ")
	}
	if s.DryRun {
		return nil, nil
	}
	updated, err := sonarr.WithTags(raw, next)
	if err != nil {
		return nil, err
	}
	if err := arr.put(ctx, m.ArrID, updated); err != nil {
		return nil, err
	}
	return labelsOf(next), nil
}
