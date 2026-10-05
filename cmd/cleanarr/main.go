// Command cleanarr serves the web UI and offers maintenance subcommands.
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
	"sync"
	"syscall"
	"time"
	_ "time/tzdata" // distroless has no zoneinfo; TZ must work anyway

	"cleanarr/internal/actions"
	"cleanarr/internal/auth"
	"cleanarr/internal/config"
	"cleanarr/internal/images"
	"cleanarr/internal/secretbox"
	"cleanarr/internal/store"
	"cleanarr/internal/syncer"
	"cleanarr/internal/upstream"
	"cleanarr/internal/web"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `Usage: cleanarr <command>

Commands:
  serve                                  start the web server (default)
  sync --once                            run one synchronization and print a summary
  admin reset-password [--username NAME] print a new temporary password for an account
  healthcheck                            exit 0 if the local server answers /healthz
  master-key                             print a new random master key (CLEANARR_MASTER_KEY)
  backup [PATH]                          consistent copy of the database, safe while serving
                                         (default: DATA_DIR/backups/cleanarr-YYYYMMDD-HHMMSS.db)
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
	case "master-key":
		err = masterKey()
	case "backup":
		err = backup(args)
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
	log := newLogger(cfg, os.Stdout)
	slog.SetDefault(log)
	log.Info("starting cleanarr", "version", version, "dry_run", cfg.DryRun, "master_key", cfg.MasterKey != nil)

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

	services, err := newServices(ctx, cfg, st, log)
	if err != nil {
		return err
	}
	if up := services.Registry.Get(); !up.Ready() {
		log.Warn("services not fully configured: synchronization disabled until configured in the UI",
			"missing", up.Missing)
	}
	sy := newSyncer(cfg, st, services.Registry, log)
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
	lookup := func(ctx context.Context, kind string, arrID int64, variant string) (images.Info, bool, error) {
		title, remote, ok, err := st.ImageInfo(ctx, kind, arrID, variant)
		return images.Info{Title: title, RemoteURL: remote}, ok, err
	}
	proxy := images.New(filepath.Join(cfg.DataDir, "cache", "img"), services.Registry.ArrHTTP, lookup, log)

	deleter := &actions.Service{Store: st, Upstreams: services.Registry, Refresher: sy,
		DryRun: cfg.DryRun, ProtectedTags: cfg.ProtectedTags, ConfirmByTyping: cfg.ConfirmByTyping,
		PlexRefresh: cfg.PlexRefreshAfterDelete, Log: log}

	srv, err := web.New(web.Deps{Config: cfg, Auth: svc, Store: st, Log: log, Version: version,
		PreSessionKey: key, LoginLimiter: limiter, Images: proxy, Sync: sy,
		Actions: deleter, Upstreams: services.Registry, Services: services})
	if err != nil {
		return err
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		janitor(ctx, log, svc, func() { limiter.Sweep(); srv.SweepLimiters() })
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
func janitor(ctx context.Context, log *slog.Logger, svc *auth.Service, sweep func()) {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		if n, err := svc.PurgeExpired(ctx); err != nil && ctx.Err() == nil {
			log.Error("session purge failed", "err", err)
		} else if n > 0 {
			log.Info("expired sessions purged", "count", n)
		}
		sweep()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func admin(args []string) error {
	if len(args) == 0 || args[0] != "reset-password" {
		return errors.New("usage: cleanarr admin reset-password [--username NAME]")
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

// newServices loads the upstream configuration (environment, then values
// saved from the UI) and builds the clients.
func newServices(ctx context.Context, cfg *config.Config, st *store.Store, log *slog.Logger) (*upstream.Manager, error) {
	m := &upstream.Manager{Store: st, Env: upstream.EnvValues(cfg), HistoryMode: cfg.HistorySource,
		Registry: upstream.NewRegistry(nil), Log: log}
	if err := m.SetupKey(ctx, cfg.MasterKey, filepath.Join(cfg.DataDir, upstream.KeyFileName)); err != nil {
		return nil, err
	}
	if _, err := m.Reload(ctx); err != nil {
		return nil, fmt.Errorf("loading service configuration: %w", err)
	}
	return m, nil
}

// newSyncer builds the syncer on the upstream registry.
func newSyncer(cfg *config.Config, st *store.Store, reg *upstream.Registry, log *slog.Logger) *syncer.Syncer {
	return &syncer.Syncer{
		Store:       st,
		Upstreams:   reg,
		Log:         log,
		AbandonDays: cfg.AbandonDays,
		Location:    cfg.Location,
	}
}

// backup writes a consistent copy of the database (VACUUM INTO).
func backup(args []string) error {
	if len(args) > 1 {
		return errors.New("usage: cleanarr backup [PATH]")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	path := filepath.Join(cfg.DataDir, "backups", "cleanarr-"+time.Now().Format("20060102-150405")+".db")
	if len(args) == 1 {
		path = args[0]
	}
	// The path is given by the operator on the command line.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil { //nolint:gosec // G703: operator-provided path
		return err
	}
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.DataDir)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	if err := st.BackupTo(ctx, path); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	fmt.Println(path)
	if cfg.MasterKey == nil {
		fmt.Fprintf(os.Stderr, "note: the saved credentials are encrypted with %s; keep it with the backup.\n",
			filepath.Join(cfg.DataDir, upstream.KeyFileName))
	} else {
		fmt.Fprintln(os.Stderr, "note: the saved credentials are encrypted with CLEANARR_MASTER_KEY; keep the key to restore them.")
	}
	return nil
}

// masterKey prints a fresh master key. It is never written to disk: store it
// in a Docker or Kubernetes secret.
func masterKey() error {
	k, err := secretbox.NewKey()
	if err != nil {
		return err
	}
	fmt.Println(k)
	return nil
}

// syncOnce runs a single synchronization (useful in dev) and prints a summary.
func syncOnce(args []string) error {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	once := fs.Bool("once", false, "run one synchronization and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*once {
		return errors.New("usage: cleanarr sync --once")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	st, err := store.Open(ctx, cfg.DataDir)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	log := newLogger(cfg, os.Stderr)
	services, err := newServices(ctx, cfg, st, log)
	if err != nil {
		return err
	}
	res, err := newSyncer(cfg, st, services.Registry, log).Run(ctx)
	if errors.Is(err, syncer.ErrNotConfigured) {
		return fmt.Errorf("services not configured: %v", services.Registry.Get().Missing)
	}
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
	fmt.Fprintf(w, "Sync finished in %s\n\n", res.Duration.Round(time.Millisecond))
	for _, s := range res.Sources {
		status := "ok"
		if s.Err != nil {
			status = "ERROR: " + s.Err.Error()
		}
		fmt.Fprintf(w, "  %-13s %6d items  %8s  %s\n", s.Source, s.Items, s.Duration.Round(time.Millisecond), status)
	}
	fmt.Fprintf(w, "\nMatching: %d matched, %d only in Sonarr/Radarr, %d only in Plex\n",
		res.Matches["matched"], res.Matches["arr_only"], res.Matches["plex_only"])
	fmt.Fprintf(w, "History : %d sessions linked, %d orphans (deleted media)\n", res.Linked, res.Orphans)
}
