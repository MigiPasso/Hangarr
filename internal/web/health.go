package web

import (
	"net/http"

	"cleanarr/internal/store"
	"cleanarr/internal/upstream"
)

// healthListLimit caps each data list of the Santé page.
const healthListLimit = 50

type healthData struct {
	Services []upstream.ServiceStatus // nil: loaded by HTMX
	Latest   []store.SyncRun
	Runs     []store.SyncRun
	Data     *store.DataHealth
}

// health is the Santé page; the live service checks load separately
// (healthServices) so that an unreachable service never delays the page.
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	s.renderHealth(w, r, nil)
}

func (s *Server) renderHealth(w http.ResponseWriter, r *http.Request, services []upstream.ServiceStatus) {
	ctx := r.Context()
	d := healthData{Services: services}
	var err error
	if d.Latest, err = s.Store.LatestSyncRuns(ctx); err != nil {
		s.serverError(w, r, err)
		return
	}
	if d.Runs, err = s.Store.RecentSyncRuns(ctx, 40); err != nil {
		s.serverError(w, r, err)
		return
	}
	if d.Data, err = s.Store.DataHealth(ctx, healthListLimit); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "health", "", s.page(r, "Health", "health", d))
}

// healthServices checks every service live: an HTMX fragment, or the whole
// page without JavaScript.
func (s *Server) healthServices(w http.ResponseWriter, r *http.Request) {
	statuses := upstream.CheckAll(r.Context(), s.Upstreams.Get(), s.loc(r))
	if isHTMX(r) {
		s.render(w, r, http.StatusOK, "health", "health_services_partial", s.page(r, "Health", "health", statuses))
		return
	}
	s.renderHealth(w, r, statuses)
}
