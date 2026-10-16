package web

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"time"

	"hangarr/internal/media"
	"hangarr/internal/store"
)

const day = 24 * time.Hour

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
	// Three queries for every viewer at once, not three per viewer.
	ids := make([]int64, 0, len(users))
	for _, u := range users {
		ids = append(ids, u.ID)
	}
	byUser, err := s.Store.InProgressSeries(ctx, ids, 8, now, s.Config.AbandonDays)
	if err != nil {
		return nil, err
	}
	last, err := s.Store.LastEpisodesByUser(ctx, ids)
	if err != nil {
		return nil, err
	}
	var series []int64
	for _, items := range byUser {
		for _, it := range items {
			series = append(series, it.ID)
		}
	}
	totals, err := s.Store.SeasonTotals(ctx, series)
	if err != nil {
		return nil, err
	}
	var out []resumeGroup
	for _, u := range users {
		items := byUser[u.ID]
		if len(items) == 0 {
			continue
		}
		g := resumeGroup{User: u.Name}
		for _, it := range items {
			ri := resumeItem{LibraryItem: it}
			if l, ok := last[u.ID][it.ID]; ok {
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
