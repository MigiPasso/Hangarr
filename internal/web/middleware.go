package web

import (
	"context"
	"errors"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"hangarr/internal/auth"
	"hangarr/internal/store"
)

const (
	sessionCookie    = "hangarr_session"
	preSessionCookie = "hangarr_presession"
	csrfHeader       = "X-CSRF-Token"
	csrfField        = "csrf_token"
)

// With COOKIE_SECURE, the cookies carry a prefix the browser enforces: a
// sibling subdomain can then neither set nor overwrite them. __Host- needs
// Path=/, so the pre-session cookie (Path=/login) gets __Secure-.
const (
	hostPrefix   = "__Host-"
	securePrefix = "__Secure-"
)

// cookieName is the name a cookie is written under.
func (s *Server) cookieName(name string) string {
	if !s.Config.CookieSecure {
		return name
	}
	if name == preSessionCookie {
		return securePrefix + name
	}
	return hostPrefix + name
}

// readCookie reads a cookie under its current name, else under its name
// before the prefixes (sessions opened before the upgrade stay valid).
func (s *Server) readCookie(r *http.Request, name string) (string, bool) {
	if c, err := r.Cookie(s.cookieName(name)); err == nil {
		return c.Value, true
	}
	if c, err := r.Cookie(name); err == nil {
		return c.Value, true
	}
	return "", false
}

const contentSecurityPolicy = "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; " +
	"object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'"

const permissionsPolicy = "accelerometer=(), camera=(), geolocation=(), gyroscope=(), magnetometer=(), " +
	"microphone=(), payment=(), usb=(), browsing-topics=()"

type ctxKey int

const sessionKey ctxKey = iota

func sessionFrom(r *http.Request) *store.Session {
	s, _ := r.Context().Value(sessionKey).(*store.Session)
	return s
}

func isHTMX(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

// redirect sends a 303, or an HX-Redirect header for HTMX requests so that
// the browser navigates instead of swapping a page into a fragment.
func redirect(w http.ResponseWriter, r *http.Request, to string) {
	if isHTMX(r) {
		w.Header().Set("HX-Redirect", to)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

func isUnsafe(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return true
}

func (s *Server) clientMeta(r *http.Request) auth.ClientMeta {
	ip := s.ips.ClientIP(r)
	meta := auth.ClientMeta{UserAgent: r.UserAgent()}
	if ip.IsValid() {
		meta.IP = ip.String()
	}
	return meta
}

// setCookie writes an HttpOnly, SameSite=Lax cookie. maxAge < 0 deletes it,
// 0 makes it a browser-session cookie.
func (s *Server) setCookie(w http.ResponseWriter, name, value, path string, maxAge int) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure follows COOKIE_SECURE (false only for local dev without TLS)
		Name: s.cookieName(name), Value: value, Path: path, MaxAge: maxAge,
		HttpOnly: true, Secure: s.Config.CookieSecure, SameSite: http.SameSiteLaxMode,
	})
	if maxAge < 0 && s.cookieName(name) != name {
		// Also drop the cookie written before the prefixes.
		http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: deletion
			Name: name, Path: path, MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
		})
	}
}

func (s *Server) setSessionCookie(w http.ResponseWriter, token string) {
	s.setCookie(w, sessionCookie, token, "/", int(s.Config.SessionMaxAge/time.Second))
}

func (s *Server) clearCookie(w http.ResponseWriter, name string) {
	s.setCookie(w, name, "", "/", -1)
}

type authFlags int

// allowMustChange lets a route through while the password must be changed.
const allowMustChange authFlags = 1

// authed requires a valid session, enforces the forced password change and
// checks the CSRF token on unsafe methods.
func (s *Server) authed(h http.HandlerFunc, flags authFlags) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, _ := s.readCookie(r, sessionCookie)
		sess, err := s.Auth.Authenticate(r.Context(), token)
		if err != nil {
			if !errors.Is(err, auth.ErrSessionInvalid) {
				s.serverError(w, r, err)
				return
			}
			if token != "" {
				s.clearCookie(w, sessionCookie)
			}
			redirect(w, r, "/login")
			return
		}
		if sess.MustChangePassword && flags&allowMustChange == 0 {
			redirect(w, r, "/account/password")
			return
		}
		if isUnsafe(r.Method) {
			sent := r.Header.Get(csrfHeader)
			if sent == "" {
				sent = r.PostFormValue(csrfField)
			}
			if !auth.TokensEqual(sess.CSRFToken, sent) {
				s.Log.Warn("csrf token rejected", "path", r.URL.Path, "ip", s.clientMeta(r).IP)
				http.Error(w, s.loc(r).T("Invalid or missing CSRF token. Reload the page."), http.StatusForbidden)
				return
			}
		}
		h(w, r.WithContext(context.WithValue(r.Context(), sessionKey, sess)))
	})
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Permissions-Policy", permissionsPolicy)
		h.Set("X-Frame-Options", "DENY")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		// HSTS only on HTTPS (as the browser saw it) and for this host only:
		// includeSubDomains on a parent domain (maison.lan) would force HTTPS
		// on every service below it for a year.
		if s.Config.CookieSecure && s.ips.SecureRequest(r) {
			v := "max-age=31536000"
			if s.Config.HSTSIncludeSubdomains {
				v += "; includeSubDomains"
			}
			h.Set("Strict-Transport-Security", v)
		}
		if !strings.HasPrefix(r.URL.Path, "/static/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK // nothing written: net/http answers 200
		}
		attrs := []any{
			"method", r.Method, "path", r.URL.Path, "status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(), "ip", s.clientMeta(r).IP,
		}
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" || r.URL.Path == "/favicon.ico" ||
			strings.HasPrefix(r.URL.Path, "/static/") {
			s.Log.Debug("http request", attrs...)
			return
		}
		s.Log.Info("http request", attrs...)
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w}
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler { //nolint:errorlint // sentinel compared as in net/http
					panic(v)
				}
				s.Log.Error("panic in handler", "panic", v, "stack", string(debug.Stack()))
				if rec.status != 0 {
					// The answer has started: a 500 can no longer be sent,
					// cut the connection so the client sees it failed.
					panic(http.ErrAbortHandler)
				}
				http.Error(w, s.loc(r).T("Internal error."), http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(rec, r)
	})
}

func (s *Server) serverError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	s.Log.Error("request failed", "path", r.URL.Path, "err", err)
	http.Error(w, s.loc(r).T("Internal error."), http.StatusInternalServerError)
}
