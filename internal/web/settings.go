package web

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"cleanarr/internal/i18n"
	"cleanarr/internal/store"
	"cleanarr/internal/syncer"
)

type settingsData struct {
	Hour    string // "04"
	Minute  string // "00"
	Every   int    // hours between syncs (24: once a day)
	Choices []int  // allowed intervals
	Times   string // "04:00, 10:00, 16:00, 22:00" (several syncs a day)
	NextRun string
	Error   string
	Saved   bool
}

// settingsPage is the data of the Réglages page: two independent forms.
type settingsPage struct {
	Sync    settingsData
	Cleanup cleanupSettings
}

func (s *Server) settingsData(l i18n.Localizer, errMsg string, saved bool) settingsData {
	d := settingsData{Error: errMsg, Saved: saved, Choices: syncer.EveryHours}
	if s.Sync != nil {
		sc := s.Sync.Schedule()
		at := sc.At
		d.Hour, d.Minute, d.Every = fmt.Sprintf("%02d", at.Hour), fmt.Sprintf("%02d", at.Minute), sc.EveryHours
		if sc.EveryHours < 24 {
			var times []string
			for _, t := range sc.Times() {
				times = append(times, t.String())
			}
			d.Times = strings.Join(times, ", ")
		}
		d.NextRun = l.DateTime(s.Sync.NextRun()) // already in the schedule time zone
	}
	return d
}

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request) {
	data := settingsPage{Sync: s.settingsData(s.loc(r), "", false), Cleanup: cleanupSettings{Days: s.cleanupDays(r.Context())}}
	s.render(w, r, http.StatusOK, "settings", "", s.page(r, "Settings", "settings", data))
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
		s.renderSettings(w, r, http.StatusUnprocessableEntity, "Invalid time: hour 0 to 23, minute 0 to 59.", false)
		return
	}
	every := 24 // field absent (older form): once a day
	if v := r.PostFormValue("sync_every"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || !syncer.ValidEvery(n) {
			s.renderSettings(w, r, http.StatusUnprocessableEntity, "Invalid frequency.", false)
			return
		}
		every = n
	}
	now := time.Now()
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
