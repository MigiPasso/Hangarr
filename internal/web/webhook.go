package web

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"time"

	"hangarr/internal/store"
	"hangarr/internal/upstream"
)

// Tautulli webhook limits.
const (
	webhookHeader = "X-Hangarr-Token"
	// legacyWebhookHeader is the header of Cleanarr (the former name), still
	// accepted so that a Tautulli configured before the rename keeps working.
	legacyWebhookHeader = "X-Cleanarr-Token"
	webhookMaxBody      = 64 << 10
	webhookPerMinute    = 60
)

// tautulliWebhook (POST /webhooks/tautulli) is the only route without a
// session: it is disabled (404) without a secret or while Jellyfin is the
// media server (Tautulli is not read then), checks the secret in constant
// time, ignores the body and only schedules a history sync.
func (s *Server) tautulliWebhook(w http.ResponseWriter, r *http.Request) {
	up := s.Upstreams.Get()
	secret := up.WebhookSecret()
	if secret == "" || up.MediaServer != upstream.Plex {
		http.NotFound(w, r)
		return
	}
	ip := s.clientMeta(r).IP
	if ok, retry := s.webhookLimiter.Allow(ip); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	// The payload is never trusted nor parsed: the history is read back
	// from the Tautulli API.
	_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, webhookMaxBody))
	token, legacy := r.Header.Get(webhookHeader), false
	if token == "" {
		token, legacy = r.Header.Get(legacyWebhookHeader), true
	}
	got := sha256.Sum256([]byte(token))
	want := sha256.Sum256([]byte(secret))
	if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
		s.Log.Warn("tautulli webhook rejected: bad token", "ip", ip)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	scheduled := false
	if s.Sync != nil {
		scheduled = s.Sync.RequestHistorySync(s.baseContext())
	}
	s.Log.Info("tautulli webhook received", "ip", ip, "scheduled", scheduled)
	if legacy {
		s.Log.Warn("tautulli webhook uses the deprecated header " + legacyWebhookHeader + ": rename it " + webhookHeader + " in Tautulli")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_, _ = io.WriteString(w, `{"status":"accepted"}`)
}

// webhookURL is the address shown in the Configuration page; Tautulli must
// use the address under which it reaches hangarr.
func webhookURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/webhooks/tautulli"
}

// webhookSecretSubmit generates and saves a new webhook secret, shown once.
func (s *Server) webhookSecretSubmit(w http.ResponseWriter, r *http.Request) {
	if s.Services == nil {
		s.notFound(w, r)
		return
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		s.serverError(w, r, err)
		return
	}
	secret := base64.RawURLEncoding.EncodeToString(b)
	in := upstream.Input{Values: map[string]string{upstream.TautulliWebhook: secret}}
	// Keep the other Tautulli fields as they are: URL fields are rewritten
	// by Save, so pass their current values.
	cur := s.Upstreams.Get().Values
	for _, f := range upstream.FieldsOf(upstream.Tautulli) {
		if f.Kind == upstream.KindURL {
			in.Values[f.Key] = cur[f.Key]
		}
	}
	sess := sessionFrom(r)
	meta := s.clientMeta(r)
	_, err := s.Services.Save(r.Context(), upstream.Tautulli, in,
		store.AuthEvent{UserID: &sess.UserID, Username: sess.Username, IP: meta.IP, UserAgent: truncateUA(meta.UserAgent)}, time.Now())
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
	idx := slices.IndexFunc(forms, func(f serviceForm) bool { return f.Service == upstream.Tautulli })
	form := &forms[idx]
	if verr != nil {
		form.Errors = verr
	} else {
		form.GeneratedSecret = secret
	}
	if isHTMX(r) {
		pd := s.page(r, "Services", "config", savedForm{Form: *form})
		pd.OOB = true
		s.render(w, r, http.StatusOK, "config", "config_saved", pd)
		return
	}
	s.render(w, r, http.StatusOK, "config", "", s.page(r, "Services", "config", s.configData(forms)))
}

func truncateUA(ua string) string {
	if len(ua) > 256 {
		return ua[:256]
	}
	return ua
}
