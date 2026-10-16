package web

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"hangarr/internal/store"
)

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
