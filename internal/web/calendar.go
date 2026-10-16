package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"hangarr/internal/clients/radarr"
	"hangarr/internal/clients/sonarr"
	"hangarr/internal/i18n"
	"hangarr/internal/media"
)

// calendarRanges are the periods offered on the Upcoming page (days), in the
// order of the tabs; calendarDefaultDays is the default one.
var calendarRanges = []int{30, 90, 365}

const calendarDefaultDays = 90

// calendarPageSizes are the page sizes of the list; the second is the default.
var calendarPageSizes = []int{10, 25, 50, 100}

// calendarTimeout bounds each live call to Sonarr/Radarr.
const calendarTimeout = 8 * time.Second

// calendarEntry is one line of the calendar: episodes of a series airing the
// same day (grouped), or one release of a movie.
type calendarEntry struct {
	Kind    string // series | movie
	ArrID   int64
	MediaID int64 // 0: not synced yet, no link
	Title   string
	// What is released: "S02E01", "S02E01–E03" or a movie release type.
	What         string
	EpisodeTitle string   // single episode only
	Count        int      // episodes in the group
	Time         string   // local air time, "" for movies (dates only)
	Notes        []string // premiere, finale (translated), in episode order
	HasFile      bool
	at           time.Time
	season       int
	episodes     []int
	notes        map[int]string // episode number -> note, while grouping
}

type calendarDay struct {
	Label   string // "Today", "Tomorrow", "Monday 12/10/2026"
	Entries []calendarEntry
	// Date tile of the grid view: "Mon", "12", "Oct" and "Tomorrow",
	// "in 5 days"…
	Weekday, Num, Month, Rel string
}

// calendarCell is a day of the month grid.
type calendarCell struct {
	Day     int
	InMonth bool // false: day of the previous or next month (grid padding)
	Today   bool
	Past    bool
	Entries []calendarEntry
}

type calendarData struct {
	View   string // list | grid (posters) | month
	Days   int
	Ranges []int
	List   []calendarDay // current page, by day
	// List paging: Total entries, Page of Pages, Size entries per page.
	Total, Page, Pages, Size int
	Sizes                    []int
	PrevURL, NextURL         string
	// Month view: "2026-10", its label, the neighbours and the weeks
	// (Monday first).
	Month, MonthLabel, Prev, Next string
	Weekdays                      []string
	Weeks                         [][]calendarCell
	// Failed names the services that did not answer; Unconfigured those
	// without a configuration.
	Failed       []string
	Unconfigured []string
	// Loading: the page is sent at once and the releases load by HTMX (a
	// slow Sonarr/Radarr never delays the page); LiveURL shows them without
	// JavaScript.
	Loading bool
	LiveURL string
}

// monthNames are the full English month names (translation keys; "May" is
// also the short name of the heatmap, hence its context suffix).
var monthNames = [...]string{"January", "February", "March", "April", "May|full", "June", "July", "August", "September",
	"October", "November", "December"}

// calendar is the Upcoming page: monitored episodes (Sonarr) and movie
// releases (Radarr), read live so it is always up to date. The list starts
// today; the month view (view=month&month=2026-10) shows a whole month.
func (s *Server) calendar(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	l := s.loc(r)
	loc := s.Config.Location
	q := r.URL.Query()
	d := calendarData{View: "list", Days: calendarDefaultDays, Ranges: calendarRanges,
		Size: calendarPageSizes[1], Sizes: calendarPageSizes, Page: 1}
	if n, err := strconv.Atoi(q.Get("days")); err == nil && slices.Contains(calendarRanges, n) {
		d.Days = n
	}
	if n, err := strconv.Atoi(q.Get("size")); err == nil && slices.Contains(calendarPageSizes, n) {
		d.Size = n
	}
	if n, err := strconv.Atoi(q.Get("page")); err == nil && n > 1 {
		d.Page = min(n, maxPage)
	}
	now := time.Now().In(loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	start, end := today, today.AddDate(0, 0, d.Days)
	var first time.Time // month view: first day of the month
	if q.Get("view") == "grid" {
		d.View = "grid"
	}
	if q.Get("view") == "month" {
		d.View = "month"
		first = time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, loc)
		if m, err := time.ParseInLocation("2006-01", q.Get("month"), loc); err == nil && m.Year() >= 2000 && m.Year() <= 2100 {
			first = m
		}
		// Whole weeks, Monday first.
		start = first.AddDate(0, 0, -(int(first.Weekday())+6)%7)
		last := first.AddDate(0, 1, -1)
		end = last.AddDate(0, 0, 7-(int(last.Weekday())+6)%7)
		d.Month, d.Prev, d.Next = first.Format("2006-01"), first.AddDate(0, -1, 0).Format("2006-01"), first.AddDate(0, 1, 0).Format("2006-01")
		d.MonthLabel = l.T(monthNames[first.Month()-1]) + " " + strconv.Itoa(first.Year())
		for _, k := range weekdayKeys {
			d.Weekdays = append(d.Weekdays, l.T(k))
		}
	}

	// First load: the page at once, the releases by HTMX (or with live=1).
	if !isHTMX(r) && q.Get("live") != "1" {
		live := r.URL.Query()
		live.Set("live", "1")
		d.Loading, d.LiveURL = true, "/calendar?"+live.Encode()
		s.render(w, r, http.StatusOK, "calendar", "", s.page(r, "Upcoming", "calendar", d))
		return
	}

	entries, failed, unconfigured := s.calendarEntries(ctx, l, start, end)
	d.Failed, d.Unconfigured = failed, unconfigured
	if len(entries) > 0 {
		media, err := s.Store.ListArrMedia(ctx)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		ids := map[string]int64{}
		for _, m := range media {
			ids[fmt.Sprintf("%s/%d", m.Kind, m.ArrID)] = m.ID
		}
		for i := range entries {
			entries[i].MediaID = ids[fmt.Sprintf("%s/%d", entries[i].Kind, entries[i].ArrID)]
		}
	}
	if d.View == "month" {
		d.Weeks = calendarWeeks(entries, first, start, end, today)
	} else {
		d.Total = len(entries)
		d.Pages = max(1, (d.Total+d.Size-1)/d.Size)
		d.Page = min(d.Page, d.Pages)
		if d.Page > 1 {
			d.PrevURL = calendarListURL(d.View, d.Days, d.Size, d.Page-1)
		}
		if d.Page < d.Pages {
			d.NextURL = calendarListURL(d.View, d.Days, d.Size, d.Page+1)
		}
		from := (d.Page - 1) * d.Size
		d.List = calendarDays(l, entries[from:min(from+d.Size, d.Total)], today)
	}
	s.render(w, r, http.StatusOK, "calendar", "", s.page(r, "Upcoming", "calendar", d))
}

// calendarListURL is the list or grid view with its period, page size and
// page.
func calendarListURL(view string, days, size, page int) string {
	q := url.Values{"days": {strconv.Itoa(days)}, "size": {strconv.Itoa(size)}}
	if view == "grid" {
		q.Set("view", "grid")
	}
	if page > 1 {
		q.Set("page", strconv.Itoa(page))
	}
	return "/calendar?" + q.Encode()
}

// calendarEntries reads both calendars in parallel; failed and unconfigured
// name the services without an answer.
func (s *Server) calendarEntries(ctx context.Context, l i18n.Localizer, start, end time.Time) (entries []calendarEntry, failed, unconfigured []string) {
	up := s.Upstreams.Get()
	var (
		wg       sync.WaitGroup
		episodes []sonarr.CalendarEpisode
		movies   []radarr.CalendarMovie
		errSon   error
		errRad   error
	)
	if up.Sonarr == nil {
		unconfigured = append(unconfigured, "Sonarr")
	} else {
		wg.Go(func() {
			cctx, cancel := context.WithTimeout(ctx, calendarTimeout)
			defer cancel()
			episodes, errSon = up.Sonarr.Calendar(cctx, start, end)
		})
	}
	if up.Radarr == nil {
		unconfigured = append(unconfigured, "Radarr")
	} else {
		wg.Go(func() {
			cctx, cancel := context.WithTimeout(ctx, calendarTimeout)
			defer cancel()
			movies, errRad = up.Radarr.Calendar(cctx, start, end)
		})
	}
	wg.Wait()
	for _, f := range []struct {
		name string
		err  error
	}{{"Sonarr", errSon}, {"Radarr", errRad}} {
		if f.err != nil {
			s.Log.Warn("calendar unavailable", "service", f.name, "err", f.err)
			failed = append(failed, f.name)
		}
	}
	loc := s.Config.Location
	entries = append(seriesEntries(l, episodes, loc), movieEntries(l, movies, loc, start, end)...)
	sortEntries(entries)
	return entries, failed, unconfigured
}

// calendarWeeks lays the entries out on the weeks of [start, end).
func calendarWeeks(entries []calendarEntry, first, start, end, today time.Time) [][]calendarCell {
	byDay := map[string][]calendarEntry{}
	for _, e := range entries {
		k := e.at.Format(time.DateOnly)
		byDay[k] = append(byDay[k], e)
	}
	var weeks [][]calendarCell
	for day := start; day.Before(end); day = day.AddDate(0, 0, 1) {
		if (int(day.Weekday())+6)%7 == 0 {
			weeks = append(weeks, nil)
		}
		w := len(weeks) - 1
		weeks[w] = append(weeks[w], calendarCell{Day: day.Day(), InMonth: day.Month() == first.Month(),
			Today: day.Equal(today), Past: day.Before(today), Entries: byDay[day.Format(time.DateOnly)]})
	}
	return weeks
}

// seriesEntries groups the episodes of a series and season airing the same
// local day ("S02E01–E03": a whole season released at once).
func seriesEntries(l i18n.Localizer, episodes []sonarr.CalendarEpisode, loc *time.Location) []calendarEntry {
	var out []calendarEntry
	index := map[string]int{}
	for _, e := range episodes {
		if e.AirDateUTC.IsZero() {
			continue
		}
		at := e.AirDateUTC.In(loc)
		title := ""
		if e.Series != nil {
			title = e.Series.Title
		}
		key := fmt.Sprintf("%d/%d/%s", e.SeriesID, e.SeasonNumber, at.Format(time.DateOnly))
		i, ok := index[key]
		if !ok {
			index[key] = len(out)
			out = append(out, calendarEntry{Kind: "series", ArrID: e.SeriesID, Title: title, at: at,
				season: e.SeasonNumber, HasFile: true})
			i = len(out) - 1
		}
		g := &out[i]
		if at.Before(g.at) {
			g.at = at
		}
		g.episodes = append(g.episodes, e.EpisodeNumber)
		g.EpisodeTitle = e.Title
		g.HasFile = g.HasFile && e.HasFile
		if n := episodeNote(l, e); n != "" {
			if g.notes == nil {
				g.notes = map[int]string{}
			}
			g.notes[e.EpisodeNumber] = n
		}
	}
	for i := range out {
		g := &out[i]
		sort.Ints(g.episodes)
		// Premiere and finale of a season released at once: both, in
		// episode order.
		for _, ep := range g.episodes {
			if n := g.notes[ep]; n != "" && !slices.Contains(g.Notes, n) {
				g.Notes = append(g.Notes, n)
			}
		}
		g.Count = len(g.episodes)
		g.Time = g.at.Format("15:04")
		g.What = media.EpisodeCode(g.season, g.episodes[0])
		if g.Count > 1 {
			g.What += fmt.Sprintf("–E%02d", g.episodes[g.Count-1])
			g.EpisodeTitle = ""
		}
	}
	return out
}

// episodeNote flags finales (Sonarr v4) and premieres.
func episodeNote(l i18n.Localizer, e sonarr.CalendarEpisode) string {
	switch e.FinaleType {
	case "series":
		return l.T("Series finale")
	case "season":
		return l.T("Season finale")
	case "midseason":
		return l.T("Mid-season finale")
	}
	switch {
	case e.EpisodeNumber != 1 || e.SeasonNumber == 0:
		return ""
	case e.SeasonNumber == 1:
		return l.T("Series premiere")
	default:
		return l.T("Season premiere")
	}
}

// releaseTypes name the Radarr release dates, in the order of movieEntries.
var releaseTypes = [...]string{"In cinemas", "Digital release", "Physical release"}

// movieEntries gives one entry per release date in [start, end). Radarr dates
// are days (midnight UTC): they are kept as is, not converted to loc, so they
// never move to the previous day.
func movieEntries(l i18n.Localizer, movies []radarr.CalendarMovie, loc *time.Location, start, end time.Time) []calendarEntry {
	var out []calendarEntry
	for _, m := range movies {
		for i, at := range []time.Time{m.InCinemas.Time, m.DigitalRelease.Time, m.PhysicalRelease.Time} {
			if at.IsZero() {
				continue
			}
			y, mo, d := at.UTC().Date()
			day := time.Date(y, mo, d, 0, 0, 0, 0, loc)
			if day.Before(start) || !day.Before(end) {
				continue
			}
			out = append(out, calendarEntry{Kind: "movie", ArrID: m.ID, Title: m.Title, What: l.T(releaseTypes[i]),
				HasFile: m.HasFile, at: day})
		}
	}
	return out
}

// sortEntries orders by day, movies first (no time), then episodes by air
// time, then by title.
func sortEntries(entries []calendarEntry) {
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if !a.at.Equal(b.at) {
			return a.at.Before(b.at)
		}
		if a.Kind != b.Kind {
			return a.Kind == "movie"
		}
		return strings.ToLower(a.Title) < strings.ToLower(b.Title)
	})
}

// calendarDays splits sorted entries by day.
func calendarDays(l i18n.Localizer, entries []calendarEntry, today time.Time) []calendarDay {
	var out []calendarDay
	last := ""
	for _, e := range entries {
		key := e.at.Format(time.DateOnly)
		if key != last {
			out = append(out, newCalendarDay(l, e.at, today))
			last = key
		}
		out[len(out)-1].Entries = append(out[len(out)-1].Entries, e)
	}
	return out
}

// weekdayNames are the full English day names (translation keys).
var weekdayNames = [...]string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}

func newCalendarDay(l i18n.Localizer, t, today time.Time) calendarDay {
	y, m, d := t.Date()
	day := time.Date(y, m, d, 0, 0, 0, 0, today.Location())
	cd := calendarDay{Label: dayLabel(l, t, today), Weekday: l.T(weekdayKeys[(int(t.Weekday())+6)%7]),
		Num: strconv.Itoa(d), Month: l.T(monthKeys[m-1])}
	// Whole days between the two midnights (23 or 25 h around DST changes).
	switch n := int((day.Sub(today) + 12*time.Hour) / (24 * time.Hour)); {
	case n == 0:
		cd.Rel = l.T("Today")
	case n == 1:
		cd.Rel = l.T("Tomorrow")
	case n > 1:
		cd.Rel = l.N(int64(n), "in %d day", "in %d days")
	}
	return cd
}

func dayLabel(l i18n.Localizer, t, today time.Time) string {
	y, m, d := t.Date()
	day := time.Date(y, m, d, 0, 0, 0, 0, today.Location())
	switch {
	case day.Equal(today):
		return l.T("Today")
	case day.Equal(today.AddDate(0, 0, 1)):
		return l.T("Tomorrow")
	}
	return l.T(weekdayNames[t.Weekday()]) + " " + l.Date(t)
}
