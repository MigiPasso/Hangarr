package actions

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"

	"hangarr/internal/clients/httpx"
	"hangarr/internal/store"
	"hangarr/internal/upstream"
)

// DeleteVersion is the action_log name of the deletion of one version of a
// movie the media server has several times.
const DeleteVersion = "delete_version"

// Errors of a version deletion, explained to the user.
var (
	// ErrStaleVersions: the versions changed since the page was shown.
	ErrStaleVersions = store.ErrStaleDuplicate
	// ErrLastVersion: the server has one version left; deleting it would
	// delete the movie, which is not what this page does.
	ErrLastVersion = errors.New("only one version is left")
	// ErrSameFolder: Jellyfin deletes the whole folder of an item, so the
	// other versions in that folder would go too (seen with Jellyfin 12.2).
	ErrSameFolder = errors.New("another version is in the same folder: Jellyfin would delete it too")
	// ErrRadarrMonitored: this is Radarr's file and Radarr monitors the movie:
	// it would download it again.
	ErrRadarrMonitored = errors.New("this is Radarr's file and the movie is monitored: Radarr would download it again")
	// ErrRadarrFileUnknown: Radarr has a file for this movie but none of the
	// versions has its name, and the movie is monitored.
	ErrRadarrFileUnknown = errors.New("none of these versions is Radarr's file and the movie is monitored")
	// ErrRadarrConfirm: Radarr does not monitor the movie but will see its
	// file as missing; the user must confirm (VersionRequest.AcceptRadarr).
	ErrRadarrConfirm = errors.New("the movie will be missing in Radarr")
)

// VersionRequest deletes one version of a duplicate group.
type VersionRequest struct {
	GroupKey    string
	VersionKey  string
	Fingerprint string // versions the user saw (store.DuplicateGroup.Fingerprint)
	// AcceptRadarr: the user confirmed ErrRadarrConfirm.
	AcceptRadarr bool
	Username     string
}

// VersionResult describes what was (or would have been) deleted.
type VersionResult struct {
	Title     string
	File      string
	SizeFreed int64
	DryRun    bool
	// RadarrFile: it was Radarr's file (Radarr will see the movie missing).
	RadarrFile bool
}

// liveVersion is a version as the media server lists it now.
type liveVersion struct {
	item, key, file string
}

// DeleteVersion deletes (or simulates) one version of a movie the media
// server has several times, through that server. Before anything is sent it
// checks, on live data: the protection of the movie (Radarr tags and local
// protection), that the version still exists and is not the last one, that
// Jellyfin would not take another version with it, and that Radarr will not
// download the file again. Deletions run one at a time: two at once could
// each see the other version and delete both.
func (s *Service) DeleteVersion(ctx context.Context, req VersionRequest) (*VersionResult, error) {
	s.versionMu.Lock()
	defer s.versionMu.Unlock()
	up := s.Upstreams.Get()
	var server string
	switch {
	case up.ServerConflict:
		return nil, ErrServerConflict
	case up.MediaServer == upstream.Plex && up.Plex != nil:
		server = store.ServerPlex
	case up.MediaServer == upstream.Jellyfin && up.Jellyfin != nil:
		server = store.ServerJellyfin
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnconfigured, up.ServerLabel())
	}
	g, err := s.Store.DuplicateGroup(ctx, server, req.GroupKey, s.ProtectedTags)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if g.Fingerprint != req.Fingerprint {
		return nil, ErrStaleVersions
	}
	var v *store.DuplicateVersion
	for i := range g.Versions {
		if g.Versions[i].VersionKey == req.VersionKey {
			v = &g.Versions[i]
		}
	}
	if v == nil {
		return nil, ErrNotFound
	}
	res := &VersionResult{Title: g.Title, File: v.File, SizeFreed: v.SizeBytes, DryRun: s.DryRun}

	wholeItem, err := s.checkVersion(ctx, up, server, g, v, req, res)
	if errors.Is(err, ErrRadarrConfirm) {
		return nil, err // a question to the user, not an attempt
	}
	if err == nil && !s.DryRun {
		err = s.sendVersionDeletion(ctx, up, server, v, wholeItem)
	}
	s.journalVersion(ctx, g, v, req, err)
	if err != nil {
		return nil, err
	}
	if !s.DryRun {
		s.afterVersionDeletion(context.WithoutCancel(ctx), up, server, v, wholeItem)
	}
	return res, nil
}

// checkVersion runs the checks of DeleteVersion; wholeItem reports that the
// version is its item's only one, so the whole item is deleted.
func (s *Service) checkVersion(ctx context.Context, up *upstream.Clients, server string, g *store.DuplicateGroup,
	v *store.DuplicateVersion, req VersionRequest, res *VersionResult) (wholeItem bool, err error) {
	if g.Protected {
		return false, ErrProtected
	}
	live, err := s.liveVersions(ctx, up, server, g)
	if err != nil {
		return false, err
	}
	var target *liveVersion
	inItem := 0
	for i := range live {
		if live[i].item == v.ItemKey {
			inItem++
			if live[i].key == v.VersionKey {
				target = &live[i]
			}
		}
	}
	switch {
	case target == nil:
		return false, ErrStaleVersions
	case len(live) < 2:
		return false, ErrLastVersion
	}
	if server == store.ServerJellyfin {
		// Jellyfin deletes the item's folder: refuse if any other version is
		// in it, grouped under the same item or not.
		if inItem > 1 {
			return false, ErrSameFolder
		}
		for _, o := range live {
			if o.key != target.key && dirOf(o.file) == dirOf(target.file) {
				return false, ErrSameFolder
			}
		}
	}
	if g.ArrID != 0 {
		if err := s.checkRadarr(ctx, up, g.ArrID, *target, live, req, res); err != nil {
			return false, err
		}
	}
	return inItem == 1, nil
}

// checkRadarr reads the movie from Radarr: a protection tag added since the
// sync refuses, and so does deleting the file of a monitored movie (Radarr
// would download it again). Deleting its file when the movie is not
// monitored only needs the user's confirmation. A movie Radarr no longer has
// sets no condition.
func (s *Service) checkRadarr(ctx context.Context, up *upstream.Clients, arrID int64, target liveVersion,
	live []liveVersion, req VersionRequest, res *VersionResult) error {
	if up.Radarr == nil {
		return fmt.Errorf("%w: Radarr", ErrUnconfigured)
	}
	mv, err := up.Radarr.MovieByID(ctx, arrID)
	if isNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	tags, err := up.Radarr.Tags(ctx)
	if err != nil {
		return err
	}
	byID := map[int64]string{}
	for _, t := range tags {
		byID[t.ID] = t.Label
	}
	labels := make([]string, 0, len(mv.Tags))
	for _, id := range mv.Tags {
		labels = append(labels, byID[id])
	}
	if s.protected(labels) {
		return ErrProtected
	}
	if !mv.HasFile {
		return nil
	}
	radarrFile := ""
	if mv.MovieFile != nil {
		radarrFile = baseOf(mv.MovieFile.RelativePath)
	}
	isRadarr, known := false, false
	for _, l := range live {
		if radarrFile != "" && strings.EqualFold(baseOf(l.file), radarrFile) {
			known = true
			if l.key == target.key {
				isRadarr = true
			}
		}
	}
	switch {
	case isRadarr && mv.Monitored:
		return ErrRadarrMonitored
	case !known && mv.Monitored:
		return ErrRadarrFileUnknown
	case (isRadarr || !known) && !req.AcceptRadarr:
		return ErrRadarrConfirm
	}
	res.RadarrFile = isRadarr
	return nil
}

// liveVersions reads the versions of the group's items from the media server
// (an item it no longer has has none).
func (s *Service) liveVersions(ctx context.Context, up *upstream.Clients, server string, g *store.DuplicateGroup) ([]liveVersion, error) {
	var items []string
	seen := map[string]bool{}
	for _, v := range g.Versions {
		if !seen[v.ItemKey] {
			seen[v.ItemKey] = true
			items = append(items, v.ItemKey)
		}
	}
	var out []liveVersion
	if server == store.ServerJellyfin {
		got, err := up.Jellyfin.Versions(ctx, items)
		if err != nil {
			return nil, err
		}
		for _, it := range got {
			if !seen[it.ID] {
				continue
			}
			for _, src := range it.MediaSources {
				if src.ID != "" {
					out = append(out, liveVersion{item: it.ID, key: src.ID, file: src.Path})
				}
			}
		}
		return out, nil
	}
	for _, key := range items {
		it, err := up.Plex.Metadata(ctx, key)
		if isNotFound(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, m := range it.Media {
			if m.ID <= 0 {
				continue
			}
			lv := liveVersion{item: key, key: fmt.Sprint(int64(m.ID))}
			if len(m.Part) > 0 {
				lv.file = m.Part[0].File
			}
			out = append(out, lv)
		}
	}
	return out, nil
}

// sendVersionDeletion asks the media server to delete the version with its
// files: Plex deletes one media of an item, or the item when it is its only
// one; Jellyfin deletes the item (checkVersion made sure it holds no other
// version).
func (s *Service) sendVersionDeletion(ctx context.Context, up *upstream.Clients, server string,
	v *store.DuplicateVersion, wholeItem bool) error {
	var err error
	denied := ErrPlexDeletionDenied
	switch {
	case server == store.ServerJellyfin:
		denied = ErrJellyfinDeletionDenied
		err = up.Jellyfin.DeleteItem(ctx, v.ItemKey)
	case wholeItem:
		err = up.Plex.DeleteItem(ctx, v.ItemKey)
	default:
		err = up.Plex.DeleteMedia(ctx, v.ItemKey, v.VersionKey)
	}
	var se *httpx.StatusError
	switch {
	case errors.As(err, &se) && (se.Status == http.StatusUnauthorized || se.Status == http.StatusForbidden):
		return denied
	case isNotFound(err):
		return fmt.Errorf("%w in %s", ErrNotFound, up.ServerLabel())
	}
	return err
}

// afterVersionDeletion updates local data: the version is forgotten (its
// group too when one version is left) and an item deleted whole that only
// the server knew leaves the library. The next sync reads everything again
// anyway. Failures are logged: the deletion itself succeeded.
func (s *Service) afterVersionDeletion(ctx context.Context, up *upstream.Clients, server string,
	v *store.DuplicateVersion, wholeItem bool) {
	err := s.Store.RemoveVersion(ctx, server, v.VersionKey)
	if err == nil && wholeItem && v.MediaID != 0 {
		var m *store.Media
		if m, err = s.Store.MediaByID(ctx, v.MediaID); err == nil && m.ArrID == 0 {
			if err = s.Store.DeleteMedia(ctx, m.ID); err == nil {
				err = s.Refresher.Recompute(ctx)
			}
		}
	}
	if err != nil {
		s.Log.Warn("local refresh after version deletion failed", "version", v.VersionKey, "err", err)
	}
	if s.PlexRefresh {
		s.refreshServer(ctx, up, []string{"movie"})
	}
}

// journalVersion records a version deletion attempt in action_log and in the
// logs; the title names the file.
func (s *Service) journalVersion(ctx context.Context, g *store.DuplicateGroup, v *store.DuplicateVersion,
	req VersionRequest, err error) {
	title := g.Title
	if name := baseOf(v.File); name != "" {
		title += " (" + name + ")"
	}
	entry := store.Action{
		At: s.now().Unix(), Action: DeleteVersion, MediaKind: "movie", ArrID: g.ArrID, Title: title,
		SizeFreedBytes: v.SizeBytes, DeleteFiles: true, DryRun: s.DryRun, Success: err == nil, Username: req.Username,
	}
	if err != nil {
		entry.SizeFreedBytes = 0
		entry.Error = err.Error()
	}
	if lerr := s.Store.AddAction(context.WithoutCancel(ctx), entry); lerr != nil {
		s.Log.Error("action not journaled", "err", lerr)
	}
	s.Log.Info("version deletion", "title", g.Title, "file", v.File, "dry_run", s.DryRun,
		"success", err == nil, "err", err, "user", req.Username)
}

// baseOf is the file name of a path written with / or \ (Windows servers).
func baseOf(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

// dirOf is the folder of a path written with / or \.
func dirOf(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return path.Clean(strings.ReplaceAll(p[:i], `\`, "/"))
	}
	return ""
}
