package web

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"cleanarr/internal/store"
	"cleanarr/internal/syncer"
)

type settingsData struct {
	Hour    string // "04"
	Minute  string // "00"
	NextRun string
	Error   string
	Saved   bool
}

// settingsPage is the data of the Réglages page: two independent forms.
type settingsPage struct {
	Sync    settingsData
	Cleanup cleanupSettings
}

func (s *Server) settingsData(errMsg string, saved bool) settingsData {
	d := settingsData{Error: errMsg, Saved: saved}
	if s.Sync != nil {
		at := s.Sync.DailyTime()
		d.Hour, d.Minute = fmt.Sprintf("%02d", at.Hour), fmt.Sprintf("%02d", at.Minute)
		d.NextRun = s.Sync.NextRun().Format("02/01/2006 à 15:04") // already in the schedule time zone
	}
	return d
}

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request) {
	data := settingsPage{Sync: s.settingsData("", false), Cleanup: cleanupSettings{Days: s.cleanupDays(r.Context())}}
	s.render(w, r, http.StatusOK, "settings", "", s.page(r, "Réglages", "settings", data))
}

// parseClockFields reads the hour and minute fields; both accept one or two
// digits ("4" and "04"), typed or picked from the suggestions.
func parseClockFields(hour, minute string) (syncer.ClockTime, bool) {
	h, herr := strconv.Atoi(strings.TrimSpace(hour))
	m, merr := strconv.Atoi(strings.TrimSpace(minute))
	if herr != nil || merr != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return syncer.ClockTime{}, false
	}
	return syncer.ClockTime{Hour: h, Minute: m}, true
}

func (s *Server) settingsSubmit(w http.ResponseWriter, r *http.Request) {
	at, ok := parseClockFields(r.PostFormValue("sync_hour"), r.PostFormValue("sync_minute"))
	if !ok || s.Sync == nil {
		s.renderSettings(w, r, http.StatusUnprocessableEntity, "Heure invalide : heure entre 0 et 23, minute entre 0 et 59.", false)
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
	data := settingsPage{Sync: s.settingsData(errMsg, saved), Cleanup: cleanupSettings{Days: s.cleanupDays(r.Context())}}
	pd := s.page(r, "Réglages", "settings", data)
	if isHTMX(r) {
		s.render(w, r, http.StatusOK, "settings", "settings_form", pd)
		return
	}
	s.render(w, r, status, "settings", "", pd)
}
