package web

import (
	"errors"
	"net/http"
	"slices"
	"time"

	"arrstats/internal/store"
	"arrstats/internal/upstream"
)

// serviceForm is one section of the Configuration page.
type serviceForm struct {
	upstream.ServiceState
	CSRFToken       string
	CanStoreSecrets bool
	Errors          map[string]string
	Saved           bool
	NothingChanged  bool
	SyncStarted     bool
	Test            *testResult // connection test result without JavaScript
	WebhookURL      string      // Tautulli: address of the webhook (as seen by the browser)
	GeneratedSecret string      // Tautulli: new webhook secret, shown once
}

// WebhookState reports whether the webhook secret is set, and whether it
// comes from the environment.
func (f serviceForm) WebhookState() (set, locked bool) {
	for _, fs := range f.Fields {
		if fs.Key == upstream.TautulliWebhook {
			return fs.Hint != "", fs.Locked
		}
	}
	return false, false
}

// WebhookSet reports whether the webhook secret is set.
func (f serviceForm) WebhookSet() bool { set, _ := f.WebhookState(); return set }

// WebhookLocked reports whether the webhook secret comes from the environment.
func (f serviceForm) WebhookLocked() bool { _, locked := f.WebhookState(); return locked }

type configData struct {
	Services        []serviceForm
	CanStoreSecrets bool
	KeyInDataDir    bool
	HistorySource   string
}

// testResult is the outcome of « Tester la connexion ».
type testResult struct {
	Service string
	OK      bool
	Message string
}

func (s *Server) serviceForms(r *http.Request) ([]serviceForm, error) {
	if s.Services == nil {
		return nil, nil
	}
	states, err := s.Services.State(r.Context())
	if err != nil {
		return nil, err
	}
	out := make([]serviceForm, 0, len(states))
	csrf := ""
	if sess := sessionFrom(r); sess != nil {
		csrf = sess.CSRFToken
	}
	for _, st := range states {
		f := serviceForm{ServiceState: st, CSRFToken: csrf, CanStoreSecrets: s.Services.CanStoreSecrets()}
		if st.Service == upstream.Tautulli {
			f.WebhookURL = webhookURL(r)
		}
		out = append(out, f)
	}
	return out, nil
}

func (s *Server) configData(forms []serviceForm) configData {
	d := configData{Services: forms, HistorySource: s.Upstreams.Get().HistorySource}
	if s.Services != nil {
		d.CanStoreSecrets, d.KeyInDataDir = s.Services.CanStoreSecrets(), s.Services.KeyInDataDir
	}
	return d
}

func (s *Server) configPage(w http.ResponseWriter, r *http.Request) {
	forms, err := s.serviceForms(r)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "config", "", s.page(r, "Configuration", "config", s.configData(forms)))
}

// serviceInput reads the form of one service.
func serviceInput(r *http.Request, svc string) upstream.Input {
	in := upstream.Input{Values: map[string]string{}, Clear: map[string]bool{}}
	for _, f := range upstream.FieldsOf(svc) {
		in.Values[f.Key] = r.PostFormValue(f.Key)
		in.Clear[f.Key] = r.PostFormValue(f.Key+".clear") == "1"
	}
	return in
}

func (s *Server) configService(w http.ResponseWriter, r *http.Request) (string, bool) {
	svc := r.PathValue("service")
	if s.Services == nil || !slices.Contains(upstream.Services, svc) {
		s.notFound(w, r)
		return "", false
	}
	return svc, true
}

func (s *Server) configSubmit(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.configService(w, r)
	if !ok {
		return
	}
	in := serviceInput(r, svc)
	sess := sessionFrom(r)
	meta := s.clientMeta(r)
	ua := truncateUA(meta.UserAgent)
	wasReady := s.Upstreams.Get().Ready()
	changed, err := s.Services.Save(r.Context(), svc, in,
		store.AuthEvent{UserID: &sess.UserID, Username: sess.Username, IP: meta.IP, UserAgent: ua}, time.Now())
	var verr upstream.ValidationError
	if err != nil && !errors.As(err, &verr) {
		s.serverError(w, r, err)
		return
	}
	forms, ferr := s.serviceForms(r)
	if ferr != nil {
		s.serverError(w, r, ferr)
		return
	}
	idx := slices.IndexFunc(forms, func(f serviceForm) bool { return f.Service == svc })
	form := &forms[idx]
	status := http.StatusOK
	if verr != nil {
		status = http.StatusUnprocessableEntity
		form.Errors = verr
		// Keep what the user typed in the URL fields (never the credentials).
		for i, f := range form.Fields {
			if f.Kind == upstream.KindURL && !f.Locked {
				form.Fields[i].Value = in.Values[f.Key]
			}
		}
	} else {
		form.Saved, form.NothingChanged = len(changed) > 0, len(changed) == 0
		// First complete configuration: synchronize right away instead of
		// waiting for the daily schedule.
		if len(changed) > 0 && !wasReady && s.Upstreams.Get().Ready() && s.Sync != nil {
			form.SyncStarted = s.Sync.Start(s.baseContext())
		}
	}
	if isHTMX(r) {
		// The section is swapped; the banner and the sync status depend on
		// the configuration too and are swapped out of band.
		pd := s.page(r, "Configuration", "config", form)
		pd.OOB = true
		s.render(w, r, http.StatusOK, "config", "config_saved", pd)
		return
	}
	s.render(w, r, status, "config", "", s.page(r, "Configuration", "config", s.configData(forms)))
}

func (s *Server) configTest(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.configService(w, r)
	if !ok {
		return
	}
	res := testResult{Service: svc}
	version, err := s.Services.Test(r.Context(), svc, serviceInput(r, svc))
	var verr upstream.ValidationError
	switch {
	case errors.As(err, &verr):
		res.Message = "Adresse invalide : http(s) attendue."
	case err != nil:
		res.Message = upstream.Describe(err)
		s.Log.Info("connection test failed", "service", svc, "err", err)
	default:
		res.OK = true
		res.Message = "Connexion réussie"
		if version != "" {
			res.Message += " : " + upstream.ServiceLabel(svc) + " " + version
		}
		res.Message += "."
	}
	if isHTMX(r) {
		s.render(w, r, http.StatusOK, "config", "test_result", s.page(r, "Configuration", "config", res))
		return
	}
	// Without JavaScript: the page, with the result in the service section.
	forms, err := s.serviceForms(r)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	for i := range forms {
		if forms[i].Service == svc {
			forms[i].Test = &res
		}
	}
	s.render(w, r, http.StatusOK, "config", "", s.page(r, "Configuration", "config", s.configData(forms)))
}
