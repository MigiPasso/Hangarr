package upstream

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"hangarr/internal/clients/sonarr"
	"hangarr/internal/i18n"
)

// Tracker is an indexer of Sonarr or Radarr and its live state.
type Tracker struct {
	Name     string
	Protocol string // usenet | torrent
	RSS      bool
	Search   bool // automatic or interactive search
	// DisabledUntil is set while the instance skips the indexer after
	// failures.
	DisabledUntil *time.Time
}

// Failing reports whether the instance currently skips the indexer after failures.
func (t Tracker) Failing() bool { return t.DisabledUntil != nil }

// Enabled reports whether the indexer is used at all (RSS or search).
func (t Tracker) Enabled() bool { return t.RSS || t.Search }

// TrackerStatus lists the indexers of one service (Santé page).
type TrackerStatus struct {
	Service    string
	Label      string
	Configured bool
	Error      string // explained in the UI language; Trackers is then empty
	Trackers   []Tracker
	Alerts     []sonarr.HealthCheck // indexer health alerts
}

// indexerReader is what Sonarr and Radarr clients share for the trackers.
type indexerReader interface {
	Indexers(context.Context) ([]sonarr.Indexer, error)
	IndexerStatuses(context.Context) ([]sonarr.IndexerStatus, error)
}

// CheckTrackers reads the indexers of Sonarr and Radarr in parallel, failing
// ones first; each service is bounded by CheckTimeout.
func CheckTrackers(ctx context.Context, c *Clients, l i18n.Localizer, now time.Time) []TrackerStatus {
	svcs := []struct {
		name string
		r    indexerReader
	}{{Sonarr, nil}, {Radarr, nil}}
	if c.Sonarr != nil {
		svcs[0].r = c.Sonarr
	}
	if c.Radarr != nil {
		svcs[1].r = c.Radarr
	}
	out := make([]TrackerStatus, len(svcs))
	var wg sync.WaitGroup
	for i, s := range svcs {
		out[i] = TrackerStatus{Service: s.name, Label: ServiceLabel(s.name), Configured: s.r != nil}
		if s.r == nil {
			continue
		}
		wg.Go(func() {
			st := &out[i]
			cctx, cancel := context.WithTimeout(ctx, CheckTimeout)
			defer cancel()
			indexers, err := s.r.Indexers(cctx)
			if err != nil {
				st.Error = Describe(l, err)
				return
			}
			statuses, _ := s.r.IndexerStatuses(cctx) // optional: no status means no failure
			st.Trackers = trackers(indexers, statuses, now)
			for _, a := range healthAlerts(ctx, c, s.name) {
				if IsIndexerAlert(a) {
					st.Alerts = append(st.Alerts, a)
				}
			}
		})
	}
	wg.Wait()
	return out
}

// trackers joins the indexers and their statuses: failing first, then
// enabled ones, then by name.
func trackers(indexers []sonarr.Indexer, statuses []sonarr.IndexerStatus, now time.Time) []Tracker {
	byID := map[int64]sonarr.IndexerStatus{}
	for _, s := range statuses {
		byID[s.IndexerID] = s
	}
	out := make([]Tracker, 0, len(indexers))
	for _, ix := range indexers {
		t := Tracker{Name: ix.Name, Protocol: ix.Protocol, RSS: ix.EnableRss,
			Search: ix.EnableAutomaticSearch || ix.EnableInteractiveSearch}
		if s, ok := byID[ix.ID]; ok && s.DisabledTill.After(now) {
			until := s.DisabledTill.Time
			t.DisabledUntil = &until
		}
		out = append(out, t)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Failing() != b.Failing() {
			return a.Failing()
		}
		if a.Enabled() != b.Enabled() {
			return a.Enabled()
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
	return out
}
