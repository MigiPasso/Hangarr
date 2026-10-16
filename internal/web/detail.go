package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"

	"hangarr/internal/clients/sonarr"
	"hangarr/internal/media"
	"hangarr/internal/store"
	"hangarr/internal/upstream"
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
	UserID    int64
	Name      string // "" when the account is unknown (shown as "User 12")
	Seasons   []userSeason
	Watched   int    // episodes watched among those on disk, all seasons (series)
	Files     int    // episodes on disk, all seasons (series)
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
	ProtectTag string // first PROTECTED_TAGS entry, added by "Protect"
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
			a = &acc{p: userProgress{UserID: w.UserID, Name: w.UserName}, watched: map[epKey]bool{}}
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
			a.p.Last = media.EpisodeCode(a.lastKey.season, a.lastKey.episode)
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
			a.p.Watched += us.Watched
			a.p.Files += us.Files
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
					a.p.Next = media.EpisodeCode(k.season, k.episode)
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
	switch {
	case m.ArrID <= 0:
	case m.Kind == "series" && m.TitleSlug != "":
		out = append(out, link{"Sonarr", up.SonarrWebURL() + "/series/" + url.PathEscape(m.TitleSlug)})
	case m.Kind == "movie":
		// Radarr's slug is the TMDB id (v4+); fall back to it when missing.
		slug := m.TitleSlug
		if slug == "" && m.TmdbID > 0 {
			slug = strconv.FormatInt(m.TmdbID, 10)
		}
		if slug != "" {
			out = append(out, link{"Radarr", up.RadarrWebURL() + "/movie/" + url.PathEscape(slug)})
		}
	}
	if mid := s.Store.PlexMachineID(ctx); up.MediaServer == upstream.Plex && mid != "" && m.PlexRatingKey != "" {
		base := "https://app.plex.tv/desktop"
		if pub := up.PlexWebURL(); pub != "" {
			base = pub + "/web/index.html"
		}
		out = append(out, link{"Plex", base + "#!/server/" + url.PathEscape(mid) + "/details?key=" +
			url.QueryEscape("/library/metadata/"+m.PlexRatingKey)})
	}
	if up.MediaServer == upstream.Jellyfin && m.JellyfinID != "" {
		if serverID := s.Store.JellyfinServerID(ctx); serverID != "" {
			out = append(out, link{"Jellyfin", up.JellyfinWebURL() + "/web/#/details?id=" + url.QueryEscape(m.JellyfinID) +
				"&serverId=" + url.QueryEscape(serverID)})
		}
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
		up := s.Upstreams.Get()
		d := detailData{M: m, Path: kindPath(kind), Links: s.links(ctx, m),
			Protected: store.ProtectionOf(m.Tags, m.ProtectedLocal, s.Config.ProtectedTags) != store.Unprotected,
			Deletable: serverDeletable(up, m) || m.ArrID > 0}
		switch {
		case s.Actions == nil:
		case serverDeletable(up, m):
			d.CanProtect = true // in Hangarr only
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
