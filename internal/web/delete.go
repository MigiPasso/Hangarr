package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"hangarr/internal/actions"
	"hangarr/internal/i18n"
	"hangarr/internal/store"
	"hangarr/internal/upstream"
)

// --- deletion -----------------------------------------------------------------

// serverDeletable: only the media server in use still knows the item (not
// managed by Sonarr/Radarr), so it is deleted through that server (no season
// deletion there). Hidden items belong to the server not in use.
func serverDeletable(up *upstream.Clients, m *store.Media) bool {
	if m.ArrID != 0 || m.MatchState == "hidden" {
		return false
	}
	if up.MediaServer == upstream.Jellyfin {
		return m.JellyfinID != ""
	}
	return m.PlexRatingKey != ""
}

type deleteData struct {
	M               *store.Media
	Path            string
	Server          string // label of the media server the item is deleted through (files always deleted, size may be unknown)
	Season          *int
	SizeBytes       int64
	DryRun          bool
	ConfirmByTyping bool
	Protected       bool
	DeleteFiles     bool
	ImportExclusion bool
	Error           string
}

func (s *Server) deleteForm(r *http.Request, m *store.Media, season *int) (deleteData, error) {
	d := deleteData{M: m, Path: kindPath(m.Kind), Season: season, SizeBytes: m.SizeBytes, DryRun: s.Config.DryRun,
		ConfirmByTyping: s.Config.ConfirmByTyping && m.Kind == "series" && season == nil,
		Protected:       store.ProtectionOf(m.Tags, m.ProtectedLocal, s.Config.ProtectedTags) != store.Unprotected,
		DeleteFiles:     true}
	if m.ArrID == 0 {
		d.Server = s.Upstreams.Get().ServerLabel()
	}
	if season != nil {
		seasons, err := s.Store.Seasons(r.Context(), m.ID)
		if err != nil {
			return d, err
		}
		d.SizeBytes = 0
		for _, se := range seasons {
			if se.Number == *season {
				d.SizeBytes = se.SizeBytes
			}
		}
	}
	return d, nil
}

// seasonDeletable: only series managed by Sonarr have seasons to delete.
func seasonDeletable(m *store.Media) bool {
	return m.Kind == "series" && m.ArrID != 0
}

func seasonParam(v string) *int {
	if v == "" {
		return nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return nil
	}
	return &n
}

func (s *Server) renderDelete(w http.ResponseWriter, r *http.Request, status int, d deleteData) {
	pd := s.pageRaw(r, s.loc(r).T("Delete %s", d.M.Title), d.M.Kind, d)
	if isHTMX(r) {
		s.render(w, r, http.StatusOK, "delete", "delete_form", pd)
		return
	}
	s.render(w, r, status, "delete", "", pd)
}

func (s *Server) deletePage(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m, ok := s.mediaFromPath(w, r, kind)
		if !ok {
			return
		}
		season := seasonParam(r.URL.Query().Get("season"))
		if (m.ArrID == 0 && !serverDeletable(s.Upstreams.Get(), m)) || (season != nil && !seasonDeletable(m)) {
			s.notFound(w, r)
			return
		}
		d, err := s.deleteForm(r, m, season)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		s.renderDelete(w, r, http.StatusOK, d)
	}
}

// actionErrors explain the known actions errors (English keys).
var actionErrors = []struct {
	err error
	msg string
}{
	{actions.ErrNotFound, "Item not found."},
	{actions.ErrNotDeletable, "Not managed by Sonarr/Radarr: cannot be deleted."},
	{actions.ErrProtected, "Protected by a tag."},
	{actions.ErrProtectedLocal, "Protected in Hangarr."},
	{actions.ErrConfirmMismatch, "The typed title does not match."},
	{actions.ErrNoSuchSeason, "Season not found."},
	{actions.ErrNotManaged, "Not managed by Sonarr/Radarr: cannot be protected."},
	{actions.ErrNoProtectedTag, "No protection tag configured (PROTECTED_TAGS)."},
	{actions.ErrUnconfigured, "Service not configured."},
	{actions.ErrPlexDeletionDenied, "Plex refused the deletion: allow it on the Plex server (Settings › Library › Allow media deletion)."},
	{actions.ErrJellyfinDeletionDenied, "Jellyfin refused the deletion: check that its API key may delete media."},
	{actions.ErrInactiveServer, "Known only by the media server that is not in use: nothing is done."},
	{actions.ErrServerConflict, "Plex and Jellyfin are both configured: clear one of them first."},
	{actions.ErrStopping, "Hangarr is stopping: nothing was sent. Try again once it has restarted."},
	{actions.ErrArrCatalogUnhealthy, "The last Sonarr/Radarr catalog sync failed or was refused: nothing is deleted through the media server until it succeeds (see Health)."},
	{actions.ErrUnmonitorAfterDelete, "Files deleted, but the season could not be unmonitored: Sonarr may download it again."},
}

// explain turns an actions error into a sentence in the user's language;
// unknown errors are prefixed with failed ("Deletion failed: %s").
func explain(l i18n.Localizer, err error, failed string) string {
	for _, k := range actionErrors {
		if errors.Is(err, k.err) {
			return l.T(k.msg)
		}
	}
	return l.T(failed, err.Error())
}

func (s *Server) deleteSubmit(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m, ok := s.mediaFromPath(w, r, kind)
		if !ok {
			return
		}
		season := seasonParam(r.PostFormValue("season"))
		if season != nil && !seasonDeletable(m) {
			// A season of a movie (or of a series Sonarr does not manage)
			// must never turn into a deletion of the whole item.
			s.notFound(w, r)
			return
		}
		d, err := s.deleteForm(r, m, season)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		d.DeleteFiles = r.PostFormValue("delete_files") == "1"
		d.ImportExclusion = r.PostFormValue("import_exclusion") == "1"
		if s.Actions == nil {
			d.Error = s.loc(r).T("Deletion unavailable.")
			s.renderDelete(w, r, http.StatusServiceUnavailable, d)
			return
		}
		res, err := s.Actions.Delete(r.Context(), actions.Request{
			MediaID: m.ID, Season: season, DeleteFiles: d.DeleteFiles, ImportExclusion: d.ImportExclusion,
			ConfirmTitle: r.PostFormValue("confirm_title"), Username: sessionFrom(r).Username,
		})
		if err != nil {
			d.Error = explain(s.loc(r), err, "Deletion failed: %s")
			s.renderDelete(w, r, http.StatusUnprocessableEntity, d)
			return
		}
		l := s.loc(r)
		var msg string
		switch {
		case res.DryRun && res.Season != nil:
			msg = l.T("Simulation: season %d of “%s” would have been deleted (%s). Nothing changed.", *res.Season, res.Title, l.Bytes(res.SizeFreed))
		case res.DryRun:
			msg = l.T("Simulation: “%s” would have been deleted (%s). Nothing changed.", res.Title, l.Bytes(res.SizeFreed))
		case res.Season != nil:
			msg = l.T("Season %d of “%s” was deleted (%s freed).", *res.Season, res.Title, l.Bytes(res.SizeFreed))
		default:
			msg = l.T("“%s” was deleted (%s freed).", res.Title, l.Bytes(res.SizeFreed))
		}
		detailURL := fmt.Sprintf("%s/%d", kindPath(kind), m.ID)
		if !isHTMX(r) {
			if res.Season != nil || res.DryRun {
				http.Redirect(w, r, detailURL, http.StatusSeeOther)
			} else {
				http.Redirect(w, r, kindPath(kind), http.StatusSeeOther)
			}
			return
		}
		level := "success"
		if res.DryRun {
			level = "info"
		}
		if res.Warning != nil {
			// Done, but not entirely: say what is left to do.
			msg, level = msg+" "+explain(l, res.Warning, "%s"), "info"
		}
		trigger := map[string]any{
			"hangarr:toast": map[string]any{"message": msg, "level": level},
			"hangarr:deleted": map[string]any{"mediaId": m.ID, "season": res.Season, "dryRun": res.DryRun,
				"detailUrl": detailURL, "libraryUrl": kindPath(kind)},
		}
		w.Header().Set("HX-Trigger", asciiJSON(trigger))
		w.Header().Set("HX-Reswap", "none")
		w.WriteHeader(http.StatusOK)
	}
}

// asciiJSON encodes v with non-ASCII characters escaped (\uXXXX): header
// values are read as Latin-1 by browsers.
func asciiJSON(v any) string {
	b, _ := json.Marshal(v)
	var sb strings.Builder
	for _, r := range string(b) {
		switch {
		case r < 0x80:
			sb.WriteRune(r)
		case r <= 0xFFFF:
			fmt.Fprintf(&sb, `\u%04x`, r)
		default:
			r -= 0x10000
			fmt.Fprintf(&sb, `\u%04x\u%04x`, 0xD800+(r>>10), 0xDC00+(r&0x3FF))
		}
	}
	return sb.String()
}
