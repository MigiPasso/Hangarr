package actions

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"hangarr/internal/clients/httpx"
	"hangarr/internal/clients/plex"
	"hangarr/internal/media"
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
	Note       string // see Result.Note
}

// LiveVersion is a version as the media server lists it now, with its
// tracks (read by the deletion checks and by the Duplicates page).
type LiveVersion struct {
	Item, Key, File string
	Audio           []string // display titles
	Subtitles       []Track
}

// Track is a subtitle track.
type Track struct {
	Title  string
	Forced bool
}

// JellyfinTakesOther reports whether Jellyfin, deleting target, would take
// another of versions with it: Jellyfin deletes the item's whole folder, so
// any other version of the same item or in the same folder goes too (seen
// with Jellyfin 12.2).
func JellyfinTakesOther(versions []LiveVersion, target LiveVersion) bool {
	for _, o := range versions {
		if o.Key != target.Key && (o.Item == target.Item || media.Dir(o.File) == media.Dir(target.File)) {
			return true
		}
	}
	return false
}

// DeleteVersion deletes (or simulates) one version of a movie the media
// server has several times, through that server. Before anything is sent it
// checks, on live data: the protection of the movie (Radarr tags and local
// protection), that the version still exists and is not the last one, that
// Jellyfin would not take another version with it, and that Radarr will not
// download the file again. Deletions run one at a time: two at once could
// each see the other version and delete both.
func (s *Service) DeleteVersion(ctx context.Context, req VersionRequest) (*VersionResult, error) {
	if !s.enter() {
		return nil, ErrStopping
	}
	defer s.leave()
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
	id, jerr := s.journalVersionBegin(ctx, g, v, req)
	if jerr != nil {
		return nil, jerr // nothing sent without its journal entry
	}
	if err == nil && !s.DryRun {
		res.Note, err = s.sendVersionDeletion(ctx, up, server, v, wholeItem)
	}
	s.journalVersionEnd(ctx, id, g, v, req, res, err)
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
	var target *LiveVersion
	inItem := 0
	for i := range live {
		if live[i].Item == v.ItemKey {
			inItem++
			if live[i].Key == v.VersionKey {
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
	if server == store.ServerJellyfin && JellyfinTakesOther(live, *target) {
		return false, ErrSameFolder
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
func (s *Service) checkRadarr(ctx context.Context, up *upstream.Clients, arrID int64, target LiveVersion,
	live []LiveVersion, req VersionRequest, res *VersionResult) error {
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
	if err := s.protectionError(labels, false); err != nil {
		return err
	}
	if !mv.HasFile {
		return nil
	}
	radarrFile := ""
	if mv.MovieFile != nil {
		radarrFile = media.Base(mv.MovieFile.RelativePath)
	}
	isRadarr, known := false, false
	for _, l := range live {
		if radarrFile != "" && strings.EqualFold(media.Base(l.File), radarrFile) {
			known = true
			if l.Key == target.Key {
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

// liveVersions reads the versions of the group's items from the media server.
func (s *Service) liveVersions(ctx context.Context, up *upstream.Clients, server string, g *store.DuplicateGroup) ([]LiveVersion, error) {
	return LiveVersions(ctx, up, server, g)
}

// LiveVersions reads the versions of the group's items, with their tracks,
// from the media server (an item it no longer has has none).
func LiveVersions(ctx context.Context, up *upstream.Clients, server string, g *store.DuplicateGroup) ([]LiveVersion, error) {
	var items []string
	seen := map[string]bool{}
	for _, v := range g.Versions {
		if !seen[v.ItemKey] {
			seen[v.ItemKey] = true
			items = append(items, v.ItemKey)
		}
	}
	var out []LiveVersion
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
				if src.ID == "" {
					continue
				}
				lv := LiveVersion{Item: it.ID, Key: src.ID, File: src.Path}
				for _, st := range src.MediaStreams {
					switch st.Type {
					case "Audio":
						lv.Audio = append(lv.Audio, st.DisplayTitle)
					case "Subtitle":
						lv.Subtitles = append(lv.Subtitles, Track{Title: st.DisplayTitle, Forced: st.IsForced})
					}
				}
				out = append(out, lv)
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
			lv := LiveVersion{Item: key, Key: fmt.Sprint(int64(m.ID))}
			if len(m.Part) > 0 {
				lv.File = m.Part[0].File
				for _, st := range m.Part[0].Stream {
					switch st.StreamType {
					case plex.StreamAudio:
						lv.Audio = append(lv.Audio, st.DisplayTitle)
					case plex.StreamSubtitle:
						lv.Subtitles = append(lv.Subtitles, Track{Title: st.DisplayTitle, Forced: st.Forced})
					}
				}
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
	v *store.DuplicateVersion, wholeItem bool) (note string, err error) {
	denied := ErrPlexDeletionDenied
	// gone checks the version is no longer listed, after a deletion without answer.
	gone := func(ctx context.Context) (bool, error) {
		it, err := up.Plex.Metadata(ctx, v.ItemKey)
		if isNotFound(err) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		for _, m := range it.Media {
			if fmt.Sprint(int64(m.ID)) == v.VersionKey {
				return false, nil
			}
		}
		return true, nil
	}
	switch {
	case server == store.ServerJellyfin:
		denied = ErrJellyfinDeletionDenied
		err = up.Jellyfin.DeleteItem(ctx, v.ItemKey)
		gone = func(ctx context.Context) (bool, error) {
			items, err := up.Jellyfin.Versions(ctx, []string{v.ItemKey})
			return err == nil && len(items) == 0, err
		}
	case wholeItem:
		err = up.Plex.DeleteItem(ctx, v.ItemKey)
	default:
		err = up.Plex.DeleteMedia(ctx, v.ItemKey, v.VersionKey)
	}
	var se *httpx.StatusError
	switch {
	case errors.As(err, &se) && (se.Status == http.StatusUnauthorized || se.Status == http.StatusForbidden):
		return "", denied
	case isNotFound(err):
		return "", fmt.Errorf("%w in %s", ErrNotFound, up.ServerLabel())
	}
	return s.confirmGone(ctx, err, gone)
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

// journalVersionBegin writes a version deletion attempt to action_log
// (pending) before anything is sent; the title names the file.
func (s *Service) journalVersionBegin(ctx context.Context, g *store.DuplicateGroup, v *store.DuplicateVersion,
	req VersionRequest) (int64, error) {
	title := g.Title
	if name := media.Base(v.File); name != "" {
		title += " (" + name + ")"
	}
	id, err := s.Store.BeginAction(context.WithoutCancel(ctx), store.Action{
		At: s.now().Unix(), Action: DeleteVersion, MediaKind: "movie", ArrID: g.ArrID, Title: title,
		DeleteFiles: true, DryRun: s.DryRun, Username: req.Username,
	})
	if err != nil {
		s.Log.Error("action not journaled: nothing sent", "title", title, "err", err)
		return 0, fmt.Errorf("journal: %w", err)
	}
	return id, nil
}

// journalVersionEnd records the outcome of a version deletion attempt in
// action_log and in the logs.
func (s *Service) journalVersionEnd(ctx context.Context, id int64, g *store.DuplicateGroup, v *store.DuplicateVersion,
	req VersionRequest, res *VersionResult, err error) {
	size, msg := v.SizeBytes, res.Note
	if err != nil {
		size, msg = 0, err.Error()
	}
	if lerr := s.Store.FinishAction(context.WithoutCancel(ctx), id, err == nil, size, msg); lerr != nil {
		s.Log.Error("action outcome not journaled", "err", lerr)
	}
	s.Log.Info("version deletion", "title", g.Title, "file", v.File, "dry_run", s.DryRun,
		"success", err == nil, "err", err, "note", res.Note, "user", req.Username)
}
