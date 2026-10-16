package web

import (
	"fmt"
	"net/http"

	"hangarr/internal/actions"
)

// protectSubmit adds or removes the protection tag ("Protect" / "Unprotect"
// on the detail page).
func (s *Server) protectSubmit(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m, ok := s.mediaFromPath(w, r, kind)
		if !ok {
			return
		}
		if s.Actions == nil {
			http.Error(w, s.loc(r).T("Protection unavailable."), http.StatusServiceUnavailable)
			return
		}
		protect := r.PostFormValue("protect") == "1"
		detailURL := fmt.Sprintf("%s/%d", kindPath(kind), m.ID)
		res, err := s.Actions.SetProtected(r.Context(), actions.ProtectRequest{MediaID: m.ID, Protect: protect,
			Username: sessionFrom(r).Username})
		l := s.loc(r)
		var msg, level string
		switch {
		case err != nil && protect:
			msg, level = explain(l, err, "Protection failed: %s"), "error"
		case err != nil:
			msg, level = explain(l, err, "Unprotecting failed: %s"), "error"
		case res.Unchanged && protect:
			msg, level = l.T("“%s” is already protected.", res.Title), "info"
		case res.Unchanged:
			msg, level = l.T("“%s” is not protected.", res.Title), "info"
		case res.Local && protect:
			msg, level = l.T("“%s” is protected in Hangarr: it can no longer be deleted. %s is not changed.", res.Title, s.Upstreams.Get().ServerLabel()), "success"
		case res.Local:
			msg, level = l.T("“%s” is no longer protected.", res.Title), "success"
		case res.DryRun && protect:
			msg, level = l.T("Simulation: “%s” would have received the %s tag. Nothing changed.", res.Title, res.Tag), "info"
		case res.DryRun:
			msg, level = l.T("Simulation: the %s tag would have been removed from “%s”. Nothing changed.", res.Tag, res.Title), "info"
		case protect:
			msg, level = l.T("“%s” is protected (%s tag): it will no longer be proposed for cleanup.", res.Title, res.Tag), "success"
		default:
			msg, level = l.T("“%s” is no longer protected (%s tag removed).", res.Title, res.Tag), "success"
		}
		if !isHTMX(r) {
			if err != nil {
				http.Error(w, msg, http.StatusUnprocessableEntity)
				return
			}
			http.Redirect(w, r, detailURL, http.StatusSeeOther)
			return
		}
		trigger := map[string]any{"hangarr:toast": map[string]any{"message": msg, "level": level}}
		// Unchanged too: the local tags were realigned with Sonarr/Radarr.
		if err == nil && (!res.DryRun || res.Unchanged) {
			trigger["hangarr:changed"] = map[string]any{"detailUrl": detailURL}
		}
		w.Header().Set("HX-Trigger", asciiJSON(trigger))
		w.Header().Set("HX-Reswap", "none")
		w.WriteHeader(http.StatusOK)
	}
}
