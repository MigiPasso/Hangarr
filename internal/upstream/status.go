package upstream

import (
	"context"
	"sync"
	"time"

	"cleanarr/internal/clients/sonarr"
)

// ServiceStatus is the live state of one service (Santé page).
type ServiceStatus struct {
	Service    string
	Label      string
	Configured bool
	OK         bool
	Version    string
	Latency    time.Duration
	Error      string               // explained in French (Describe)
	Alerts     []sonarr.HealthCheck // Sonarr/Radarr health alerts other than "ok"
}

// CheckAll tests every service in parallel; each check is bounded by
// CheckTimeout.
func CheckAll(ctx context.Context, c *Clients) []ServiceStatus {
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
			st.OK, st.Version, st.Error = err == nil, version, Describe(err)
			if err == nil {
				st.Alerts = alerts(ctx, c, st.Service)
			}
		}(&out[i])
	}
	wg.Wait()
	return out
}

// alerts returns the Sonarr/Radarr health alerts; failures are ignored (the
// connection itself was checked).
func alerts(ctx context.Context, c *Clients, svc string) []sonarr.HealthCheck {
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
