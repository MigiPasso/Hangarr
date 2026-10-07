package upstream

import (
	"context"
	"sort"
	"strings"
	"sync"

	"hangarr/internal/clients/sonarr"
	"hangarr/internal/i18n"
)

// Tracker is an indexer of Sonarr or Radarr and its live state.
type Tracker struct {
	Name     string
	Protocol string // usenet | torrent
	RSS      bool
	Search   bool // automatic or interactive search
	// Failing: the instance currently skips the indexer after failures.
	Failing bool
}

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
}

// CheckTrackers reads the indexers of Sonarr and Radarr in parallel, failing
// ones first; each service is bounded by CheckTimeout.
//
// Sonarr and Radarr have no API for the state of their indexers (that is
// Prowlarr's /indexerstatus): the failing ones are read from their health
// checks, whose message lists them ("…: A, B").
func CheckTrackers(ctx context.Context, c *Clients, l i18n.Localizer) []TrackerStatus {
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
			for _, a := range healthAlerts(ctx, c, s.name) {
				if IsIndexerAlert(a) {
					st.Alerts = append(st.Alerts, a)
				}
			}
			st.Trackers = trackers(indexers, st.Alerts)
		})
	}
	wg.Wait()
	return out
}

// failureChecks are the health checks of indexers skipped after failures
// (now, and for more than 6 hours); their message ends with the names.
var failureChecks = map[string]bool{"IndexerStatusCheck": true, "IndexerLongTermStatusCheck": true}

// trackers marks the indexers named by a failure check: failing first, then
// enabled ones, then by name. A failure check that contains no indexer name
// at all is the "all indexers are unavailable" variant: every enabled one is
// failing. A name found but not recognized as a list item (unexpected
// separator) marks nothing: better a missed failure than false ones.
func trackers(indexers []sonarr.Indexer, alerts []sonarr.HealthCheck) []Tracker {
	out := make([]Tracker, 0, len(indexers))
	for _, ix := range indexers {
		out = append(out, Tracker{Name: ix.Name, Protocol: ix.Protocol, RSS: ix.EnableRss,
			Search: ix.EnableAutomaticSearch || ix.EnableInteractiveSearch})
	}
	for _, a := range alerts {
		if !failureChecks[a.Source] {
			continue
		}
		mentioned := false
		for i := range out {
			if namedIn(a.Message, out[i].Name) {
				out[i].Failing = true
			}
			mentioned = mentioned || (out[i].Name != "" && strings.Contains(a.Message, out[i].Name))
		}
		if !mentioned {
			for i := range out {
				out[i].Failing = out[i].Failing || out[i].Enabled()
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Failing != b.Failing {
			return a.Failing
		}
		if a.Enabled() != b.Enabled() {
			return a.Enabled()
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
	return out
}

// namedIn reports whether name is one of the items of the list that ends msg
// ("Indexers unavailable due to failures: A, B"): the exact name, after a
// colon (ASCII or full-width, any spacing) or ", ", and before ", ", a final
// period or the end, so "Orion" never matches "Orion - S".
func namedIn(msg, name string) bool {
	if name == "" {
		return false
	}
	for i := 0; ; {
		j := strings.Index(msg[i:], name)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(name)
		head := strings.TrimRight(msg[:start], " ")
		before := strings.HasSuffix(head, ":") || strings.HasSuffix(head, "：") || strings.HasSuffix(msg[:start], ", ")
		rest := msg[end:]
		after := rest == "" || rest == "." || rest == "。" || strings.HasPrefix(rest, ", ")
		if before && after {
			return true
		}
		i = start + 1
	}
}
