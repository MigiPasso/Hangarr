package web

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"arrstats/internal/store"
	"arrstats/internal/syncer"
)

// DefaultPageSize is the number of items per infinite-scroll batch.
const DefaultPageSize = 48

const viewCookie = "arrstats_view"

// SyncRunner is the part of the syncer used by the UI.
type SyncRunner interface {
	Status() syncer.Status
	Start(ctx context.Context) bool
	RequestHistorySync(ctx context.Context) bool
	DailyTime() syncer.ClockTime
	SetDailyTime(at syncer.ClockTime)
	NextRun() time.Time
}

var (
	watchStates = []string{"never", "in_progress", "completed", "abandoned"}
	lastOptions = []string{"never", "30", "90", "180", "365"}
	addOptions  = []string{"lt30", "gt180", "gt365"}
	matchStates = []string{"matched", "arr_only", "plex_only"}
	statuses    = []string{"continuing", "ended"}
)

func pick(v string, allowed []string) string {
	if slices.Contains(allowed, v) {
		return v
	}
	return ""
}

func pickAll(vs []string, allowed []string) []string {
	var out []string
	for _, v := range vs {
		if slices.Contains(allowed, v) && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

func nonEmpty(vs []string) []string {
	var out []string
	for _, v := range vs {
		if v = strings.TrimSpace(v); v != "" && len(v) <= 200 && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

func ids(vs []string) []int64 {
	var out []int64
	for _, v := range vs {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}

func atoi(v string) int {
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func atof(v string) float64 {
	f, err := strconv.ParseFloat(strings.ReplaceAll(v, ",", "."), 64)
	if err != nil || f < 0 {
		return 0
	}
	return f
}

// parseLibraryFilter reads filters from the query string. Unknown values are
// dropped, so URLs are shareable and safe to replay.
func parseLibraryFilter(q url.Values, kind string) (store.LibraryFilter, int) {
	f := store.LibraryFilter{
		Kind:       kind,
		Query:      strings.TrimSpace(q.Get("q")),
		States:     pickAll(q["state"], watchStates),
		LastViewed: pick(q.Get("last"), lastOptions),
		Added:      pick(q.Get("added"), addOptions),
		Genres:     nonEmpty(q["genre"]),
		YearMin:    atoi(q.Get("year_min")),
		YearMax:    atoi(q.Get("year_max")),
		RatingMin:  atof(q.Get("rating_min")),
		Tags:       nonEmpty(q["tag"]),
		Profile:    strings.TrimSpace(q.Get("profile")),
		Network:    strings.TrimSpace(q.Get("network")),
		SizeMinGB:  atof(q.Get("size_min")),
		SeenBy:     ids(q["seen_by"]),
		UnseenBy:   ids(q["unseen_by"]),
		Nobody:     q.Get("nobody") == "1",
		ViewersMin: atoi(q.Get("viewers_min")),
		Match:      pickAll(q["match"], matchStates),
		Sort:       q.Get("sort"),
	}
	if len(f.Query) > 200 {
		f.Query = f.Query[:200]
	}
	if kind == "series" {
		f.Status = pick(q.Get("status"), statuses)
	}
	if u := ids([]string{q.Get("user")}); len(u) == 1 {
		f.User = u[0]
	}
	if !store.ValidSort(f.Sort) {
		f.Sort = "title"
	}
	switch q.Get("dir") {
	case "asc":
		f.Desc = false
	case "desc":
		f.Desc = true
	default:
		f.Desc = f.Sort != "title" // newest/largest first, titles A→Z
	}
	page := max(atoi(q.Get("page")), 1)
	return f, page
}

// filterValues is the canonical query string of a filter (without page).
func filterValues(f store.LibraryFilter) url.Values {
	v := url.Values{}
	set := func(k, val string) {
		if val != "" {
			v.Set(k, val)
		}
	}
	num := func(k string, n int) {
		if n > 0 {
			v.Set(k, strconv.Itoa(n))
		}
	}
	flt := func(k string, x float64) {
		if x > 0 {
			v.Set(k, strconv.FormatFloat(x, 'f', -1, 64))
		}
	}
	set("q", f.Query)
	v["state"] = f.States
	set("last", f.LastViewed)
	set("added", f.Added)
	v["genre"] = f.Genres
	num("year_min", f.YearMin)
	num("year_max", f.YearMax)
	flt("rating_min", f.RatingMin)
	v["tag"] = f.Tags
	set("profile", f.Profile)
	set("network", f.Network)
	set("status", f.Status)
	flt("size_min", f.SizeMinGB)
	for _, u := range f.SeenBy {
		v.Add("seen_by", strconv.FormatInt(u, 10))
	}
	for _, u := range f.UnseenBy {
		v.Add("unseen_by", strconv.FormatInt(u, 10))
	}
	if f.Nobody {
		v.Set("nobody", "1")
	}
	num("viewers_min", f.ViewersMin)
	v["match"] = f.Match
	if f.User > 0 {
		v.Set("user", strconv.FormatInt(f.User, 10))
	}
	if f.Sort != "title" {
		v.Set("sort", f.Sort)
	}
	if f.Desc != (f.Sort != "title") {
		if f.Desc {
			v.Set("dir", "desc")
		} else {
			v.Set("dir", "asc")
		}
	}
	for k, vals := range v {
		if len(vals) == 0 {
			delete(v, k)
		}
	}
	return v
}

type libraryData struct {
	Kind      string // movie | series
	Path      string // /movies | /series
	Filter    store.LibraryFilter
	Values    url.Values
	Facets    *store.LibraryFacets
	Items     []store.LibraryItem
	Count     int
	SizeBytes int64
	NextURL   string
	View      string // grid | table
	ViewURLs  map[string]string
	UserName  string
	Active    bool // at least one filter set
}

func (s *Server) pageSize() int {
	if s.PageSize > 0 {
		return s.PageSize
	}
	return DefaultPageSize
}

func (s *Server) libraryView(w http.ResponseWriter, r *http.Request) string {
	view := r.URL.Query().Get("view")
	if view == "grid" || view == "table" {
		s.setCookie(w, viewCookie, view, "/", 365*24*3600)
		return view
	}
	if c, err := r.Cookie(viewCookie); err == nil && c.Value == "table" {
		return "table"
	}
	return "grid"
}

func (s *Server) library(kind, path, title string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		f, page := parseLibraryFilter(r.URL.Query(), kind)
		size := s.pageSize()
		f.Limit, f.Offset = size+1, (page-1)*size
		now := time.Now()

		items, err := s.Store.Library(ctx, f, now, s.Config.AbandonDays)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		values := filterValues(f)
		data := libraryData{Kind: kind, Path: path, Filter: f, Values: values, Items: items, View: s.libraryView(w, r)}
		if len(items) > size {
			data.Items = items[:size]
			next := filterValues(f)
			next.Set("page", strconv.Itoa(page+1))
			data.NextURL = path + "?" + next.Encode()
		}
		w.Header().Set("Vary", "HX-Request, HX-Target")

		// Infinite scroll: only the next batch of cards/rows.
		if page > 1 {
			s.render(w, r, http.StatusOK, "library", "library_items", s.page(r, title, kind, data))
			return
		}
		if data.Count, data.SizeBytes, err = s.Store.LibrarySummary(ctx, f, now, s.Config.AbandonDays); err != nil {
			s.serverError(w, r, err)
			return
		}
		active := filterValues(f)
		active.Del("sort")
		active.Del("dir")
		data.Active = len(active) > 0
		data.ViewURLs = map[string]string{}
		for _, v := range []string{"grid", "table"} {
			u := filterValues(f)
			u.Set("view", v)
			data.ViewURLs[v] = path + "?" + u.Encode()
		}
		if f.User > 0 {
			if accounts, err := s.Store.Accounts(ctx); err == nil {
				for _, a := range accounts {
					if a.ID == f.User {
						data.UserName = a.Name
					}
				}
			}
		}
		// Filter changes target #results; history restores need the full page.
		if isHTMX(r) && r.Header.Get("HX-Target") == "results" && r.Header.Get("HX-History-Restore-Request") != "true" {
			s.render(w, r, http.StatusOK, "library", "library_results", s.page(r, title, kind, data))
			return
		}
		if data.Facets, err = s.Store.Facets(ctx, kind); err != nil {
			s.serverError(w, r, err)
			return
		}
		s.render(w, r, http.StatusOK, "library", "", s.page(r, title, kind, data))
	}
}

// --- images ---------------------------------------------------------------------

func (s *Server) image(w http.ResponseWriter, r *http.Request) {
	kind, variant := r.PathValue("kind"), r.PathValue("variant")
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if s.Images == nil || err != nil || id <= 0 || (kind != "series" && kind != "movie") ||
		(variant != "poster" && variant != "fanart") {
		http.NotFound(w, r)
		return
	}
	s.Images.ServeImage(w, r, kind, id, variant)
}

// --- sync -----------------------------------------------------------------------

type syncView struct {
	Disabled bool // services not configured
	Running  bool
	Progress string
	LastAt   *int64
	Failed   []string
}

func (s *Server) syncStatus(ctx context.Context) *syncView {
	if s.Sync == nil {
		return nil
	}
	st := s.Sync.Status()
	v := &syncView{Running: st.Running, Progress: st.Progress, Disabled: !s.Upstreams.Get().Ready()}
	if st.Last != nil {
		end := st.Last.StartedAt.Add(st.Last.Duration).Unix()
		v.LastAt = &end
		for _, src := range st.Last.Sources {
			if src.Err != nil {
				v.Failed = append(v.Failed, src.Source)
			}
		}
	} else if last, err := s.Store.LastSyncAt(ctx); err == nil {
		v.LastAt = last
	}
	return v
}

func (s *Server) syncStart(w http.ResponseWriter, r *http.Request) {
	if s.Sync != nil {
		s.Sync.Start(s.baseContext())
	}
	if !isHTMX(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, "home", "sync_status", s.page(r, "", "", nil))
}

func (s *Server) syncStatusPartial(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "home", "sync_status", s.page(r, "", "", nil))
}
