package web

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"cleanarr/internal/auth"
	"cleanarr/internal/config"
	"cleanarr/internal/store"
)

var (
	csrfRe     = regexp.MustCompile(`name="csrf_token" value="([^"]+)"`)
	passwordRe = regexp.MustCompile(`(?m)^ Mot de passe : (\S+)$`)
)

const newPassword = "N3w-Strong-Pass!"

type harness struct {
	t        *testing.T
	srv      *Server
	ts       *httptest.Server
	password string
}

func newHarness(t *testing.T, mutate func(*config.Config), opts ...func(*Deps)) *harness {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg, err := config.Load(func(string) (string, bool) { return "", false }, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.CookieSecure = false
	if mutate != nil {
		mutate(cfg)
	}
	h, err := auth.NewHasher(auth.HashParams{Memory: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32})
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := &auth.Service{Store: st, Hasher: h, Log: log, IdleTimeout: cfg.SessionIdleTimeout, MaxAge: cfg.SessionMaxAge}
	var banner bytes.Buffer
	if _, err := svc.Bootstrap(ctx, cfg.AdminUsername, &banner); err != nil {
		t.Fatal(err)
	}
	m := passwordRe.FindStringSubmatch(banner.String())
	if m == nil {
		t.Fatal("no password in banner")
	}
	deps := Deps{
		Config: cfg, Auth: svc, Store: st, Log: log, Version: "test",
		PreSessionKey: []byte("test-key"),
		LoginLimiter:  auth.NewRateLimiter(cfg.LoginMaxPerIP, auth.LoginWindow, nil),
	}
	for _, o := range opts {
		o(&deps)
	}
	srv, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &harness{t: t, srv: srv, ts: ts, password: m[1]}
}

func (h *harness) client() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

type resp struct {
	status int
	header http.Header
	body   string
}

func (h *harness) do(c *http.Client, method, path string, form url.Values, headers map[string]string) resp {
	h.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, h.ts.URL+path, body)
	if err != nil {
		h.t.Fatal(err)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := c.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(res.Body)
	return resp{status: res.StatusCode, header: res.Header, body: string(b)}
}

func csrfFrom(t *testing.T, body string) string {
	t.Helper()
	m := csrfRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no csrf token in page:\n%s", body)
	}
	return m[1]
}

// login performs GET /login then POST /login and returns the POST response.
func (h *harness) login(c *http.Client, user, pw string, headers map[string]string) resp {
	h.t.Helper()
	page := h.do(c, "GET", "/login", nil, headers)
	return h.do(c, "POST", "/login", url.Values{
		"csrf_token": {csrfFrom(h.t, page.body)}, "username": {user}, "password": {pw},
	}, headers)
}

// changePassword logs in with the initial password and sets newPassword.
func (h *harness) loggedIn() *http.Client {
	h.t.Helper()
	c := h.client()
	if r := h.login(c, "admin", h.password, nil); r.status != http.StatusSeeOther {
		h.t.Fatalf("login: %d", r.status)
	}
	page := h.do(c, "GET", "/account/password", nil, nil)
	r := h.do(c, "POST", "/account/password", url.Values{
		"csrf_token": {csrfFrom(h.t, page.body)}, "current_password": {h.password},
		"new_password": {newPassword}, "confirm_password": {newPassword},
	}, nil)
	if r.status != http.StatusSeeOther || r.header.Get("Location") != "/" {
		h.t.Fatalf("password change: %d %s\n%s", r.status, r.header.Get("Location"), r.body)
	}
	return c
}

func TestPublicEndpoints(t *testing.T) {
	h := newHarness(t, nil)
	c := h.client()
	for _, p := range []string{"/healthz", "/readyz", "/login"} {
		if r := h.do(c, "GET", p, nil, nil); r.status != http.StatusOK {
			t.Errorf("%s: %d", p, r.status)
		}
	}
	if r := h.do(c, "GET", "/static/app.css", nil, nil); r.status != http.StatusOK ||
		!strings.Contains(r.header.Get("Content-Type"), "text/css") {
		t.Errorf("static: %d %s", r.status, r.header.Get("Content-Type"))
	}
	h.srv.SetDraining()
	if r := h.do(c, "GET", "/readyz", nil, nil); r.status != http.StatusServiceUnavailable {
		t.Errorf("readyz while draining: %d", r.status)
	}
	if r := h.do(c, "GET", "/healthz", nil, nil); r.status != http.StatusOK {
		t.Errorf("healthz while draining: %d", r.status)
	}
}

func TestSecurityHeaders(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.CookieSecure = true })
	r := h.do(h.client(), "GET", "/login", nil, nil)
	want := map[string]string{
		"Content-Security-Policy":   contentSecurityPolicy,
		"X-Content-Type-Options":    "nosniff",
		"Referrer-Policy":           "same-origin",
		"Strict-Transport-Security": "max-age=31536000; includeSubDomains",
		"Cache-Control":             "no-store",
	}
	for k, v := range want {
		if got := r.header.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if r.header.Get("Permissions-Policy") == "" {
		t.Error("Permissions-Policy missing")
	}
	if !strings.Contains(r.body, `"allowEval":false`) {
		t.Error("htmx-config meta missing")
	}
}

func TestAnonymousIsRedirected(t *testing.T) {
	h := newHarness(t, nil)
	c := h.client()
	for _, p := range []string{"/", "/account", "/account/password", "/does-not-exist"} {
		r := h.do(c, "GET", p, nil, nil)
		if r.status != http.StatusSeeOther || r.header.Get("Location") != "/login" {
			t.Errorf("%s: %d %s", p, r.status, r.header.Get("Location"))
		}
	}
	r := h.do(c, "GET", "/", nil, map[string]string{"HX-Request": "true"})
	if r.header.Get("HX-Redirect") != "/login" {
		t.Errorf("htmx anonymous: HX-Redirect=%q", r.header.Get("HX-Redirect"))
	}
}

func TestLoginRequiresCSRF(t *testing.T) {
	h := newHarness(t, nil)
	c := h.client()
	h.do(c, "GET", "/login", nil, nil)
	r := h.do(c, "POST", "/login", url.Values{"username": {"admin"}, "password": {h.password}}, nil)
	if r.status != http.StatusForbidden {
		t.Fatalf("login without csrf: %d", r.status)
	}
	// A token minted for another pre-session cookie is useless.
	other := h.client()
	tok := csrfFrom(t, h.do(other, "GET", "/login", nil, nil).body)
	r = h.do(c, "POST", "/login", url.Values{"csrf_token": {tok}, "username": {"admin"}, "password": {h.password}}, nil)
	if r.status != http.StatusForbidden {
		t.Fatalf("login with foreign csrf: %d", r.status)
	}
}

func TestLoginGenericError(t *testing.T) {
	h := newHarness(t, nil)
	c := h.client()
	a := h.login(c, "admin", "wrong", nil)
	b := h.login(c, "ghost", "wrong", nil)
	if a.status != http.StatusUnauthorized || b.status != http.StatusUnauthorized {
		t.Fatalf("statuses %d %d", a.status, b.status)
	}
	for _, r := range []resp{a, b} {
		if !strings.Contains(r.body, "Identifiants invalides.") {
			t.Fatal("generic error message missing")
		}
	}
}

func TestForcedPasswordChange(t *testing.T) {
	h := newHarness(t, nil)
	c := h.client()
	r := h.login(c, "admin", h.password, nil)
	if r.status != http.StatusSeeOther || r.header.Get("Location") != "/account/password" {
		t.Fatalf("first login: %d %s", r.status, r.header.Get("Location"))
	}
	for _, p := range []string{"/", "/account", "/nope"} {
		r := h.do(c, "GET", p, nil, nil)
		if r.status != http.StatusSeeOther || r.header.Get("Location") != "/account/password" {
			t.Errorf("%s: %d %s", p, r.status, r.header.Get("Location"))
		}
		r = h.do(c, "GET", p, nil, map[string]string{"HX-Request": "true"})
		if r.status != http.StatusOK || r.header.Get("HX-Redirect") != "/account/password" {
			t.Errorf("htmx %s: %d HX-Redirect=%q", p, r.status, r.header.Get("HX-Redirect"))
		}
	}
	for _, p := range []string{"/healthz", "/readyz", "/static/app.js", "/account/password"} {
		if r := h.do(c, "GET", p, nil, nil); r.status != http.StatusOK {
			t.Errorf("%s must stay reachable: %d", p, r.status)
		}
	}

	page := h.do(c, "GET", "/account/password", nil, nil)
	tok := csrfFrom(t, page.body)

	r = h.do(c, "POST", "/account/password", url.Values{
		"current_password": {h.password}, "new_password": {newPassword}, "confirm_password": {newPassword},
	}, nil)
	if r.status != http.StatusForbidden {
		t.Fatalf("change without csrf: %d", r.status)
	}

	weak := url.Values{"csrf_token": {tok}, "current_password": {h.password},
		"new_password": {"password"}, "confirm_password": {"password"}}
	r = h.do(c, "POST", "/account/password", weak, nil)
	if r.status != http.StatusUnprocessableEntity || !strings.Contains(r.body, "trop courant") {
		t.Fatalf("weak password: %d", r.status)
	}
	r = h.do(c, "POST", "/account/password", weak, map[string]string{"HX-Request": "true"})
	if r.status != http.StatusOK || strings.Contains(r.body, "<html") || !strings.Contains(r.body, `id="password-form"`) {
		t.Fatalf("htmx weak password must return the form partial: %d\n%s", r.status, r.body)
	}

	// CSRF via header, as sent by HTMX hx-headers.
	r = h.do(c, "POST", "/account/password", url.Values{
		"current_password": {h.password}, "new_password": {newPassword}, "confirm_password": {newPassword},
	}, map[string]string{"HX-Request": "true", csrfHeader: tok})
	if r.status != http.StatusOK || r.header.Get("HX-Redirect") != "/" {
		t.Fatalf("change: %d HX-Redirect=%q\n%s", r.status, r.header.Get("HX-Redirect"), r.body)
	}
	if r := h.do(c, "GET", "/", nil, nil); r.status != http.StatusOK || !strings.Contains(r.body, "Tableau de bord") {
		t.Fatalf("dashboard after change: %d", r.status)
	}
}

func TestLogoutAndCSRFOnLogout(t *testing.T) {
	h := newHarness(t, nil)
	c := h.loggedIn()
	if r := h.do(c, "POST", "/logout", url.Values{}, nil); r.status != http.StatusForbidden {
		t.Fatalf("logout without csrf: %d", r.status)
	}
	tok := csrfFrom(t, h.do(c, "GET", "/", nil, nil).body)
	r := h.do(c, "POST", "/logout", url.Values{"csrf_token": {tok}}, nil)
	if r.status != http.StatusSeeOther || r.header.Get("Location") != "/login" {
		t.Fatalf("logout: %d", r.status)
	}
	if r := h.do(c, "GET", "/", nil, nil); r.status != http.StatusSeeOther {
		t.Fatalf("still logged in after logout: %d", r.status)
	}
	if r := h.login(c, "admin", h.password, nil); r.status != http.StatusUnauthorized {
		t.Fatalf("initial password still valid: %d", r.status)
	}
	if r := h.login(c, "admin", newPassword, nil); r.status != http.StatusSeeOther || r.header.Get("Location") != "/" {
		t.Fatalf("login with new password: %d %s", r.status, r.header.Get("Location"))
	}
}

func TestRevokeSession(t *testing.T) {
	h := newHarness(t, nil)
	a := h.loggedIn()
	b := h.client()
	if r := h.login(b, "admin", newPassword, nil); r.status != http.StatusSeeOther {
		t.Fatalf("second login: %d", r.status)
	}
	page := h.do(a, "GET", "/account", nil, nil)
	ids := regexp.MustCompile(`/account/sessions/(\d+)/revoke`).FindAllStringSubmatch(page.body, -1)
	if len(ids) == 0 {
		t.Fatalf("no revocable session listed:\n%s", page.body)
	}
	r := h.do(a, "POST", "/account/sessions/"+ids[0][1]+"/revoke", url.Values{},
		map[string]string{"HX-Request": "true", csrfHeader: csrfFrom(t, page.body)})
	if r.status != http.StatusOK || r.body != "" {
		t.Fatalf("revoke: %d %q", r.status, r.body)
	}
	if r := h.do(b, "GET", "/", nil, nil); r.status != http.StatusSeeOther {
		t.Fatalf("revoked session still works: %d", r.status)
	}
	if r := h.do(a, "GET", "/", nil, nil); r.status != http.StatusOK {
		t.Fatalf("revoking another session logged us out: %d", r.status)
	}
}

func TestLoginRateLimit(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.LoginMaxPerIP = 3 })
	c := h.client()
	for i := range 3 {
		if r := h.login(c, "ghost", "x", nil); r.status != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d", i, r.status)
		}
	}
	r := h.login(c, "admin", h.password, nil)
	if r.status != http.StatusTooManyRequests || r.header.Get("Retry-After") == "" {
		t.Fatalf("expected 429 with Retry-After, got %d", r.status)
	}
	// Untrusted peer: X-Forwarded-For is ignored, still limited.
	r = h.login(c, "admin", h.password, map[string]string{"X-Forwarded-For": "198.51.100.7"})
	if r.status != http.StatusTooManyRequests {
		t.Fatalf("spoofed XFF bypassed the limit: %d", r.status)
	}
}

func TestLoginRateLimitTrustedProxy(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.LoginMaxPerIP = 2
		c.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}
	})
	c := h.client()
	client1 := map[string]string{"X-Forwarded-For": "198.51.100.1"}
	client2 := map[string]string{"X-Forwarded-For": "198.51.100.2"}
	for range 2 {
		h.login(c, "ghost", "x", client1)
	}
	if r := h.login(c, "ghost", "x", client1); r.status != http.StatusTooManyRequests {
		t.Fatalf("client1 should be limited: %d", r.status)
	}
	if r := h.login(c, "ghost", "x", client2); r.status != http.StatusUnauthorized {
		t.Fatalf("client2 must have its own bucket behind a trusted proxy: %d", r.status)
	}
}

func TestRelativeTime(t *testing.T) {
	now := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	cases := map[time.Duration]string{
		10 * time.Second:     "à l'instant",
		5 * time.Minute:      "il y a 5 min",
		2 * time.Hour:        "il y a 2 h",
		24 * time.Hour:       "il y a 1 jour",
		72 * time.Hour:       "il y a 3 jours",
		60 * 24 * time.Hour:  "il y a 2 mois",
		800 * 24 * time.Hour: "il y a 2 ans",
	}
	for d, want := range cases {
		if got := relativeTime(now, now.Add(-d)); got != want {
			t.Errorf("%s: got %q want %q", d, got, want)
		}
	}
}

func TestInsecureCookieWarning(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.CookieSecure = true })
	cases := map[string]struct {
		host, proto string
		want        bool
	}{
		"lan over http":     {"192.168.1.10:8080", "", true},
		"localhost":         {"localhost:8080", "", false},
		"loopback v6":       {"[::1]:8080", "", false},
		"behind https":      {"cleanarr.example.org", "https", false},
		"lan without port":  {"nas.lan", "", true},
		"loopback v4 plain": {"127.0.0.1", "", false},
	}
	for name, c := range cases {
		r := httptest.NewRequest(http.MethodGet, "/login", nil)
		r.Host = c.host
		if c.proto != "" {
			r.Header.Set("X-Forwarded-Proto", c.proto)
		}
		if got := h.srv.insecureCookieSetup(r); got != c.want {
			t.Errorf("%s: got %v", name, got)
		}
	}
	h.srv.Config.CookieSecure = false
	r := httptest.NewRequest(http.MethodGet, "/login", nil)
	r.Host = "192.168.1.10:8080"
	if h.srv.insecureCookieSetup(r) {
		t.Error("COOKIE_SECURE=false: no warning")
	}
	page := h.do(h.client(), "GET", "/login", nil, nil)
	if strings.Contains(page.body, "Connexion impossible en HTTP") {
		t.Error("warning shown with COOKIE_SECURE=false")
	}
}
