package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"cleanarr/internal/actions"
	"cleanarr/internal/clients/sonarr"
	"cleanarr/internal/i18n"
	"cleanarr/internal/store"
)

// historyLimit bounds the play history shown on the detail page.
const historyLimit = 200

type link struct{ Label, URL string }

type seasonView struct {
	store.Season
	Files     int  // episodes with a file
	Total     int  // episodes known to Sonarr
	Watched   int  // distinct episodes watched (complete play) by anyone
	Deletable bool // has files or is still monitored
}

type userSeason struct {
	Number  int
	Watched int // among episodes with a file when exact
	Files   int
}

type userProgress struct {
	Name      string
	Seasons   []userSeason
	Last      string // "S01E03" (series) of the last complete play
	LastAt    int64
	Next      string // next episode on disk to watch, "" when unknown
	UpToDate  bool
	Plays     int
	Completed bool // movie: at least one complete play
}

type detailData struct {
	M          *store.Media
	Path       string // /series or /movies
	Seasons    []seasonView
	Users      []userProgress
	History    []store.HistoryRow
	Links      []link
	Protected  bool
	Deletable  bool
	CanProtect bool   // managed by Sonarr/Radarr and PROTECTED_TAGS not empty
	ProtectTag string // first PROTECTED_TAGS entry, added by « Protéger »
	CSRFToken  string
	Exact      bool // per-episode data from Sonarr
	Modal      bool
}

func kindPath(kind string) string {
	if kind == "movie" {
		return "/movies"
	}
	return "/series"
}

func episodeLabel(season, episode int) string {
	return fmt.Sprintf("S%02dE%02d", season, episode)
}

type epKey struct{ season, episode int }

// buildProgress computes season rows and per-user progress. With Sonarr
// episodes (eps != nil) counts are exact per episode; otherwise they fall
// back to the stored per-season numbers.
func buildProgress(eps []sonarr.Episode, watches []store.EpisodeWatch, seasons []store.Season) ([]seasonView, []userProgress) {
	hasFile := map[epKey]bool{}
	perSeason := map[int][2]int{} // files, total
	var onDisk []epKey
	for _, e := range eps {
		k := epKey{e.SeasonNumber, e.EpisodeNumber}
		c := perSeason[e.SeasonNumber]
		c[1]++
		if e.HasFile {
			hasFile[k] = true
			c[0]++
			if e.SeasonNumber > 0 {
				onDisk = append(onDisk, k)
			}
		}
		perSeason[e.SeasonNumber] = c
	}
	sort.Slice(onDisk, func(i, j int) bool {
		if onDisk[i].season != onDisk[j].season {
			return onDisk[i].season < onDisk[j].season
		}
		return onDisk[i].episode < onDisk[j].episode
	})
	exact := eps != nil

	watchedAny := map[int]map[int]bool{}
	for _, w := range watches {
		if w.Watched {
			if watchedAny[w.Season] == nil {
				watchedAny[w.Season] = map[int]bool{}
			}
			watchedAny[w.Season][w.Episode] = true
		}
	}

	var rows []seasonView
	for _, se := range seasons {
		v := seasonView{Season: se, Files: se.EpisodeFileCount, Total: se.TotalEpisodeCount, Watched: se.WatchedEpisodes}
		if exact {
			c := perSeason[se.Number]
			v.Files, v.Total, v.Watched = c[0], c[1], len(watchedAny[se.Number])
		}
		v.Deletable = v.Files > 0 || se.Monitored
		rows = append(rows, v)
	}

	type acc struct {
		p         userProgress
		watched   map[epKey]bool
		lastKey   epKey // last episode with a complete play
		lastKeyAt int64
	}
	byUser := map[int64]*acc{}
	var order []int64
	for _, w := range watches {
		a := byUser[w.UserID]
		if a == nil {
			a = &acc{p: userProgress{Name: w.UserName}, watched: map[epKey]bool{}}
			if a.p.Name == "" {
				a.p.Name = "Utilisateur " + strconv.FormatInt(w.UserID, 10)
			}
			byUser[w.UserID] = a
			order = append(order, w.UserID)
		}
		a.p.Plays += w.Plays
		a.p.LastAt = max(a.p.LastAt, w.LastAt)
		k := epKey{w.Season, w.Episode}
		if w.Watched {
			a.watched[k] = true
			a.p.Completed = true
			if w.LastAt >= a.lastKeyAt {
				a.lastKeyAt, a.lastKey = w.LastAt, k
			}
		}
	}
	var users []userProgress
	for _, id := range order {
		a := byUser[id]
		if a.lastKey.episode > 0 {
			a.p.Last = episodeLabel(a.lastKey.season, a.lastKey.episode)
		}
		for _, se := range rows {
			if se.Number == 0 {
				continue
			}
			us := userSeason{Number: se.Number, Files: se.Files}
			for k := range a.watched {
				if k.season == se.Number && (!exact || hasFile[k]) {
					us.Watched++
				}
			}
			if !exact && se.Files > 0 {
				us.Watched = min(us.Watched, se.Files)
			}
			a.p.Seasons = append(a.p.Seasons, us)
		}
		if exact && len(onDisk) > 0 {
			start := 0
			if a.lastKey.episode > 0 {
				for i, k := range onDisk {
					if k.season > a.lastKey.season || (k.season == a.lastKey.season && k.episode > a.lastKey.episode) {
						start = i
						break
					}
					start = len(onDisk)
				}
			}
			for _, k := range onDisk[start:] {
				if !a.watched[k] {
					a.p.Next = episodeLabel(k.season, k.episode)
					break
				}
			}
			a.p.UpToDate = a.p.Next == ""
		}
		users = append(users, a.p)
	}
	sort.SliceStable(users, func(i, j int) bool { return users[i].LastAt > users[j].LastAt })
	return rows, users
}

func (s *Server) links(ctx context.Context, m *store.Media) []link {
	var out []link
	up := s.Upstreams.Get()
	if m.ArrID > 0 && m.TitleSlug != "" {
		if m.Kind == "series" {
			out = append(out, link{"Sonarr", up.SonarrWebURL() + "/series/" + url.PathEscape(m.TitleSlug)})
		} else {
			out = append(out, link{"Radarr", up.RadarrWebURL() + "/movie/" + url.PathEscape(m.TitleSlug)})
		}
	}
	if mid := s.Store.PlexMachineID(ctx); mid != "" && m.PlexRatingKey != "" {
		base := "https://app.plex.tv/desktop"
		if pub := up.PlexWebURL(); pub != "" {
			base = pub + "/web/index.html"
		}
		out = append(out, link{"Plex", base + "#!/server/" + url.PathEscape(mid) + "/details?key=" +
			url.QueryEscape("/library/metadata/"+m.PlexRatingKey)})
	}
	if m.ImdbID != "" {
		out = append(out, link{"IMDb", "https://www.imdb.com/title/" + url.PathEscape(m.ImdbID) + "/"})
	}
	return out
}

// mediaFromPath loads the media of /{series|movies}/{id}; ok is false after
// a 404 has been written.
func (s *Server) mediaFromPath(w http.ResponseWriter, r *http.Request, kind string) (*store.Media, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.notFound(w, r)
		return nil, false
	}
	m, err := s.Store.MediaByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && m.Kind != kind) {
		s.notFound(w, r)
		return nil, false
	}
	if err != nil {
		s.serverError(w, r, err)
		return nil, false
	}
	return m, true
}

func (s *Server) detail(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m, ok := s.mediaFromPath(w, r, kind)
		if !ok {
			return
		}
		ctx := r.Context()
		d := detailData{M: m, Path: kindPath(kind), Links: s.links(ctx, m),
			Protected: isProtected(m.Tags, s.Config.ProtectedTags) || m.ProtectedLocal, Deletable: plexDeletable(m) || m.ArrID > 0}
		switch {
		case s.Actions == nil:
		case plexDeletable(m):
			d.CanProtect = true // in Cleanarr only
		case len(s.Config.ProtectedTags) > 0 && m.ArrID > 0:
			d.CanProtect, d.ProtectTag = true, s.Config.ProtectedTags[0]
		}
		if sess := sessionFrom(r); sess != nil {
			d.CSRFToken = sess.CSRFToken
		}
		var err error
		if d.History, err = s.Store.MediaHistory(ctx, m.ID, historyLimit); err != nil {
			s.serverError(w, r, err)
			return
		}
		watches, err := s.Store.UserWatches(ctx, m.ID)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		var seasons []store.Season
		var eps []sonarr.Episode
		if kind == "series" {
			if seasons, err = s.Store.Seasons(ctx, m.ID); err != nil {
				s.serverError(w, r, err)
				return
			}
			if son := s.Upstreams.Get().Sonarr; m.ArrID > 0 && son != nil {
				ectx, cancel := context.WithTimeout(ctx, 5*time.Second)
				eps, err = son.Episodes(ectx, m.ArrID)
				cancel()
				if err != nil {
					s.Log.Warn("episodes unavailable, showing approximate progress", "series", m.ArrID, "err", err)
					eps = nil
				}
			}
		}
		d.Exact = eps != nil
		d.Seasons, d.Users = buildProgress(eps, watches, seasons)
		d.Modal = isHTMX(r) && r.Header.Get("HX-Target") == "modal-body"
		pd := s.pageRaw(r, m.Title, kind, d)
		if d.Modal {
			s.render(w, r, http.StatusOK, "detail", "detail_body", pd)
			return
		}
		s.render(w, r, http.StatusOK, "detail", "", pd)
	}
}

// --- deletion -----------------------------------------------------------------

// plexDeletable: only Plex still knows the item, so it is deleted through
// Plex (no season deletion there).
func plexDeletable(m *store.Media) bool { return m.ArrID == 0 && m.PlexRatingKey != "" }

type deleteData struct {
	M               *store.Media
	Path            string
	Plex            bool // deleted through Plex: files always deleted, size may be unknown
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
	d := deleteData{M: m, Path: kindPath(m.Kind), Plex: m.ArrID == 0, Season: season, SizeBytes: m.SizeBytes, DryRun: s.Config.DryRun,
		ConfirmByTyping: s.Config.ConfirmByTyping && m.Kind == "series" && season == nil,
		Protected:       isProtected(m.Tags, s.Config.ProtectedTags) || m.ProtectedLocal, DeleteFiles: true}
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
		if m.ArrID == 0 && (!plexDeletable(m) || season != nil) {
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
	{actions.ErrProtectedLocal, "Protected in Cleanarr."},
	{actions.ErrConfirmMismatch, "The typed title does not match."},
	{actions.ErrNoSuchSeason, "Season not found."},
	{actions.ErrNotManaged, "Not managed by Sonarr/Radarr: cannot be protected."},
	{actions.ErrNoProtectedTag, "No protection tag configured (PROTECTED_TAGS)."},
	{actions.ErrUnconfigured, "Service not configured."},
	{actions.ErrPlexDeletionDenied, "Plex refused the deletion: allow it on the Plex server (Settings › Library › Allow media deletion)."},
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
		trigger := map[string]any{
			"cleanarr:toast": map[string]any{"message": msg, "level": level},
			"cleanarr:deleted": map[string]any{"mediaId": m.ID, "season": res.Season, "dryRun": res.DryRun,
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

// --- journal ------------------------------------------------------------------

const journalPageSize = 50

type journalData struct {
	Tab     string // actions | security
	Actions []store.Action
	Events  []store.AuthEvent
	Page    int
	PrevURL string
	NextURL string
}

// authEventLabels name the security events (English keys, translated by the templates).
var authEventLabels = map[string]string{ //nolint:gosec // G101: UI labels, not credentials
	"admin_created": "Admin account created", "login_success": "Sign-in", "login_failure": "Failed sign-in",
	"lockout": "Account locked", "password_changed": "Password changed",
	"password_reset_cli": "Password reset (CLI)", "logout": "Sign-out", "session_revoked": "Session revoked",
	"config_sonarr": "Sonarr configuration changed", "config_radarr": "Radarr configuration changed",
	"config_plex": "Plex configuration changed", "config_tautulli": "Tautulli configuration changed",
}

func (s *Server) journal(w http.ResponseWriter, r *http.Request) {
	tab := r.URL.Query().Get("tab")
	if !slices.Contains([]string{"actions", "security"}, tab) {
		tab = "actions"
	}
	page := max(atoi(r.URL.Query().Get("page")), 1)
	d := journalData{Tab: tab, Page: page}
	offset := (page - 1) * journalPageSize
	var more bool
	var err error
	if tab == "actions" {
		d.Actions, more, err = s.Store.Actions(r.Context(), offset, journalPageSize)
	} else {
		d.Events, more, err = s.Store.AuthEventsPage(r.Context(), offset, journalPageSize)
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if page > 1 {
		d.PrevURL = fmt.Sprintf("/journal?tab=%s&page=%d", tab, page-1)
	}
	if more {
		d.NextURL = fmt.Sprintf("/journal?tab=%s&page=%d", tab, page+1)
	}
	s.render(w, r, http.StatusOK, "journal", "", s.page(r, "Log", "journal", d))
}

// protectSubmit adds or removes the protection tag (« Protéger » / « Ne plus
// protéger » on the detail page).
func (s *Server) protectSubmit(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m, ok := s.mediaFromPath(w, r, kind)
		if !ok {
			return
		}
		if s.Actions == nil {
			http.Error(w, s.loc(r).T("Protection unavailable."), http.StatusServiceUnavailable)
			return
		}
		protect := r.PostFormValue("protect") == "1"
		detailURL := fmt.Sprintf("%s/%d", kindPath(kind), m.ID)
		res, err := s.Actions.SetProtected(r.Context(), actions.ProtectRequest{MediaID: m.ID, Protect: protect,
			Username: sessionFrom(r).Username})
		l := s.loc(r)
		var msg, level string
		switch {
		case err != nil && protect:
			msg, level = explain(l, err, "Protection failed: %s"), "error"
		case err != nil:
			msg, level = explain(l, err, "Unprotecting failed: %s"), "error"
		case res.Unchanged && protect:
			msg, level = l.T("“%s” is already protected.", res.Title), "info"
		case res.Unchanged:
			msg, level = l.T("“%s” is not protected.", res.Title), "info"
		case res.Local && protect:
			msg, level = l.T("“%s” is protected in Cleanarr: it can no longer be deleted. Plex is not changed.", res.Title), "success"
		case res.Local:
			msg, level = l.T("“%s” is no longer protected.", res.Title), "success"
		case res.DryRun && protect:
			msg, level = l.T("Simulation: “%s” would have received the %s tag. Nothing changed.", res.Title, res.Tag), "info"
		case res.DryRun:
			msg, level = l.T("Simulation: the %s tag would have been removed from “%s”. Nothing changed.", res.Tag, res.Title), "info"
		case protect:
			msg, level = l.T("“%s” is protected (%s tag): it will no longer be proposed for cleanup.", res.Title, res.Tag), "success"
		default:
			msg, level = l.T("“%s” is no longer protected (%s tag removed).", res.Title, res.Tag), "success"
		}
		if !isHTMX(r) {
			if err != nil {
				http.Error(w, msg, http.StatusUnprocessableEntity)
				return
			}
			http.Redirect(w, r, detailURL, http.StatusSeeOther)
			return
		}
		trigger := map[string]any{"cleanarr:toast": map[string]any{"message": msg, "level": level}}
		// Unchanged too: the local tags were realigned with Sonarr/Radarr.
		if err == nil && (!res.DryRun || res.Unchanged) {
			trigger["cleanarr:changed"] = map[string]any{"detailUrl": detailURL}
		}
		w.Header().Set("HX-Trigger", asciiJSON(trigger))
		w.Header().Set("HX-Reswap", "none")
		w.WriteHeader(http.StatusOK)
	}
}
