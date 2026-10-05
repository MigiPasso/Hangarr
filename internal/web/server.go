// Package web contains the HTTP server: routing, middleware, handlers and
// the embedded templates and static assets.
package web

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"cleanarr/internal/actions"
	"cleanarr/internal/auth"
	"cleanarr/internal/config"
	"cleanarr/internal/images"
	"cleanarr/internal/store"
	"cleanarr/internal/upstream"
)

// ShutdownTimeout bounds how long in-flight requests may run after SIGTERM.
const ShutdownTimeout = 20 * time.Second

// maxBodyBytes limits request bodies; forms in this app are tiny.
const maxBodyBytes = 1 << 20

// Deps are the server's collaborators.
type Deps struct {
	Config        *config.Config
	Auth          *auth.Service
	Store         *store.Store
	Log           *slog.Logger
	Version       string
	PreSessionKey []byte
	LoginLimiter  *auth.RateLimiter
	Images        *images.Proxy      // nil: images answer 404
	Sync          SyncRunner         // nil: no sync button
	PageSize      int                // library batch size, DefaultPageSize when 0
	Actions       *actions.Service   // nil: deletion unavailable
	Upstreams     *upstream.Registry // current upstream clients (never nil)
	Services      *upstream.Manager  // nil: Configuration page read-only
}

// Server holds handlers state.
type Server struct {
	Deps
	ips auth.IPResolver
	// webhookLimiter rate-limits POST /webhooks/tautulli per client IP.
	webhookLimiter *auth.RateLimiter
	views          *views
	draining       atomic.Bool
	baseCtx        atomic.Value // context.Context of Run, for background work
	disks          diskCache
}

// New builds the server and parses templates.
func New(d Deps) (*Server, error) {
	v, err := newViews(d.Config.Location, d.Config.ProtectedTags, d.Config.AbandonDays)
	if err != nil {
		return nil, err
	}
	if d.Upstreams == nil {
		d.Upstreams = upstream.NewRegistry(nil)
	}
	return &Server{Deps: d, ips: auth.IPResolver{Trusted: d.Config.TrustedProxies}, views: v,
		webhookLimiter: auth.NewRateLimiter(webhookPerMinute, time.Minute, nil)}, nil
}

// baseContext outlives requests: background work started by a request uses it.
func (s *Server) baseContext() context.Context {
	if ctx, ok := s.baseCtx.Load().(context.Context); ok {
		return ctx
	}
	return context.Background()
}

// SetDraining makes /readyz fail so no new traffic is routed during shutdown.
func (s *Server) SetDraining() { s.draining.Store(true) }

// Handler returns the root handler with global middleware.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.Handle("GET /static/", s.static())

	mux.HandleFunc("POST /webhooks/tautulli", s.tautulliWebhook)
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.loginSubmit)
	mux.Handle("POST /logout", s.authed(s.logout, allowMustChange))
	mux.Handle("GET /account/password", s.authed(s.passwordPage, allowMustChange))
	mux.Handle("POST /account/password", s.authed(s.passwordSubmit, allowMustChange))

	mux.Handle("GET /account", s.authed(s.accountPage, 0))
	mux.Handle("GET /series", s.authed(s.library("series", "/series", "Séries"), 0))
	mux.Handle("GET /movies", s.authed(s.library("movie", "/movies", "Films"), 0))
	mux.Handle("GET /img/{kind}/{id}/{variant}", s.authed(s.image, 0))
	mux.Handle("POST /sync", s.authed(s.syncStart, 0))
	for _, k := range []struct{ kind, path string }{{"series", "/series"}, {"movie", "/movies"}} {
		mux.Handle("GET "+k.path+"/{id}", s.authed(s.detail(k.kind), 0))
		mux.Handle("GET "+k.path+"/{id}/delete", s.authed(s.deletePage(k.kind), 0))
		mux.Handle("POST "+k.path+"/{id}/delete", s.authed(s.deleteSubmit(k.kind), 0))
		mux.Handle("POST "+k.path+"/{id}/protect", s.authed(s.protectSubmit(k.kind), 0))
	}
	mux.Handle("GET /journal", s.authed(s.journal, 0))
	mux.Handle("GET /stats", s.authed(s.stats, 0))
	mux.Handle("GET /stats/users/{id}", s.authed(s.userStats, 0))
	mux.Handle("GET /cleanup", s.authed(s.cleanup, 0))
	mux.Handle("POST /settings/cleanup", s.authed(s.cleanupSubmit, 0))
	mux.Handle("GET /settings", s.authed(s.settingsPage, 0))
	mux.Handle("POST /settings", s.authed(s.settingsSubmit, 0))
	mux.Handle("GET /sync/status", s.authed(s.syncStatusPartial, 0))
	mux.Handle("GET /health", s.authed(s.health, 0))
	mux.Handle("GET /health/services", s.authed(s.healthServices, 0))
	mux.Handle("GET /config", s.authed(s.configPage, 0))
	mux.Handle("POST /config/{service}", s.authed(s.configSubmit, 0))
	mux.Handle("POST /config/{service}/test", s.authed(s.configTest, 0))
	mux.Handle("POST /config/tautulli/webhook-secret", s.authed(s.webhookSecretSubmit, 0))
	mux.Handle("POST /account/sessions/{id}/revoke", s.authed(s.revokeSession, 0))
	mux.Handle("GET /{$}", s.authed(s.home, 0))
	// Everything else requires a session too, so unknown paths do not leak
	// anything to anonymous users.
	mux.Handle("/", s.authed(s.notFound, 0))

	return s.recoverer(s.logRequests(s.securityHeaders(limitBody(mux))))
}

// Run serves on addr until ctx is canceled, then shuts down gracefully:
// readiness fails, the listener closes and in-flight requests get
// ShutdownTimeout to finish.
func (s *Server) Run(ctx context.Context, addr string) error {
	s.baseCtx.Store(ctx)
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(s.Log.Handler(), slog.LevelWarn),
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	s.Log.Info("http server listening", "addr", ln.Addr().String())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	s.Log.Info("shutdown requested, draining connections", "timeout", ShutdownTimeout.String())
	s.SetDraining()
	shCtx, cancel := context.WithTimeout(context.Background(), ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shCtx); err != nil {
		return err
	}
	if err := <-errCh; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
