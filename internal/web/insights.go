package web

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"arrstats/internal/store"
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

func hours(secs int64) string {
	return trimFloat(float64(secs)/3600) + " h"
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
	Overview     *store.Overview
	CleanupDays  int
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
	pd := s.page(r, "Tableau de bord", "dashboard", &d)
	if isHTMX(r) && r.Header.Get("HX-Target") == "top" {
		s.render(w, r, http.StatusOK, "home", "dashboard_top", pd)
		return
	}
	scope, days := s.cleanupScope(ctx, now)
	d.CleanupDays = days
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
	bars := make([]bar, 30)
	for i, secs := range perDay {
		date := first.AddDate(0, 0, i)
		bars[i] = bar{Label: date.Format("02/01"), Tick: i%7 == 1 || i == 29, Value: float64(secs) / 3600,
			Tip: date.Format("02/01") + " : " + hours(secs)}
		d.ActivityRows = append(d.ActivityRows, dayPoint{Date: date.Format("02/01/2006"), Hours: hours(secs)})
	}
	d.Activity = barChart(bars, " h", "Heures regardées par jour sur 30 jours")
	d.Disks = s.diskSpace(ctx)
	s.render(w, r, http.StatusOK, "home", "", pd)
}

// --- statistics ---------------------------------------------------------------------

var periods = []pair{{"7", "7 jours"}, {"30", "30 jours"}, {"90", "90 jours"}, {"365", "12 mois"}, {"all", "Tout"}}

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
	return "30", "30 jours", now.Add(-30 * day).Unix()
}

// nextEpisode approximates the next episode from the last one watched and
// stored season sizes (the detail page has the exact value).
func nextEpisode(last [2]int, totals map[int]int) string {
	season, ep := last[0], last[1]
	if ep < totals[season] {
		return episodeLabel(season, ep+1)
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
	return episodeLabel(next[0], 1)
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
				ri.Last = episodeLabel(l[0], l[1])
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
	for wd := range 7 {
		row := []string{weekdays[wd]}
		for h := range 24 {
			c := &grid[wd][h]
			c.Tip = fmt.Sprintf("%s %dh–%dh : %s", weekdays[wd], h, h+1, plural(int64(c.Count), "session", "sessions"))
			row = append(row, strconv.Itoa(c.Count))
		}
		d.HeatRows = append(d.HeatRows, row)
	}
	d.Heatmap = heatmap(grid, "Sessions de lecture par jour de la semaine et par heure")
	d.Hours = hours(secs)

	if d.Ranking, err = s.Store.UserRanking(ctx, since); fail(err) {
		return
	}
	for _, u := range d.Ranking {
		if d.User == 0 || u.UserID == d.User {
			d.Plays += u.Plays
			d.Titles += u.Titles
		}
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
		rows = append(rows, pairRow{Label: g.Genre, A: g.StoredShare, B: g.PlaysShare})
	}
	d.Genres = pairedBars(rows, "Espace stocké", "Lectures", "Part de l'espace stocké et des lectures par genre")
	if d.Cost, err = s.Store.CostPerPlay(ctx, 15); fail(err) {
		return
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
	if d.WhoWatches, err = s.Store.WhoWatchesWhat(ctx, 10); fail(err) {
		return
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
	s.render(w, r, http.StatusOK, "stats", "", s.page(r, "Statistiques", "stats", d))
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

var monthNames = []string{"janv.", "févr.", "mars", "avr.", "mai", "juin", "juil.", "août", "sept.", "oct.", "nov.", "déc."}

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
	bars := make([]bar, 12)
	for i, secs := range perMonth {
		m := firstMonth.AddDate(0, i, 0)
		label := monthNames[m.Month()-1]
		bars[i] = bar{Label: label, Tick: true, Value: float64(secs) / 3600,
			Tip: label + " " + strconv.Itoa(m.Year()) + " : " + hours(secs)}
		d.MonthRows = append(d.MonthRows, monthPoint{Month: label + " " + strconv.Itoa(m.Year()), Hours: hours(secs)})
	}
	d.Hours = barChart(bars, " h", "Heures regardées par mois")
	d.TotalHours = hours(total)
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
	s.render(w, r, http.StatusOK, "userstats", "", s.page(r, d.Name, "stats", d))
}

// --- cleanup --------------------------------------------------------------------------

type cleanupData struct {
	Kind         string
	Days         int
	Items        []store.CleanupItem
	Recoverable  int64
	Seasons      []store.UnwatchedSeason
	SeasonsBytes int64
	Protected    string
}

func (s *Server) cleanup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := time.Now()
	kind := pick(r.URL.Query().Get("kind"), []string{"series", "movie"})
	scope, days := s.cleanupScope(ctx, now)
	d := cleanupData{Kind: kind, Days: days, Protected: strings.Join(s.Config.ProtectedTags, ", ")}
	var err error
	if d.Items, d.Recoverable, err = s.Store.CleanupCandidates(ctx, scope, kind, 100); err != nil {
		s.serverError(w, r, err)
		return
	}
	if kind != "movie" {
		if d.Seasons, d.SeasonsBytes, err = s.Store.UnwatchedSeasons(ctx, scope, 50); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	s.render(w, r, http.StatusOK, "cleanup", "", s.page(r, "Nettoyage", "cleanup", d))
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
		st.Error = fmt.Sprintf("Nombre de jours invalide : entre %d et %d.", minCleanupDays, maxCleanupDays)
		status = http.StatusUnprocessableEntity
	} else {
		if err := s.Store.SetSetting(ctx, store.SettingCleanupDays, strconv.Itoa(n), time.Now()); err != nil {
			s.serverError(w, r, err)
			return
		}
		s.Log.Info("cleanup rule changed", "days", n, "by", sessionFrom(r).Username)
		st.Days, st.Saved = n, true
	}
	pd := s.page(r, "Réglages", "settings", settingsPage{Sync: s.settingsData("", false), Cleanup: st})
	if isHTMX(r) {
		s.render(w, r, http.StatusOK, "settings", "cleanup_form", pd)
		return
	}
	s.render(w, r, status, "settings", "", pd)
}
