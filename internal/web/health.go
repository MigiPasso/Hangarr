package web

import (
	"net/http"

	"hangarr/internal/store"
	"hangarr/internal/upstream"
)

// healthListLimit caps each data list of the Health page.
const healthListLimit = 50

type healthData struct {
	Services []upstream.ServiceStatus // nil: loaded by HTMX
	Trackers []upstream.TrackerStatus // nil: loaded by HTMX
	Latest   []store.SyncRun
	Runs     []store.SyncRun
	Data     *store.DataHealth
}

// health is the Health page; the live checks of the services and of the
// trackers load separately (healthServices, healthTrackers) so that an
// unreachable service never delays the page.
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	s.renderHealth(w, r, healthData{})
}

func (s *Server) renderHealth(w http.ResponseWriter, r *http.Request, d healthData) {
	ctx := r.Context()
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
// page without JavaScript (with the trackers too).
func (s *Server) healthServices(w http.ResponseWriter, r *http.Request) {
	up, l := s.Upstreams.Get(), s.loc(r)
	statuses := upstream.CheckAll(r.Context(), up, l)
	if isHTMX(r) {
		s.render(w, r, http.StatusOK, "health", "health_services_partial", s.page(r, "Health", "health", statuses))
		return
	}
	s.renderHealth(w, r, healthData{Services: statuses, Trackers: upstream.CheckTrackers(r.Context(), up, l)})
}

// healthTrackers reads the indexers of Sonarr and Radarr live (HTMX
// fragment; without JavaScript, see healthServices).
func (s *Server) healthTrackers(w http.ResponseWriter, r *http.Request) {
	trackers := upstream.CheckTrackers(r.Context(), s.Upstreams.Get(), s.loc(r))
	if isHTMX(r) {
		s.render(w, r, http.StatusOK, "health", "health_trackers_partial", s.page(r, "Health", "health", trackers))
		return
	}
	s.renderHealth(w, r, healthData{Trackers: trackers, Services: upstream.CheckAll(r.Context(), s.Upstreams.Get(), s.loc(r))})
}
