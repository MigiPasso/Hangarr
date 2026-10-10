package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"hangarr/internal/actions"
	"hangarr/internal/clients/httpx"
	"hangarr/internal/clients/plex"
	"hangarr/internal/i18n"
	"hangarr/internal/release"
	"hangarr/internal/store"
	"hangarr/internal/upstream"
)

// duplicatesTimeout bounds the live reads of one group (tracks, Radarr).
const duplicatesTimeout = 8 * time.Second

// duplicatesData is the Duplicates page.
type duplicatesData struct {
	Tab      string // review (to review) or acked
	Review   int    // groups to review
	Acked    int    // acknowledged groups
	Groups   []duplicateCard
	Error    string
	Conflict bool // Plex and Jellyfin are both configured
	NoServer bool // no media server configured
}

// duplicateCard is a movie the media server has several times, with its versions.
type duplicateCard struct {
	Index       int
	GroupKey    string
	Title       string
	Year        int
	MediaID     int64  // 0: no media row, no link
	Poster      string // "" when there is no image
	Count       int
	Size        int64
	Protected   bool
	Acked       bool
	AckedUnix   int64
	Fingerprint string
	Tab         string
	CSRFToken   string
	AckURL      string
	UnackURL    string
	LiveURL     string // where the versions load from (page only)
	Live        bool   // the tracks were asked for (fragment)
	Failed      bool   // the tracks could not be read
	NoteID      string // id of the note on versions Jellyfin deletes together; "" when there are none
	Versions    []duplicateVersion
}

// duplicateVersion is a file of a duplicate. The tracks and the Radarr fields
// are filled by the live read only.
type duplicateVersion struct {
	File        string
	Name        string // base name of the file
	Quality     string
	Resolution  string
	Video       string
	Audio       string
	Size        int64
	DeleteURL   string
	CanDelete   bool
	WholeFolder bool // Jellyfin would delete the whole folder, other versions too
	AudioTracks []string
	Subtitles   []string
	Radarr      bool // Radarr's file
	Monitored   bool // Radarr monitors the movie (Radarr's file only)
}

// liveTracks are the audio and subtitle tracks of one version, as the media server lists them.
type liveTracks struct{ audio, subtitles []string }

// radarrFileInfo is the file Radarr has for a movie, and whether Radarr monitors the movie.
type radarrFileInfo struct {
	name      string // base name
	monitored bool
}

// versionDeleteData is the form that deletes one version of a duplicate.
type versionDeleteData struct {
	Title       string
	Name        string
	File        string
	Quality     string
	Resolution  string
	Size        int64
	ActionURL   string
	Fingerprint string
	Protected   bool
	NeedRadarr  bool // Radarr would show the movie as missing: the user must confirm
	Error       string
	Done        bool   // the deletion was attempted: its result, no form
	Simulated   bool   // dry run
	Message     string // result of a successful deletion
}

// duplicateServer is the media server the duplicates are read from: "" when
// none is configured or when Plex and Jellyfin both are.
func duplicateServer(up *upstream.Clients) string {
	switch {
	case up.ServerConflict:
		return ""
	case up.MediaServer == upstream.Plex && up.Plex != nil:
		return store.ServerPlex
	case up.MediaServer == upstream.Jellyfin && up.Jellyfin != nil:
		return store.ServerJellyfin
	}
	return ""
}

// duplicateServerOrFail returns the services snapshot of the request and the
// media server in use, or writes why there is none. The handler uses only this
// snapshot: the configuration may change between two reads.
func (s *Server) duplicateServerOrFail(w http.ResponseWriter, r *http.Request) (*upstream.Clients, string, bool) {
	up := s.Upstreams.Get()
	if server := duplicateServer(up); server != "" {
		return up, server, true
	}
	if up.ServerConflict {
		http.Error(w, s.loc(r).T("Plex and Jellyfin are both configured: clear one of them first."), http.StatusUnprocessableEntity)
	} else {
		http.Error(w, s.loc(r).T("No media server is configured."), http.StatusUnprocessableEntity)
	}
	return up, "", false
}

func duplicateTab(v string) string {
	if v == "acked" {
		return "acked"
	}
	return "review"
}

func duplicatesURL(tab string) string {
	if tab == "acked" {
		return "/duplicates?tab=acked"
	}
	return "/duplicates"
}

func dupURL(groupKey string) string {
	return "/duplicates/" + url.PathEscape(groupKey)
}

func versionURL(groupKey, versionKey string) string {
	return dupURL(groupKey) + "/versions/" + url.PathEscape(versionKey) + "/delete"
}

// duplicates is the Duplicates page: the movies the media server has several times.
func (s *Server) duplicates(w http.ResponseWriter, r *http.Request) {
	up := s.Upstreams.Get()
	s.renderDuplicates(w, r, up, http.StatusOK, duplicateTab(r.URL.Query().Get("tab")), "")
}

// renderDuplicates writes the Duplicates page from the services snapshot up, with
// errMsg above the list when set.
func (s *Server) renderDuplicates(w http.ResponseWriter, r *http.Request, up *upstream.Clients, status int, tab, errMsg string) {
	d := duplicatesData{Tab: tab, Error: errMsg, Conflict: up.ServerConflict}
	if server := duplicateServer(up); server == "" {
		d.NoServer = !up.ServerConflict
	} else {
		groups, err := s.Store.DuplicateGroups(r.Context(), server, s.Config.ProtectedTags)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		csrf := ""
		if sess := sessionFrom(r); sess != nil {
			csrf = sess.CSRFToken
		}
		for i := range groups {
			g := &groups[i]
			if g.AckedAt == nil {
				d.Review++
			} else {
				d.Acked++
			}
			if (g.AckedAt != nil) != (tab == "acked") {
				continue
			}
			c, err := s.newDuplicateCard(r, server, g, len(d.Groups)+1, tab, csrf)
			if err != nil {
				s.serverError(w, r, err)
				return
			}
			d.Groups = append(d.Groups, c)
		}
	}
	s.render(w, r, status, "duplicates", "", s.page(r, "Duplicates", "duplicates", d))
}

// newDuplicateCard builds a card from the stored data; the tracks are read later.
func (s *Server) newDuplicateCard(r *http.Request, server string, g *store.DuplicateGroup, index int, tab, csrf string) (duplicateCard, error) {
	c := duplicateCard{Index: index, GroupKey: g.GroupKey, Title: g.Title, Year: g.Year, MediaID: g.MediaID,
		Count: len(g.Versions), Size: g.SizeBytes, Protected: g.Protected, Fingerprint: g.Fingerprint, Tab: tab,
		CSRFToken: csrf, AckURL: dupURL(g.GroupKey) + "/ack", UnackURL: dupURL(g.GroupKey) + "/unack",
		LiveURL: dupURL(g.GroupKey) + "/live", Versions: duplicateVersions(s.loc(r), server, g)}
	c.NoteID = folderNoteID(c.GroupKey, c.Versions)
	if g.AckedAt != nil {
		c.Acked, c.AckedUnix = true, g.AckedAt.Unix()
	}
	if g.MediaID != 0 {
		m, err := s.Store.MediaByID(r.Context(), g.MediaID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			c.MediaID = 0
		case err != nil:
			return c, err
		default:
			c.Poster = imgURL("movie", m.ArrID, m.PlexRatingKey, m.JellyfinID, "poster")
		}
	}
	return c, nil
}

// duplicateVersions lists the versions of a group, in the order of the group, from the stored data.
func duplicateVersions(l i18n.Localizer, server string, g *store.DuplicateGroup) []duplicateVersion {
	out := make([]duplicateVersion, 0, len(g.Versions))
	for _, v := range g.Versions {
		shared := server == store.ServerJellyfin && sharesWithOther(g.Versions, v)
		out = append(out, duplicateVersion{
			File: v.File, Name: fileBase(v.File), Quality: qualityLabel(l, v.File),
			Resolution: resolutionLabel(v.Resolution), Video: orDash(strings.ToUpper(v.VideoCodec)),
			Audio: orDash(strings.ToUpper(v.AudioCodec)), Size: v.SizeBytes,
			DeleteURL: versionURL(g.GroupKey, v.VersionKey), CanDelete: !g.Protected && !shared,
			WholeFolder: shared && !g.Protected,
		})
	}
	return out
}

// sharesWithOther reports whether Jellyfin, deleting v, would take another
// version with it: the same item, or a file in the same folder.
func sharesWithOther(versions []store.DuplicateVersion, v store.DuplicateVersion) bool {
	for _, o := range versions {
		if o.VersionKey != v.VersionKey && (o.ItemKey == v.ItemKey || fileDir(o.File) == fileDir(v.File)) {
			return true
		}
	}
	return false
}

// folderNoteID is the id of the note saying that Jellyfin would delete the
// versions of a group together; "" when no version is in that case. The id
// comes from the group key, so it is unique on the page.
func folderNoteID(groupKey string, versions []duplicateVersion) string {
	for _, v := range versions {
		if v.WholeFolder {
			return "dup-folder-" + strings.Map(htmlIDRune, groupKey)
		}
	}
	return ""
}

// htmlIDRune keeps the characters of a group key that are safe in an HTML id,
// and turns the others into dashes.
func htmlIDRune(r rune) rune {
	if r == '-' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
		return r
	}
	return '-'
}

// duplicatesLive is the versions table of one group, with the tracks and
// Radarr's status read live: the page loads it by HTMX once it is shown.
func (s *Server) duplicatesLive(w http.ResponseWriter, r *http.Request) {
	up, server, ok := s.duplicateServerOrFail(w, r)
	if !ok {
		return
	}
	g, err := s.Store.DuplicateGroup(r.Context(), server, r.PathValue("group"), s.Config.ProtectedTags)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	l := s.loc(r)
	c := duplicateCard{GroupKey: g.GroupKey, Live: true, Versions: duplicateVersions(l, server, g)}
	c.NoteID = folderNoteID(c.GroupKey, c.Versions)
	tracks, rf, ok := s.readLive(r.Context(), l, up, server, g)
	c.Failed = !ok
	for i := range c.Versions {
		v := &c.Versions[i]
		if lt, found := tracks[g.Versions[i].VersionKey]; found {
			v.AudioTracks, v.Subtitles = lt.audio, lt.subtitles
		}
		if rf != nil && strings.EqualFold(v.Name, rf.name) {
			v.Radarr, v.Monitored = true, rf.monitored
		}
	}
	s.render(w, r, http.StatusOK, "duplicates", "duplicate_fragment", s.pageRaw(r, "", "duplicates", c))
}

// readLive reads, within duplicatesTimeout, the tracks of each version from the
// media server and Radarr's file of the movie. ok is false when the tracks
// could not be read; the Radarr file is returned regardless.
func (s *Server) readLive(ctx context.Context, l i18n.Localizer, up *upstream.Clients, server string,
	g *store.DuplicateGroup) (tracks map[string]liveTracks, rf *radarrFileInfo, ok bool) {
	ctx, cancel := context.WithTimeout(ctx, duplicatesTimeout)
	defer cancel()
	var wg sync.WaitGroup
	if up.Radarr != nil && g.ArrID != 0 {
		wg.Go(func() { rf = s.readRadarrFile(ctx, up, g.ArrID) })
	}
	tracks, err := s.serverTracks(ctx, l, up, server, g)
	wg.Wait()
	if err != nil {
		s.Log.Warn("duplicate tracks unavailable", "server", server, "group", g.GroupKey, "err", err)
		return nil, rf, false
	}
	return tracks, rf, true
}

// readRadarrFile reads Radarr's file of a movie; nil when Radarr has none or cannot be asked.
func (s *Server) readRadarrFile(ctx context.Context, up *upstream.Clients, arrID int64) *radarrFileInfo {
	mv, err := up.Radarr.MovieByID(ctx, arrID)
	if err != nil {
		s.Log.Warn("duplicate: Radarr movie unavailable", "movie", arrID, "err", err)
		return nil
	}
	if mv.MovieFile == nil || mv.MovieFile.RelativePath == "" {
		return nil
	}
	return &radarrFileInfo{name: fileBase(mv.MovieFile.RelativePath), monitored: mv.Monitored}
}

// serverTracks reads the tracks of the versions of a group from the media
// server. A movie the server no longer has has none.
func (s *Server) serverTracks(ctx context.Context, l i18n.Localizer, up *upstream.Clients, server string,
	g *store.DuplicateGroup) (map[string]liveTracks, error) {
	var items []string
	seen := map[string]bool{}
	for _, v := range g.Versions {
		if !seen[v.ItemKey] {
			seen[v.ItemKey] = true
			items = append(items, v.ItemKey)
		}
	}
	out := map[string]liveTracks{}
	if server == store.ServerJellyfin {
		got, err := up.Jellyfin.Versions(ctx, items)
		if err != nil {
			return nil, err
		}
		for _, it := range got {
			for _, src := range it.MediaSources {
				var lt liveTracks
				for _, st := range src.MediaStreams {
					switch st.Type {
					case "Audio":
						lt.audio = append(lt.audio, st.DisplayTitle)
					case "Subtitle":
						lt.subtitles = append(lt.subtitles, trackLabel(l, st.DisplayTitle, st.IsForced))
					}
				}
				out[src.ID] = lt
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
			if len(m.Part) == 0 {
				continue
			}
			var lt liveTracks
			for _, st := range m.Part[0].Stream {
				switch st.StreamType {
				case plex.StreamAudio:
					lt.audio = append(lt.audio, st.DisplayTitle)
				case plex.StreamSubtitle:
					lt.subtitles = append(lt.subtitles, trackLabel(l, st.DisplayTitle, st.Forced))
				}
			}
			out[fmt.Sprint(int64(m.ID))] = lt
		}
	}
	return out, nil
}

// duplicateAck keeps a duplicate on purpose, for the versions the page showed.
func (s *Server) duplicateAck(w http.ResponseWriter, r *http.Request) {
	up, server, ok := s.duplicateServerOrFail(w, r)
	if !ok {
		return
	}
	tab := duplicateTab(r.PostFormValue("tab"))
	err := s.Store.AckDuplicate(r.Context(), server, r.PathValue("group"), r.PostFormValue("fingerprint"), time.Now())
	switch {
	case errors.Is(err, store.ErrStaleDuplicate):
		s.renderDuplicates(w, r, up, http.StatusConflict, tab,
			s.loc(r).T("The versions changed since this page was loaded: check them again."))
	case errors.Is(err, store.ErrNotFound):
		s.notFound(w, r)
	case err != nil:
		s.serverError(w, r, err)
	default:
		redirect(w, r, duplicatesURL(tab))
	}
}

// duplicateUnack withdraws the acknowledgement of a duplicate.
func (s *Server) duplicateUnack(w http.ResponseWriter, r *http.Request) {
	_, server, ok := s.duplicateServerOrFail(w, r)
	if !ok {
		return
	}
	if err := s.Store.UnackDuplicate(r.Context(), server, r.PathValue("group")); err != nil {
		s.serverError(w, r, err)
		return
	}
	redirect(w, r, duplicatesURL(duplicateTab(r.PostFormValue("tab"))))
}

// duplicateVersionOf loads a group and one of its versions; ok is false after
// a 404 or a 500 has been written.
func (s *Server) duplicateVersionOf(w http.ResponseWriter, r *http.Request, server string) (*store.DuplicateGroup, *store.DuplicateVersion, bool) {
	g, err := s.Store.DuplicateGroup(r.Context(), server, r.PathValue("group"), s.Config.ProtectedTags)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return nil, nil, false
	}
	if err != nil {
		s.serverError(w, r, err)
		return nil, nil, false
	}
	for i := range g.Versions {
		if g.Versions[i].VersionKey == r.PathValue("version") {
			return g, &g.Versions[i], true
		}
	}
	s.notFound(w, r)
	return nil, nil, false
}

// versionDeletePage is the confirmation of the deletion of one version.
func (s *Server) versionDeletePage(w http.ResponseWriter, r *http.Request) {
	_, server, ok := s.duplicateServerOrFail(w, r)
	if !ok {
		return
	}
	g, v, ok := s.duplicateVersionOf(w, r, server)
	if !ok {
		return
	}
	s.renderVersionDelete(w, r, http.StatusOK, newVersionDelete(s.loc(r), g, v))
}

// versionDeleteSubmit deletes one version (or simulates it). DeleteVersion
// checks everything on live data; the page only shows the outcome.
func (s *Server) versionDeleteSubmit(w http.ResponseWriter, r *http.Request) {
	_, server, ok := s.duplicateServerOrFail(w, r)
	if !ok {
		return
	}
	g, v, ok := s.duplicateVersionOf(w, r, server)
	if !ok {
		return
	}
	l := s.loc(r)
	d := newVersionDelete(l, g, v)
	if s.Actions == nil {
		d.Error = l.T("Deletion unavailable.")
		s.renderVersionDelete(w, r, http.StatusUnprocessableEntity, d)
		return
	}
	res, err := s.Actions.DeleteVersion(r.Context(), actions.VersionRequest{
		GroupKey: g.GroupKey, VersionKey: v.VersionKey, Fingerprint: r.PostFormValue("fingerprint"),
		AcceptRadarr: r.PostFormValue("accept_radarr") == "1", Username: sessionFrom(r).Username,
	})
	status := http.StatusUnprocessableEntity
	switch {
	case err == nil:
		status = http.StatusOK
		d.Done, d.Simulated = true, res.DryRun
		d.Message = l.T("Version deleted.")
		if res.DryRun {
			d.Message = l.T("Simulated: nothing was deleted.")
		}
	case errors.Is(err, actions.ErrRadarrConfirm):
		d.NeedRadarr = true
	case errors.Is(err, actions.ErrStaleVersions), errors.Is(err, actions.ErrNotFound):
		d.Done, d.Error = true, versionError(l, err)
	case errors.Is(err, actions.ErrProtected):
		d.Protected = true
	default:
		d.Error = versionError(l, err)
	}
	s.renderVersionDelete(w, r, status, d)
}

func newVersionDelete(l i18n.Localizer, g *store.DuplicateGroup, v *store.DuplicateVersion) versionDeleteData {
	return versionDeleteData{Title: g.Title, Name: fileBase(v.File), File: v.File, Quality: qualityLabel(l, v.File),
		Resolution: resolutionLabel(v.Resolution), Size: v.SizeBytes, ActionURL: versionURL(g.GroupKey, v.VersionKey),
		Fingerprint: g.Fingerprint, Protected: g.Protected}
}

// versionError explains a version deletion error in the user's language.
func versionError(l i18n.Localizer, err error) string {
	switch {
	case errors.Is(err, actions.ErrStaleVersions):
		return l.T("The versions changed since this page was loaded: check them again.")
	case errors.Is(err, actions.ErrLastVersion):
		return l.T("Only one version is left: this page does not delete movies.")
	case errors.Is(err, actions.ErrSameFolder):
		return l.T("Another version is in the same folder: Jellyfin would delete it too. Delete the file on disk or through Radarr.")
	case errors.Is(err, actions.ErrRadarrMonitored):
		return l.T("This is Radarr's file and Radarr monitors the movie: it would download it again. Delete the other version, or import the one you prefer into Radarr.")
	case errors.Is(err, actions.ErrRadarrFileUnknown):
		return l.T("Radarr monitors this movie and its file is none of these versions: deleting one could make it download the movie again.")
	case errors.Is(err, actions.ErrProtected):
		return l.T("This movie is protected: remove its protection, delete the version, then protect it again.")
	}
	return explain(l, err, "Deletion failed: %s")
}

// renderVersionDelete writes the deletion form: a full page, or its partial for HTMX.
func (s *Server) renderVersionDelete(w http.ResponseWriter, r *http.Request, status int, d versionDeleteData) {
	pd := s.pageRaw(r, s.loc(r).T("Delete %s", d.Title), "duplicates", d)
	if isHTMX(r) {
		s.render(w, r, http.StatusOK, "version_delete", "version_delete_form", pd)
		return
	}
	s.render(w, r, status, "version_delete", "", pd)
}

// resolutionLabel names a resolution of the store ("4k", "1080"…) as shown.
func resolutionLabel(key string) string {
	switch key {
	case "4k":
		return "4K"
	case "1080":
		return "1080p"
	case "720":
		return "720p"
	case "480":
		return "480p"
	case "sd":
		return "SD"
	case "":
		return "—"
	}
	return key
}

// qualityLabel is the source named by a file name ("Remux", "WEB-DL"…), with
// "light" for a re-encoded release.
func qualityLabel(l i18n.Localizer, file string) string {
	info := release.Parse(file)
	var parts []string
	if info.Source != release.Unknown {
		parts = append(parts, string(info.Source))
	}
	if info.Light {
		parts = append(parts, l.T("light"))
	}
	return orDash(strings.Join(parts, " "))
}

// trackLabel is the title of a subtitle track, with "forced" added when the title does not say it.
func trackLabel(l i18n.Localizer, title string, forced bool) string {
	lower := strings.ToLower(title)
	if !forced || strings.Contains(lower, "forced") || strings.Contains(lower, strings.ToLower(l.T("forced"))) {
		return title
	}
	return title + " (" + l.T("forced") + ")"
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// fileBase is the file name of a path written with / or \.
func fileBase(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

// fileDir is the folder of a path written with / or \ ("" when there is none).
func fileDir(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return path.Clean(strings.ReplaceAll(p[:i], `\`, "/"))
	}
	return ""
}

// isNotFound reports a 404 from an upstream service.
func isNotFound(err error) bool {
	var se *httpx.StatusError
	return errors.As(err, &se) && se.Status == http.StatusNotFound
}
