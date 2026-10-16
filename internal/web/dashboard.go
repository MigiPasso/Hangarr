package web

import (
	"context"
	"html/template"
	"net/http"
	"sort"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"hangarr/internal/store"
)

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
	mu     sync.Mutex
	at     time.Time
	rows   []diskRow
	flight singleflight.Group
}

const diskCacheTTL = time.Minute

// diskSpace merges the disks reported by Sonarr and Radarr (deduplicated by
// path), cached for a minute so the dashboard does not hit the *arr apps on
// every view. The lock only guards the cache: concurrent dashboards share
// one read (singleflight) instead of waiting for each other's requests.
func (s *Server) diskSpace(ctx context.Context) []diskRow {
	s.disks.mu.Lock()
	if time.Since(s.disks.at) < diskCacheTTL {
		rows := s.disks.rows
		s.disks.mu.Unlock()
		return rows
	}
	s.disks.mu.Unlock()
	v, _, _ := s.disks.flight.Do("disks", func() (any, error) {
		rows := s.readDiskSpace(context.WithoutCancel(ctx))
		s.disks.mu.Lock()
		s.disks.at, s.disks.rows = time.Now(), rows
		s.disks.mu.Unlock()
		return rows, nil
	})
	rows, _ := v.([]diskRow)
	return rows
}

// readDiskSpace asks Sonarr and Radarr (3 seconds at most).
func (s *Server) readDiskSpace(ctx context.Context) []diskRow {
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
