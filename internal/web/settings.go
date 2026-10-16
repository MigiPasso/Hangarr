package web

import (
	"net/http"
	"strconv"
	"time"

	"hangarr/internal/i18n"
	"hangarr/internal/store"
	"hangarr/internal/syncer"
)

type settingsData struct {
	Every   int   // hours between syncs (24: once a day)
	Choices []int // allowed intervals
	NextRun string
	Error   string
	Saved   bool
}

// settingsPage is the data of the Settings page: two independent forms.
type settingsPage struct {
	Sync    settingsData
	Cleanup cleanupSettings
}

func (s *Server) settingsData(l i18n.Localizer, errMsg string, saved bool) settingsData {
	d := settingsData{Error: errMsg, Saved: saved, Choices: syncer.EveryHours}
	if s.Sync != nil {
		d.Every = s.Sync.Schedule().EveryHours
		d.NextRun = l.DateTime(s.Sync.NextRun()) // already in the schedule time zone
	}
	return d
}

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request) {
	data := settingsPage{Sync: s.settingsData(s.loc(r), "", false), Cleanup: cleanupSettings{Days: s.cleanupDays(r.Context())}}
	s.render(w, r, http.StatusOK, "settings", "", s.page(r, "Settings", "settings", data))
}

func (s *Server) settingsSubmit(w http.ResponseWriter, r *http.Request) {
	if s.Sync == nil {
		s.renderSettings(w, r, http.StatusServiceUnavailable, "Synchronization unavailable.", false)
		return
	}
	every, err := strconv.Atoi(r.PostFormValue("sync_every"))
	if err != nil || !syncer.ValidEvery(every) {
		s.renderSettings(w, r, http.StatusUnprocessableEntity, "Invalid frequency.", false)
		return
	}
	// The syncs start from the time of the save, then follow the frequency.
	now := time.Now().In(s.Config.Location)
	at := syncer.ClockTime{Hour: now.Hour(), Minute: now.Minute()}
	if err := s.Store.SetSetting(r.Context(), store.SettingSyncTime, at.String(), now); err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := s.Store.SetSetting(r.Context(), store.SettingSyncEvery, strconv.Itoa(every), now); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.Sync.SetSchedule(syncer.Schedule{At: at, EveryHours: every})
	s.Log.Info("sync schedule changed", "at", at.String(), "every_hours", every, "by", sessionFrom(r).Username)
	s.renderSettings(w, r, http.StatusOK, "", true)
}

func (s *Server) renderSettings(w http.ResponseWriter, r *http.Request, status int, errMsg string, saved bool) {
	data := settingsPage{Sync: s.settingsData(s.loc(r), errMsg, saved), Cleanup: cleanupSettings{Days: s.cleanupDays(r.Context())}}
	pd := s.page(r, "Settings", "settings", data)
	if isHTMX(r) {
		s.render(w, r, http.StatusOK, "settings", "settings_form", pd)
		return
	}
	s.render(w, r, status, "settings", "", pd)
}
