package upstream

import (
	"context"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"hangarr/internal/clients/sonarr"
	"hangarr/internal/i18n"
)

// ServiceStatus is the live state of one service (Health page).
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

// alertsTTL keeps the health alerts of a service for the two sections of the
// Health page (services and trackers), loaded by two requests at once.
const alertsTTL = 15 * time.Second

type alertsEntry struct {
	at   time.Time
	list []sonarr.HealthCheck
}

// alertsCache keeps the health alerts of one Clients, by service; concurrent
// callers share one request.
type alertsCache struct {
	mu      sync.Mutex
	entries map[string]alertsEntry
	flight  singleflight.Group
}

func newAlertsCache() *alertsCache { return &alertsCache{entries: map[string]alertsEntry{}} }

// healthAlerts returns every Sonarr/Radarr health alert other than "ok".
// Concurrent callers share one request and the result is kept alertsTTL.
func healthAlerts(ctx context.Context, c *Clients, svc string) []sonarr.HealthCheck {
	cache := c.alerts
	if cache == nil {
		list, _ := fetchAlerts(ctx, c, svc)
		return list
	}
	cache.mu.Lock()
	e, ok := cache.entries[svc]
	cache.mu.Unlock()
	if ok && time.Since(e.at) < alertsTTL {
		return e.list
	}
	v, _, _ := cache.flight.Do(svc, func() (any, error) {
		list, err := fetchAlerts(context.WithoutCancel(ctx), c, svc)
		if err != nil {
			return list, nil // shared with the concurrent callers, never cached: retried next time
		}
		cache.mu.Lock()
		defer cache.mu.Unlock()
		cache.entries[svc] = alertsEntry{at: time.Now(), list: list}
		return list, nil
	})
	list, _ := v.([]sonarr.HealthCheck)
	return list
}

// fetchAlerts reads the health alerts other than "ok"; on error the list is
// empty (the connection itself is reported by Check).
func fetchAlerts(ctx context.Context, c *Clients, svc string) ([]sonarr.HealthCheck, error) {
	ctx, cancel := context.WithTimeout(ctx, CheckTimeout)
	defer cancel()
	var (
		list []sonarr.HealthCheck
		err  error
	)
	switch {
	case svc == Sonarr && c.Sonarr != nil:
		list, err = c.Sonarr.Health(ctx)
	case svc == Radarr && c.Radarr != nil:
		list, err = c.Radarr.Health(ctx)
	}
	var out []sonarr.HealthCheck
	for _, a := range list {
		if a.Type != "ok" {
			out = append(out, a)
		}
	}
	return out, err
}
