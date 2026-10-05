// Command arrstats serves the web UI and offers maintenance subcommands.
package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata" // distroless has no zoneinfo; TZ must work anyway

	"arrstats/internal/actions"
	"arrstats/internal/auth"
	"arrstats/internal/clients/plex"
	"arrstats/internal/clients/radarr"
	"arrstats/internal/clients/sonarr"
	"arrstats/internal/clients/tautulli"
	"arrstats/internal/config"
	"arrstats/internal/images"
	"arrstats/internal/scoring"
	"arrstats/internal/store"
	"arrstats/internal/syncer"
	"arrstats/internal/web"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `Usage: arrstats <command>

Commands:
  serve                                  start the web server (default)
  sync --once                            run one synchronization and print a summary
  admin reset-password [--username NAME] print a new temporary password for an account
  healthcheck                            exit 0 if the local server answers /healthz
  version                                print the version
`

func main() {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve()
	case "sync":
		err = syncOnce(args)
	case "admin":
		err = admin(args)
	case "healthcheck":
		err = healthcheck()
	case "version", "--version", "-v":
		fmt.Println(version)
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func loadConfig() (*config.Config, error) {
	cfg, err := config.Load(os.LookupEnv, os.ReadFile)
	if err != nil {
		return nil, fmt.Errorf("invalid configuration:\n%w", err)
	}
	time.Local = cfg.Location
	return cfg, nil
}

func newLogger(cfg *config.Config, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	if cfg.LogFormat == "text" {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}

func serve() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if err := cfg.RequireUpstreams(); err != nil {
		return fmt.Errorf("invalid configuration:\n%w", err)
	}
	log := newLogger(cfg, os.Stdout)
	slog.SetDefault(log)
	log.Info("starting arrstats", "version", version, "dry_run", cfg.DryRun, "history_source", cfg.HistorySource)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	st, err := store.Open(ctx, cfg.DataDir)
	if err != nil {
		return err
	}
	defer func() {
		if err := st.Close(); err != nil {
			log.Error("closing database", "err", err)
		}
		log.Info("database closed")
	}()

	hasher, err := auth.NewHasher(auth.DefaultHashParams)
	if err != nil {
		return err
	}
	svc := &auth.Service{Store: st, Hasher: hasher, Log: log,
		IdleTimeout: cfg.SessionIdleTimeout, MaxAge: cfg.SessionMaxAge}
	// The banner goes straight to stdout, in plain text even in JSON mode,
	// so that it stays readable in `docker logs` / `kubectl logs`.
	if _, err := svc.Bootstrap(ctx, cfg.AdminUsername, os.Stdout); err != nil {
		return fmt.Errorf("admin bootstrap: %w", err)
	}

	key, err := st.Secret(ctx, "presession_csrf_key", func() ([]byte, error) {
		b := make([]byte, 32)
		_, err := rand.Read(b)
		return b, err
	})
	if err != nil {
		return err
	}
	limiter := auth.NewRateLimiter(cfg.LoginMaxPerIP, auth.LoginWindow, nil)

	sy := newSyncer(cfg, st, log)
	syncTime, err := st.Setting(ctx, store.SettingSyncTime, cfg.SyncTime)
	if err != nil {
		return err
	}
	at, err := syncer.ParseClockTime(syncTime)
	if err != nil {
		log.Warn("invalid stored sync time, using SYNC_TIME", "value", syncTime)
		at, _ = syncer.ParseClockTime(cfg.SyncTime)
	}
	sy.SetDailyTime(at)
	weights := scoring.DefaultWeights
	loadFloat := func(key string, dst *float64) {
		if v, err := st.Setting(ctx, key, ""); err == nil && v != "" {
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				*dst = f
			}
		}
	}
	loadFloat(store.SettingScoreNeverWatched, &weights.NeverWatched)
	loadFloat(store.SettingScoreContinuing, &weights.ActiveContinuing)
	window := float64(weights.ActiveWindowDays)
	loadFloat(store.SettingScoreActiveWindow, &window)
	weights.ActiveWindowDays = int(window)
	sy.SetWeights(weights)
	lookup := func(ctx context.Context, kind string, arrID int64, variant string) (images.Info, bool, error) {
		title, remote, ok, err := st.ImageInfo(ctx, kind, arrID, variant)
		return images.Info{Title: title, RemoteURL: remote}, ok, err
	}
	proxy := images.New(filepath.Join(cfg.DataDir, "cache", "img"), sy.Sonarr.HTTP, sy.Radarr.HTTP, lookup, log)

	deleter := &actions.Service{Store: st, Sonarr: sy.Sonarr, Radarr: sy.Radarr, Plex: sy.Plex, Refresher: sy,
		DryRun: cfg.DryRun, ProtectedTags: cfg.ProtectedTags, ConfirmByTyping: cfg.ConfirmByTyping,
		PlexRefresh: cfg.PlexRefreshAfterDelete, Log: log}

	srv, err := web.New(web.Deps{Config: cfg, Auth: svc, Store: st, Log: log, Version: version,
		PreSessionKey: key, LoginLimiter: limiter, Images: proxy, Sync: sy,
		Actions: deleter, Sonarr: sy.Sonarr, Radarr: sy.Radarr})
	if err != nil {
		return err
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		janitor(ctx, log, svc, limiter)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		sy.Daily(ctx)
	}()

	err = srv.Run(ctx, cfg.ListenAddr)
	stop()
	wg.Wait()
	if err != nil {
		return err
	}
	log.Info("shutdown complete")
	return nil
}

// janitor purges expired sessions and rate-limit entries until ctx ends.
func janitor(ctx context.Context, log *slog.Logger, svc *auth.Service, limiter *auth.RateLimiter) {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		if n, err := svc.PurgeExpired(ctx); err != nil && ctx.Err() == nil {
			log.Error("session purge failed", "err", err)
		} else if n > 0 {
			log.Info("expired sessions purged", "count", n)
		}
		limiter.Sweep()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func admin(args []string) error {
	if len(args) == 0 || args[0] != "reset-password" {
		return errors.New("usage: arrstats admin reset-password [--username NAME]")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("reset-password", flag.ContinueOnError)
	username := fs.String("username", cfg.AdminUsername, "account to reset")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	// Logs go to stderr: only the command's own stdout shows the password.
	log := newLogger(cfg, os.Stderr)
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.DataDir)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	hasher, err := auth.NewHasher(auth.DefaultHashParams)
	if err != nil {
		return err
	}
	svc := &auth.Service{Store: st, Hasher: hasher, Log: log,
		IdleTimeout: cfg.SessionIdleTimeout, MaxAge: cfg.SessionMaxAge}
	return svc.ResetPassword(ctx, *username, os.Stdout)
}

// healthcheck is the Docker HEALTHCHECK: distroless has no shell nor curl.
// It only reads LISTEN_ADDR so it works even if other settings are invalid.
func healthcheck() error {
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("LISTEN_ADDR: %w", err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// The target is always this container (loopback or LISTEN_ADDR host).
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(host, port)+"/healthz", nil) //nolint:gosec // G704: local health probe
	if err != nil {
		return err
	}
	res, err := http.DefaultClient.Do(req) //nolint:gosec // G704: local health probe
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz returned %d", res.StatusCode)
	}
	return nil
}

// newSyncer wires the upstream clients from the configuration.
func newSyncer(cfg *config.Config, st *store.Store, log *slog.Logger) *syncer.Syncer {
	s := &syncer.Syncer{
		Store:       st,
		Sonarr:      sonarr.New(cfg.SonarrURL, cfg.SonarrAPIKey),
		Radarr:      radarr.New(cfg.RadarrURL, cfg.RadarrAPIKey),
		Plex:        plex.New(cfg.PlexURL, cfg.PlexToken),
		Log:         log,
		AbandonDays: cfg.AbandonDays,
		Weights:     scoring.DefaultWeights,
		Location:    cfg.Location,
	}
	if cfg.HistorySource == config.HistoryTautulli {
		s.Tautulli = tautulli.New(cfg.TautulliURL, cfg.TautulliAPIKey)
	}
	return s
}

// syncOnce runs a single synchronization (useful in dev) and prints a summary.
func syncOnce(args []string) error {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	once := fs.Bool("once", false, "run one synchronization and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*once {
		return errors.New("usage: arrstats sync --once")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if err := cfg.RequireUpstreams(); err != nil {
		return fmt.Errorf("invalid configuration:\n%w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	st, err := store.Open(ctx, cfg.DataDir)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	res, err := newSyncer(cfg, st, newLogger(cfg, os.Stderr)).Run(ctx)
	if err != nil {
		return err
	}
	printSummary(os.Stdout, res)
	if res.Failed() {
		return errors.New("some sources failed")
	}
	return nil
}

func printSummary(w io.Writer, res *syncer.Result) {
	fmt.Fprintf(w, "Synchronisation terminée en %s\n\n", res.Duration.Round(time.Millisecond))
	for _, s := range res.Sources {
		status := "ok"
		if s.Err != nil {
			status = "ERREUR : " + s.Err.Error()
		}
		fmt.Fprintf(w, "  %-13s %6d éléments  %8s  %s\n", s.Source, s.Items, s.Duration.Round(time.Millisecond), status)
	}
	fmt.Fprintf(w, "\nRapprochement : %d rapprochés, %d seulement dans Sonarr/Radarr, %d seulement dans Plex\n",
		res.Matches["matched"], res.Matches["arr_only"], res.Matches["plex_only"])
	fmt.Fprintf(w, "Historique    : %d sessions rattachées, %d orphelines (médias disparus)\n", res.Linked, res.Orphans)
}
