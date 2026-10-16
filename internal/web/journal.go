package web

import (
	"fmt"
	"net/http"
	"slices"

	"hangarr/internal/store"
)

// --- journal ------------------------------------------------------------------

const journalPageSize = 50

type journalData struct {
	Tab     string // actions | security
	Actions []store.Action
	Events  []store.AuthEvent
	Page    int
	PrevURL string
	NextURL string
}

// authEventLabels name the security events (English keys, translated by the templates).
var authEventLabels = map[string]string{ //nolint:gosec // G101: UI labels, not credentials
	"admin_created": "Admin account created", "login_success": "Sign-in", "login_failure": "Failed sign-in",
	"lockout": "Account locked", "password_changed": "Password changed",
	"password_reset_cli": "Password reset (CLI)", "logout": "Sign-out", "session_revoked": "Session revoked",
	"config_sonarr": "Sonarr configuration changed", "config_radarr": "Radarr configuration changed",
	"config_plex": "Plex configuration changed", "config_tautulli": "Tautulli configuration changed",
	"config_jellyfin": "Jellyfin configuration changed",
	"reauth":          "Password confirmed", "reauth_failure": "Failed password confirmation",
}

func (s *Server) journal(w http.ResponseWriter, r *http.Request) {
	tab := r.URL.Query().Get("tab")
	if !slices.Contains([]string{"actions", "security"}, tab) {
		tab = "actions"
	}
	page := min(max(atoi(r.URL.Query().Get("page")), 1), maxPage)
	d := journalData{Tab: tab, Page: page}
	offset := (page - 1) * journalPageSize
	var more bool
	var err error
	if tab == "actions" {
		d.Actions, more, err = s.Store.Actions(r.Context(), offset, journalPageSize)
	} else {
		d.Events, more, err = s.Store.AuthEventsPage(r.Context(), offset, journalPageSize)
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if page > 1 {
		d.PrevURL = fmt.Sprintf("/journal?tab=%s&page=%d", tab, page-1)
	}
	if more {
		d.NextURL = fmt.Sprintf("/journal?tab=%s&page=%d", tab, page+1)
	}
	s.render(w, r, http.StatusOK, "journal", "", s.page(r, "Log", "journal", d))
}
