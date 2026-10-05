package web

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"cleanarr/internal/auth"
	"cleanarr/internal/store"
)

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if s.draining.Load() {
		http.Error(w, "shutting down", http.StatusServiceUnavailable)
		return
	}
	if err := s.Store.Ready(r.Context()); err != nil {
		s.Log.Warn("readiness check failed", "err", err)
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusNotFound, "notfound", "", s.page(r, "Page introuvable", "", nil))
}

// --- login -----------------------------------------------------------------

type loginData struct {
	Username string
	Error    string
	// InsecureCookies: the page is served over plain HTTP while
	// COOKIE_SECURE=true, so the browser will drop the session cookie.
	InsecureCookies bool
}

// insecureCookieSetup reports a COOKIE_SECURE=true deployment reached over
// plain HTTP. Browsers accept Secure cookies on localhost, so it is excluded.
func (s *Server) insecureCookieSetup(r *http.Request) bool {
	if !s.Config.CookieSecure || r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		return false
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	return host != "localhost" && host != "127.0.0.1" && host != "::1"
}

// preSession returns the pre-session cookie value, creating it if needed.
func (s *Server) preSession(w http.ResponseWriter, r *http.Request) (string, error) {
	if c, err := r.Cookie(preSessionCookie); err == nil && len(c.Value) == 43 {
		return c.Value, nil
	}
	v, err := auth.NewToken()
	if err != nil {
		return "", err
	}
	s.setCookie(w, preSessionCookie, v, "/login", 0)
	return v, nil
}

func (s *Server) renderLogin(w http.ResponseWriter, r *http.Request, status int, data loginData) {
	pre, err := s.preSession(w, r)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	data.InsecureCookies = s.insecureCookieSetup(r)
	pd := s.page(r, "Connexion", "", data)
	pd.CSRFToken = auth.PreSessionCSRF(s.PreSessionKey, pre)
	s.render(w, r, status, "login", "", pd)
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		if _, err := s.Auth.Authenticate(r.Context(), c.Value); err == nil {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
	}
	s.renderLogin(w, r, http.StatusOK, loginData{})
}

func (s *Server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	meta := s.clientMeta(r)
	username := r.PostFormValue("username")
	if ok, retry := s.LoginLimiter.Allow(meta.IP); !ok {
		mins := int(retry/time.Minute) + 1
		w.Header().Set("Retry-After", strconv.Itoa(int(retry/time.Second)+1))
		s.Log.Warn("login rate limited", "ip", meta.IP)
		s.renderLogin(w, r, http.StatusTooManyRequests, loginData{Username: username,
			Error: fmt.Sprintf("Trop de tentatives. Réessayez dans %d min.", mins)})
		return
	}
	var pre string
	if c, err := r.Cookie(preSessionCookie); err == nil {
		pre = c.Value
	}
	if !auth.CheckPreSessionCSRF(s.PreSessionKey, pre, r.PostFormValue(csrfField)) {
		s.Log.Warn("login csrf token rejected", "ip", meta.IP)
		s.renderLogin(w, r, http.StatusForbidden, loginData{Username: username,
			Error: "Votre session de connexion a expiré. Veuillez réessayer."})
		return
	}
	ns, err := s.Auth.Login(r.Context(), username, r.PostFormValue("password"), meta)
	if errors.Is(err, auth.ErrInvalidCredentials) {
		s.renderLogin(w, r, http.StatusUnauthorized, loginData{Username: username, Error: "Identifiants invalides."})
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.setSessionCookie(w, ns.Token)
	s.clearPreSession(w)
	if ns.Session.MustChangePassword {
		http.Redirect(w, r, "/account/password", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) clearPreSession(w http.ResponseWriter) {
	s.setCookie(w, preSessionCookie, "", "/login", -1)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if err := s.Auth.Logout(r.Context(), sessionFrom(r), s.clientMeta(r)); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.clearCookie(w, sessionCookie)
	redirect(w, r, "/login")
}

// --- password change -------------------------------------------------------

type passwordData struct {
	Forced       bool
	Username     string
	CurrentError string
	NewErrors    []string
	ConfirmError string
}

var violationMessages = map[auth.Violation]string{
	auth.ViolationTooShort:         fmt.Sprintf("Le mot de passe doit contenir au moins %d caractères.", auth.MinPasswordLength),
	auth.ViolationTooLong:          fmt.Sprintf("Le mot de passe doit contenir au plus %d caractères.", auth.MaxPasswordLength),
	auth.ViolationNoLower:          "Il faut au moins une lettre minuscule.",
	auth.ViolationNoUpper:          "Il faut au moins une lettre majuscule.",
	auth.ViolationNoDigit:          "Il faut au moins un chiffre.",
	auth.ViolationNoSpecial:        "Il faut au moins un caractère spécial.",
	auth.ViolationContainsUsername: "Le mot de passe ne doit pas contenir le nom d'utilisateur.",
	auth.ViolationCommon:           "Ce mot de passe est trop courant.",
	auth.ViolationSameAsCurrent:    "Le nouveau mot de passe doit être différent de l'actuel.",
}

func (s *Server) passwordPage(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)
	data := passwordData{Forced: sess.MustChangePassword, Username: sess.Username}
	s.render(w, r, http.StatusOK, "password", "", s.page(r, "Changer le mot de passe", "account", data))
}

func (s *Server) passwordSubmit(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)
	// The current password is checked here too: same per-IP budget as the
	// login, so a stolen session cannot guess it without limit.
	if ok, retry := s.LoginLimiter.Allow(s.clientMeta(r).IP); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(retry/time.Second)+1))
		data := passwordData{Forced: sess.MustChangePassword, Username: sess.Username,
			CurrentError: fmt.Sprintf("Trop de tentatives. Réessayez dans %d min.", int(retry/time.Minute)+1)}
		pd := s.page(r, "Changer le mot de passe", "account", data)
		if isHTMX(r) {
			s.render(w, r, http.StatusOK, "password", "password_form", pd)
			return
		}
		s.render(w, r, http.StatusTooManyRequests, "password", "", pd)
		return
	}
	ns, violations, err := s.Auth.ChangePassword(r.Context(), sess,
		r.PostFormValue("current_password"), r.PostFormValue("new_password"), r.PostFormValue("confirm_password"),
		s.clientMeta(r))
	data := passwordData{Forced: sess.MustChangePassword, Username: sess.Username}
	switch {
	case errors.Is(err, auth.ErrWrongPassword):
		data.CurrentError = "Mot de passe actuel incorrect."
	case err != nil:
		s.serverError(w, r, err)
		return
	case len(violations) > 0:
		for _, v := range violations {
			if v == auth.ViolationMismatch {
				data.ConfirmError = "La confirmation ne correspond pas."
				continue
			}
			data.NewErrors = append(data.NewErrors, violationMessages[v])
		}
	default:
		s.setSessionCookie(w, ns.Token)
		redirect(w, r, "/")
		return
	}
	pd := s.page(r, "Changer le mot de passe", "account", data)
	if isHTMX(r) {
		s.render(w, r, http.StatusOK, "password", "password_form", pd)
		return
	}
	s.render(w, r, http.StatusUnprocessableEntity, "password", "", pd)
}

// --- account and sessions --------------------------------------------------

type accountData struct {
	Sessions  []store.Session
	CurrentID int64
}

func (s *Server) accountPage(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)
	list, err := s.Auth.Sessions(r.Context(), sess.UserID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "account", "",
		s.page(r, "Compte", "account", accountData{Sessions: list, CurrentID: sess.ID}))
}

func (s *Server) revokeSession(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "Session inconnue", http.StatusNotFound)
		return
	}
	if id == sess.ID {
		s.logout(w, r)
		return
	}
	ok, err := s.Auth.RevokeSession(r.Context(), sess, id, s.clientMeta(r))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if !ok {
		http.Error(w, "Session inconnue", http.StatusNotFound)
		return
	}
	if isHTMX(r) {
		// The row is swapped with nothing.
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/account", http.StatusSeeOther)
}
