package web

import (
	"net/http"
	"strings"
	"time"

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
	// Shrinks are the Sonarr/Radarr catalogs refused by the last sync
	// (store.CatalogShrinkError), to accept on this page.
	Shrinks []catalogShrink
}

// catalogShrink is a refused Sonarr/Radarr catalog.
type catalogShrink struct {
	Kind   string // series | movie
	Source string // sonarr | radarr
	Error  string
}

// shrinkSources maps the catalog sources to their kind.
var shrinkSources = map[string]string{"sonarr": "series", "radarr": "movie"}

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
	for _, run := range d.Latest {
		if kind, ok := shrinkSources[run.Source]; ok && run.Status == "error" &&
			strings.HasPrefix(run.Error, store.CatalogShrinkPrefix) {
			d.Shrinks = append(d.Shrinks, catalogShrink{Kind: kind, Source: run.Source, Error: run.Error})
		}
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

// acceptCatalog accepts the Sonarr/Radarr catalog the last sync refused
// (much smaller than the stored one) and starts a sync that applies it.
func (s *Server) acceptCatalog(w http.ResponseWriter, r *http.Request) {
	kind, ok := shrinkSources[r.PathValue("source")]
	if !ok {
		s.notFound(w, r)
		return
	}
	if err := s.Store.AcceptCatalogShrink(r.Context(), kind, time.Now()); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.Log.Warn("smaller catalog accepted: the next sync deletes the missing items", "source", r.PathValue("source"),
		"user", sessionFrom(r).Username)
	if s.Sync != nil {
		s.Sync.Start(s.baseContext())
	}
	http.Redirect(w, r, "/health", http.StatusSeeOther)
}
