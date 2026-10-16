package web

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"hangarr/internal/media"
	"hangarr/internal/store"
)

const day = 24 * time.Hour

// --- shared helpers ---------------------------------------------------------

func (s *Server) cleanupScope(ctx context.Context, now time.Time) (store.CleanupScope, int) {
	days := s.cleanupDays(ctx)
	return store.CleanupScope{
		ProtectedTags: s.Config.ProtectedTags,
		Cutoff:        now.Add(-time.Duration(days) * day).Unix(),
	}, days
}

func startOfDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

// --- disk space (cached) ----------------------------------------------------------

type diskRow struct {
	Path       string
	FreeBytes  int64
	TotalBytes int64
	UsedPct    int
}

type diskCache struct {
	mu   sync.Mutex
	at   time.Time
	rows []diskRow
}

const diskCacheTTL = time.Minute

// diskSpace merges the disks reported by Sonarr and Radarr (deduplicated by
// path), cached for a minute so the dashboard does not hit the *arr apps on
// every view.
func (s *Server) diskSpace(ctx context.Context) []diskRow {
	s.disks.mu.Lock()
	defer s.disks.mu.Unlock()
	if time.Since(s.disks.at) < diskCacheTTL {
		return s.disks.rows
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	seen := map[string]bool{}
	var rows []diskRow
	add := func(path string, free, total int64) {
		if seen[path] || total <= 0 {
			return
		}
		seen[path] = true
		rows = append(rows, diskRow{Path: path, FreeBytes: free, TotalBytes: total, UsedPct: int((total - free) * 100 / total)})
	}
	up := s.Upstreams.Get()
	if up.Sonarr != nil {
		if ds, err := up.Sonarr.DiskSpace(ctx); err == nil {
			for _, d := range ds {
				add(d.Path, int64(d.FreeSpace), int64(d.TotalSpace))
			}
		}
	}
	if up.Radarr != nil {
		if ds, err := up.Radarr.DiskSpace(ctx); err == nil {
			for _, d := range ds {
				add(d.Path, int64(d.FreeSpace), int64(d.TotalSpace))
			}
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].TotalBytes > rows[j].TotalBytes })
	s.disks.at, s.disks.rows = time.Now(), rows
	return rows
}

// --- dashboard --------------------------------------------------------------------

type dayPoint struct {
	Date  string
	Hours string
}

type dashboardData struct {
	Overview    *store.Overview
	CleanupDays int
	// NoDurations: the history comes from Plex or Jellyfin alone, which
	// record no durations.
	NoDurations  bool
	TopAll       bool
	TopSeries    []store.TopItem
	TopMovies    []store.TopItem
	Recent       []store.RecentView
	Activity     template.HTML
	ActivityRows []dayPoint
	Disks        []diskRow
}

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := time.Now().In(s.Config.Location)
	d := dashboardData{TopAll: r.URL.Query().Get("top") == "all"}
	since := now.Add(-30 * day).Unix()
	topSince := since
	if d.TopAll {
		topSince = 0
	}
	var err error
	if d.TopSeries, err = s.Store.TopMedia(ctx, "series", topSince, 0, false, 10); err != nil {
		s.serverError(w, r, err)
		return
	}
	if d.TopMovies, err = s.Store.TopMedia(ctx, "movie", topSince, 0, false, 10); err != nil {
		s.serverError(w, r, err)
		return
	}
	pd := s.page(r, "Dashboard", "dashboard", &d)
	if isHTMX(r) && r.Header.Get("HX-Target") == "top" {
		s.render(w, r, http.StatusOK, "home", "dashboard_top", pd)
		return
	}
	scope, days := s.cleanupScope(ctx, now)
	d.CleanupDays = days
	d.NoDurations = s.Upstreams.Get().NoDurations()
	if d.Overview, err = s.Store.Overview(ctx, scope, since); err != nil {
		s.serverError(w, r, err)
		return
	}
	if d.Recent, err = s.Store.RecentViews(ctx, 10); err != nil {
		s.serverError(w, r, err)
		return
	}
	first := startOfDay(now).AddDate(0, 0, -29)
	sessions, err := s.Store.Sessions(ctx, first.Unix(), 0)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	perDay := make([]int64, 30)
	for _, x := range sessions {
		i := int(startOfDay(time.Unix(x.StoppedAt, 0).In(s.Config.Location)).Sub(first).Hours() / 24)
		if i >= 0 && i < 30 {
			perDay[i] += x.PlaySeconds
		}
	}
	l := s.loc(r)
	bars := make([]bar, 30)
	for i, secs := range perDay {
		date := first.AddDate(0, 0, i)
		bars[i] = bar{Label: l.DayMonth(date), Tick: i%7 == 1 || i == 29, Value: float64(secs) / 3600,
			Tip: l.DayMonth(date) + " · " + l.Hours(secs)}
		d.ActivityRows = append(d.ActivityRows, dayPoint{Date: l.Date(date), Hours: l.Hours(secs)})
	}
	d.Activity = barChart(l, bars, " h", l.T("Hours watched per day over 30 days"))
	d.Disks = s.diskSpace(ctx)
	s.render(w, r, http.StatusOK, "home", "", pd)
}

// --- statistics ---------------------------------------------------------------------

// periods are the stats windows (labels translated by the template).
var periods = []pair{{"7", "7 days"}, {"30", "30 days"}, {"90", "90 days"}, {"365", "12 months"}, {"all", "All time"}}

type resumeItem struct {
	store.LibraryItem
	Last string
	Next string
}

type resumeGroup struct {
	User  string
	Items []resumeItem
}

type statsData struct {
	Period      string
	PeriodLabel string
	User        int64
	UserName    string
	ByHours     bool
	Users       []store.Account
	Periods     []pair
	Hours       string
	Plays       int
	Titles      int
	TopSeries   []store.TopItem
	TopMovies   []store.TopItem
	Heatmap     template.HTML
	HeatRows    [][]string
	Genres      template.HTML
	GenreRows   []store.GenreShare
	Cost        []store.CostItem
	Ranking     []store.UserRank
	Resume      []resumeGroup
	Abandoned   []store.LibraryItem
	WhoWatches  []store.SeriesViewers
	Completion  *store.Completion
	CompletePct int
	Transcode   []store.Count
	Platforms   []store.Count
	Players     []store.Count
}

func parsePeriod(v string, now time.Time) (string, string, int64) {
	for _, p := range periods {
		if p.V == v {
			if v == "all" {
				return v, p.L, 0
			}
			n, _ := strconv.Atoi(v)
			return v, p.L, now.Add(-time.Duration(n) * day).Unix()
		}
	}
	return "30", "30 days", now.Add(-30 * day).Unix()
}

// nextEpisode approximates the next episode from the last one watched and
// stored season sizes (the detail page has the exact value).
func nextEpisode(last [2]int, totals map[int]int) string {
	season, ep := last[0], last[1]
	if ep < totals[season] {
		return media.EpisodeCode(season, ep+1)
	}
	var next []int
	for n, total := range totals {
		if n > season && total > 0 {
			next = append(next, n)
		}
	}
	if len(next) == 0 {
		return ""
	}
	slices.Sort(next)
	return media.EpisodeCode(next[0], 1)
}

func (s *Server) resume(ctx context.Context, users []store.Account, now time.Time) ([]resumeGroup, error) {
	var out []resumeGroup
	for _, u := range users {
		items, err := s.Store.Library(ctx, store.LibraryFilter{Kind: "series", User: u.ID, States: []string{"in_progress"},
			Sort: "last", Desc: true, Limit: 8}, now, s.Config.AbandonDays)
		if err != nil || len(items) == 0 {
			if err != nil {
				return nil, err
			}
			continue
		}
		last, err := s.Store.LastEpisodes(ctx, u.ID)
		if err != nil {
			return nil, err
		}
		ids := make([]int64, 0, len(items))
		for _, it := range items {
			ids = append(ids, it.ID)
		}
		totals, err := s.Store.SeasonTotals(ctx, ids)
		if err != nil {
			return nil, err
		}
		g := resumeGroup{User: u.Name}
		for _, it := range items {
			ri := resumeItem{LibraryItem: it}
			if l, ok := last[it.ID]; ok {
				ri.Last = media.EpisodeCode(l[0], l[1])
				ri.Next = nextEpisode(l, totals[it.ID])
			}
			g.Items = append(g.Items, ri)
		}
		out = append(out, g)
	}
	return out, nil
}

func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := time.Now().In(s.Config.Location)
	q := r.URL.Query()
	d := statsData{Periods: periods, ByHours: q.Get("by") == "hours"}
	var since int64
	d.Period, d.PeriodLabel, since = parsePeriod(q.Get("p"), now)
	var err error
	if d.Users, err = s.Store.Accounts(ctx); err != nil {
		s.serverError(w, r, err)
		return
	}
	if u := ids([]string{q.Get("user")}); len(u) == 1 {
		for _, a := range d.Users {
			if a.ID == u[0] {
				d.User, d.UserName = a.ID, a.Name
			}
		}
	}
	fail := func(err error) bool {
		if err != nil {
			s.serverError(w, r, err)
			return true
		}
		return false
	}

	sessions, err := s.Store.Sessions(ctx, since, d.User)
	if fail(err) {
		return
	}
	var grid [7][24]heatCell
	var secs int64
	for _, x := range sessions {
		secs += x.PlaySeconds
		t := time.Unix(x.StartedAt, 0).In(s.Config.Location)
		wd := (int(t.Weekday()) + 6) % 7 // Monday first
		grid[wd][t.Hour()].Count++
	}
	l := s.loc(r)
	for wd := range 7 {
		name := l.T(weekdayKeys[wd])
		row := []string{name}
		for h := range 24 {
			c := &grid[wd][h]
			c.Tip = fmt.Sprintf("%s %d–%d h · %s", name, h, h+1, l.N(int64(c.Count), "%d session", "%d sessions"))
			row = append(row, strconv.Itoa(c.Count))
		}
		d.HeatRows = append(d.HeatRows, row)
	}
	d.Heatmap = heatmap(l, grid, l.T("Play sessions by weekday and hour"))
	d.Hours = l.Hours(secs)

	ranking, err := s.Store.UserRanking(ctx, since)
	if fail(err) {
		return
	}
	for _, u := range ranking {
		if d.User == 0 || u.UserID == d.User {
			d.Plays += u.Plays
			d.Titles += u.Titles
		}
	}
	// Viewer-wide sections (ranking, cost per play, who watches what) only
	// make sense for everyone: hidden for a single viewer.
	if d.User == 0 {
		d.Ranking = ranking
	}
	if d.TopSeries, err = s.Store.TopMedia(ctx, "series", since, d.User, d.ByHours, 10); fail(err) {
		return
	}
	if d.TopMovies, err = s.Store.TopMedia(ctx, "movie", since, d.User, d.ByHours, 10); fail(err) {
		return
	}
	genres, err := s.Store.GenreShares(ctx, since, d.User)
	if fail(err) {
		return
	}
	sort.Slice(genres, func(i, j int) bool { return genres[i].StoredShare > genres[j].StoredShare })
	if len(genres) > 10 {
		genres = genres[:10]
	}
	d.GenreRows = genres
	rows := make([]pairRow, 0, len(genres))
	for _, g := range genres {
		label := g.Genre
		if label == store.NoGenre {
			label = l.T(label)
		}
		rows = append(rows, pairRow{Label: label, A: g.StoredShare, B: g.PlaysShare})
	}
	playsName := l.T("Plays")
	if d.UserName != "" {
		playsName = l.T("Plays by %s", d.UserName)
	}
	d.Genres = pairedBars(l, rows, l.T("Stored space"), playsName, l.T("Share of stored space and plays per genre"))
	if d.User == 0 {
		if d.Cost, err = s.Store.CostPerPlay(ctx, 15); fail(err) {
			return
		}
	}
	resumeUsers := d.Users
	if d.User != 0 {
		resumeUsers = []store.Account{{ID: d.User, Name: d.UserName}}
	}
	if d.Resume, err = s.resume(ctx, resumeUsers, now); fail(err) {
		return
	}
	for _, kind := range []string{"series", "movie"} {
		items, err := s.Store.Library(ctx, store.LibraryFilter{Kind: kind, User: d.User, States: []string{"abandoned"},
			Sort: "last", Desc: true, Limit: 10}, now, s.Config.AbandonDays)
		if fail(err) {
			return
		}
		d.Abandoned = append(d.Abandoned, items...)
	}
	if d.User == 0 {
		if d.WhoWatches, err = s.Store.WhoWatchesWhat(ctx, 10); fail(err) {
			return
		}
	}
	if d.Completion, err = s.Store.MovieCompletion(ctx, since, d.User, 10); fail(err) {
		return
	}
	if d.Completion.Started > 0 {
		d.CompletePct = d.Completion.Completed * 100 / d.Completion.Started
	}
	if d.Transcode, err = s.Store.Breakdown(ctx, "transcode_decision", since, d.User, 5); fail(err) {
		return
	}
	if d.Platforms, err = s.Store.Breakdown(ctx, "platform", since, d.User, 8); fail(err) {
		return
	}
	if d.Players, err = s.Store.Breakdown(ctx, "player", since, d.User, 8); fail(err) {
		return
	}
	s.render(w, r, http.StatusOK, "stats", "", s.page(r, "Statistics", "stats", d))
}

// --- per-user page --------------------------------------------------------------------

type monthPoint struct {
	Month string
	Hours string
}

type userStatsData struct {
	Name       string
	Hours      template.HTML
	MonthRows  []monthPoint
	TotalHours string
	TopSeries  []store.TopItem
	TopMovies  []store.TopItem
	Genres     []store.GenreShare
	InProgress []resumeItem
	Abandoned  []store.LibraryItem
	Players    []store.Count
	Platforms  []store.Count
}

// monthKeys are the short month names (translated).
var monthKeys = []string{"Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"}

func (s *Server) userStats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r)
		return
	}
	accounts, err := s.Store.Accounts(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d := userStatsData{}
	for _, a := range accounts {
		if a.ID == id {
			d.Name = a.Name
		}
	}
	if d.Name == "" {
		s.notFound(w, r)
		return
	}
	now := time.Now().In(s.Config.Location)
	firstMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()).AddDate(0, -11, 0)
	sessions, err := s.Store.Sessions(ctx, firstMonth.Unix(), id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	perMonth := make([]int64, 12)
	var total int64
	for _, x := range sessions {
		t := time.Unix(x.StoppedAt, 0).In(s.Config.Location)
		i := (t.Year()-firstMonth.Year())*12 + int(t.Month()-firstMonth.Month())
		if i >= 0 && i < 12 {
			perMonth[i] += x.PlaySeconds
			total += x.PlaySeconds
		}
	}
	l := s.loc(r)
	bars := make([]bar, 12)
	for i, secs := range perMonth {
		m := firstMonth.AddDate(0, i, 0)
		label := l.T(monthKeys[m.Month()-1])
		bars[i] = bar{Label: label, Tick: true, Value: float64(secs) / 3600,
			Tip: label + " " + strconv.Itoa(m.Year()) + " · " + l.Hours(secs)}
		d.MonthRows = append(d.MonthRows, monthPoint{Month: label + " " + strconv.Itoa(m.Year()), Hours: l.Hours(secs)})
	}
	d.Hours = barChart(l, bars, " h", l.T("Hours watched per month"))
	d.TotalHours = l.Hours(total)
	if d.TopSeries, err = s.Store.TopMedia(ctx, "series", 0, id, false, 5); err != nil {
		s.serverError(w, r, err)
		return
	}
	if d.TopMovies, err = s.Store.TopMedia(ctx, "movie", 0, id, false, 5); err != nil {
		s.serverError(w, r, err)
		return
	}
	genres, err := s.Store.GenreShares(ctx, 0, id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	sort.Slice(genres, func(i, j int) bool { return genres[i].PlaysShare > genres[j].PlaysShare })
	for _, g := range genres {
		if g.PlaysShare > 0 && len(d.Genres) < 5 {
			d.Genres = append(d.Genres, g)
		}
	}
	groups, err := s.resume(ctx, []store.Account{{ID: id, Name: d.Name}}, now)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if len(groups) == 1 {
		d.InProgress = groups[0].Items
	}
	for _, kind := range []string{"series", "movie"} {
		items, err := s.Store.Library(ctx, store.LibraryFilter{Kind: kind, User: id, States: []string{"abandoned"},
			Sort: "last", Desc: true, Limit: 10}, now, s.Config.AbandonDays)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		d.Abandoned = append(d.Abandoned, items...)
	}
	if d.Players, err = s.Store.Breakdown(ctx, "player", 0, id, 8); err != nil {
		s.serverError(w, r, err)
		return
	}
	if d.Platforms, err = s.Store.Breakdown(ctx, "platform", 0, id, 8); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "userstats", "", s.pageRaw(r, d.Name, "stats", d))
}

// --- cleanup --------------------------------------------------------------------------

type cleanupData struct {
	Kind         string
	Days         int
	Filter       cleanupForm
	Active       bool // a filter other than the type is set
	Items        []store.CleanupItem
	Recoverable  int64
	Seasons      []store.UnwatchedSeason
	SeasonsBytes int64
	Protected    string
	Columns      []sortCol // sortable headers of the candidates table
	Sort, Dir    string    // current sort, kept by the filters form
	Longer       []int     // "not watched for" choices longer than the rule (days)
}

// cleanupInactiveOptions are the "not watched for" choices of the cleanup
// page (days); only those longer than the rule are offered.
var cleanupInactiveOptions = []int{180, 365, 730}

// cleanupForm holds the cleanup filters as typed, to refill the form.
type cleanupForm struct {
	InactiveDays       int // 0: the rule of the Settings page
	States             []string
	PlaysMin, PlaysMax string
	SizeMin, SizeMax   string
}

// parseCleanupFilter reads the cleanup filters from the query string, the
// same way as the library's: unknown or invalid values are dropped.
func parseCleanupFilter(q url.Values) (store.CleanupFilter, cleanupForm) {
	f := store.CleanupFilter{
		Kind:      pick(q.Get("kind"), []string{"series", "movie"}),
		States:    pickAll(q["state"], watchStates),
		PlaysMin:  atoi(q.Get("plays_min")),
		SizeMinGB: atof(q.Get("size_min")),
		SizeMaxGB: atof(q.Get("size_max")),
	}
	if n, err := strconv.Atoi(strings.TrimSpace(q.Get("plays_max"))); err == nil && n >= 0 {
		f.PlaysMax = &n
	}
	form := cleanupForm{States: f.States}
	if n := atoi(q.Get("inactive")); slices.Contains(cleanupInactiveOptions, n) {
		form.InactiveDays = n
	}
	if f.PlaysMin > 0 {
		form.PlaysMin = strconv.Itoa(f.PlaysMin)
	}
	if f.PlaysMax != nil {
		form.PlaysMax = strconv.Itoa(*f.PlaysMax)
	}
	if f.SizeMinGB > 0 {
		form.SizeMin = strconv.FormatFloat(f.SizeMinGB, 'f', -1, 64)
	}
	if f.SizeMaxGB > 0 {
		form.SizeMax = strconv.FormatFloat(f.SizeMaxGB, 'f', -1, 64)
	}
	return f, form
}

// query encodes the filters (sort excluded), for the sortable headers.
func (c cleanupForm) query(kind string) url.Values {
	q := url.Values{}
	if kind != "" {
		q.Set("kind", kind)
	}
	if c.InactiveDays > 0 {
		q.Set("inactive", strconv.Itoa(c.InactiveDays))
	}
	if len(c.States) > 0 {
		q["state"] = c.States
	}
	for k, v := range map[string]string{"plays_min": c.PlaysMin, "plays_max": c.PlaysMax, "size_min": c.SizeMin, "size_max": c.SizeMax} {
		if v != "" {
			q.Set(k, v)
		}
	}
	return q
}

// sortCol is a sortable table header: a link that sorts by Key, or toggles
// the direction when it is the current sort.
type sortCol struct {
	Label string // English key
	Num   bool
	URL   string
	Aria  string // aria-sort: ascending | descending | "" (not sorted)
}

// cleanupColumns are the headers of the candidates table, keeping the
// filters of base. defaultDesc says which columns sort descending on the
// first click.
func cleanupColumns(base url.Values, sort string, desc bool) []sortCol {
	cols := []struct {
		key, label  string
		num, defDsc bool
	}{
		{"title", "Title", false, false}, {"size", "Size", true, true}, {"inactivity", "Why", false, true},
		{"state", "State", false, false}, {"plays", "Plays", true, true},
	}
	out := make([]sortCol, 0, len(cols))
	for _, c := range cols {
		dir := c.defDsc
		col := sortCol{Label: c.label, Num: c.num}
		if c.key == sort {
			col.Aria = "ascending"
			if desc {
				col.Aria = "descending"
			}
			dir = !desc
		}
		q := url.Values{"sort": {c.key}, "dir": {"asc"}}
		if dir {
			q.Set("dir", "desc")
		}
		for k, v := range base {
			q[k] = v
		}
		col.URL = "/cleanup?" + q.Encode()
		out = append(out, col)
	}
	return out
}

func (s *Server) cleanup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := time.Now()
	q := r.URL.Query()
	f, form := parseCleanupFilter(q)
	var d cleanupData
	scope, days := s.cleanupScope(ctx, now)
	for _, n := range cleanupInactiveOptions {
		if n > days {
			d.Longer = append(d.Longer, n)
		}
	}
	if form.InactiveDays > days {
		scope.Cutoff = now.Add(-time.Duration(form.InactiveDays) * day).Unix()
	} else {
		form.InactiveDays = 0 // not stricter than the rule: ignored
	}
	d.Kind, d.Days, d.Filter, d.Protected = f.Kind, days, form, strings.Join(s.Config.ProtectedTags, ", ")
	d.Active = len(form.query("")) > 0
	sort, desc := q.Get("sort"), q.Get("dir") == "desc"
	if !store.ValidCleanupSort(sort) {
		sort, desc = "size", true // largest first
	}
	d.Sort, d.Dir = sort, "asc"
	if desc {
		d.Dir = "desc"
	}
	d.Columns = cleanupColumns(form.query(f.Kind), sort, desc)
	var err error
	if d.Items, d.Recoverable, err = s.Store.CleanupCandidates(ctx, scope, f, sort, desc, 100); err != nil {
		s.serverError(w, r, err)
		return
	}
	// Never-watched seasons have no state or play count of their own: they
	// are listed only while those filters are unset.
	if f.Kind != "movie" && len(f.States) == 0 && f.PlaysMin == 0 && f.PlaysMax == nil {
		if d.Seasons, d.SeasonsBytes, err = s.Store.UnwatchedSeasons(ctx, scope, 50); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	s.render(w, r, http.StatusOK, "cleanup", "", s.page(r, "Cleanup", "cleanup", d))
}

// --- cleanup settings -----------------------------------------------------------------

// Cleanup rule bounds (days without any view).
const (
	defaultCleanupDays = 180
	minCleanupDays     = 7
	maxCleanupDays     = 3650
)

type cleanupSettings struct {
	Days  int
	Error string
	Saved bool
}

func (s *Server) cleanupDays(ctx context.Context) int {
	v, err := s.Store.Setting(ctx, store.SettingCleanupDays, "")
	if n, perr := strconv.Atoi(v); err == nil && perr == nil && n >= minCleanupDays && n <= maxCleanupDays {
		return n
	}
	return defaultCleanupDays
}

func (s *Server) cleanupSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st := cleanupSettings{}
	status := http.StatusOK
	n, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("days")))
	if err != nil || n < minCleanupDays || n > maxCleanupDays {
		st.Days = s.cleanupDays(ctx)
		st.Error = s.loc(r).T("Invalid number of days: %d to %d.", minCleanupDays, maxCleanupDays)
		status = http.StatusUnprocessableEntity
	} else {
		if err := s.Store.SetSetting(ctx, store.SettingCleanupDays, strconv.Itoa(n), time.Now()); err != nil {
			s.serverError(w, r, err)
			return
		}
		s.Log.Info("cleanup rule changed", "days", n, "by", sessionFrom(r).Username)
		st.Days, st.Saved = n, true
	}
	pd := s.page(r, "Settings", "settings", settingsPage{Sync: s.settingsData(s.loc(r), "", false), Cleanup: st})
	if isHTMX(r) {
		s.render(w, r, http.StatusOK, "settings", "cleanup_form", pd)
		return
	}
	s.render(w, r, status, "settings", "", pd)
}
