package web

import (
	"net/http"
	"strings"
	"time"

	"arrstats/internal/store"
	"arrstats/internal/syncer"
)

type settingsData struct {
	SyncTime string
	NextRun  string
	Error    string
	Saved    bool
}

// settingsPage is the data of the Réglages page: two independent forms.
type settingsPage struct {
	Sync  settingsData
	Score scoreSettings
}

func (s *Server) settingsData(errMsg string, saved bool) settingsData {
	d := settingsData{Error: errMsg, Saved: saved}
	if s.Sync != nil {
		d.SyncTime = s.Sync.DailyTime().String()
		d.NextRun = s.Sync.NextRun().Format("02/01/2006 à 15:04") // already in the schedule time zone
	}
	return d
}

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request) {
	data := settingsPage{Sync: s.settingsData("", false), Score: s.scoreSettings(r.Context())}
	s.render(w, r, http.StatusOK, "settings", "", s.page(r, "Réglages", "settings", data))
}

func (s *Server) settingsSubmit(w http.ResponseWriter, r *http.Request) {
	at, err := syncer.ParseClockTime(strings.TrimSpace(r.PostFormValue("sync_time")))
	if err != nil || s.Sync == nil {
		s.renderSettings(w, r, http.StatusUnprocessableEntity, "Heure invalide : utilisez le format HH:MM (00:00 à 23:59).", false)
		return
	}
	if err := s.Store.SetSetting(r.Context(), store.SettingSyncTime, at.String(), time.Now()); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.Sync.SetDailyTime(at)
	s.Log.Info("daily sync time changed", "at", at.String(), "by", sessionFrom(r).Username)
	s.renderSettings(w, r, http.StatusOK, "", true)
}

func (s *Server) renderSettings(w http.ResponseWriter, r *http.Request, status int, errMsg string, saved bool) {
	data := settingsPage{Sync: s.settingsData(errMsg, saved), Score: s.scoreSettings(r.Context())}
	pd := s.page(r, "Réglages", "settings", data)
	if isHTMX(r) {
		s.render(w, r, http.StatusOK, "settings", "settings_form", pd)
		return
	}
	s.render(w, r, status, "settings", "", pd)
}
