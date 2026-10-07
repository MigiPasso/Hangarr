package upstream

import (
	"context"
	"strings"
	"sync"
	"time"

	"hangarr/internal/clients/sonarr"
	"hangarr/internal/i18n"
)

// ServiceStatus is the live state of one service (Santé page).
type ServiceStatus struct {
	Service    string
	Label      string
	Configured bool
	OK         bool
	Version    string
	Latency    time.Duration
	Error      string               // explained in the UI language (Describe)
	Alerts     []sonarr.HealthCheck // Sonarr/Radarr health alerts other than "ok", indexers excepted
}

// CheckAll tests every service in parallel; each check is bounded by
// CheckTimeout.
func CheckAll(ctx context.Context, c *Clients, l i18n.Localizer) []ServiceStatus {
	out := make([]ServiceStatus, len(Services))
	var wg sync.WaitGroup
	for i, svc := range Services {
		out[i] = ServiceStatus{Service: svc, Label: ServiceLabel(svc), Configured: c.Values.complete(svc)}
		if !out[i].Configured {
			continue
		}
		wg.Add(1)
		go func(st *ServiceStatus) {
			defer wg.Done()
			start := time.Now()
			version, err := Check(ctx, c, st.Service)
			st.Latency = time.Since(start).Round(time.Millisecond)
			st.OK, st.Version, st.Error = err == nil, version, Describe(l, err)
			if err == nil {
				st.Alerts = alerts(ctx, c, st.Service)
			}
		}(&out[i])
	}
	wg.Wait()
	return out
}

// alerts returns the Sonarr/Radarr health alerts that are not about the
// indexers (those go with the trackers); failures are ignored (the
// connection itself was checked).
func alerts(ctx context.Context, c *Clients, svc string) []sonarr.HealthCheck {
	var out []sonarr.HealthCheck
	for _, a := range healthAlerts(ctx, c, svc) {
		if !IsIndexerAlert(a) {
			out = append(out, a)
		}
	}
	return out
}

// IsIndexerAlert reports whether a health alert is about the indexers
// (IndexerStatusCheck, IndexerRssCheck, IndexerSearchCheck…).
func IsIndexerAlert(a sonarr.HealthCheck) bool { return strings.HasPrefix(a.Source, "Indexer") }

// healthAlerts returns every Sonarr/Radarr health alert other than "ok".
func healthAlerts(ctx context.Context, c *Clients, svc string) []sonarr.HealthCheck {
	ctx, cancel := context.WithTimeout(ctx, CheckTimeout)
	defer cancel()
	var list []sonarr.HealthCheck
	switch {
	case svc == Sonarr && c.Sonarr != nil:
		list, _ = c.Sonarr.Health(ctx)
	case svc == Radarr && c.Radarr != nil:
		list, _ = c.Radarr.Health(ctx)
	}
	var out []sonarr.HealthCheck
	for _, a := range list {
		if a.Type != "ok" {
			out = append(out, a)
		}
	}
	return out
}
