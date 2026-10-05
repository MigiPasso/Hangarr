// Package config loads and validates the application configuration from
// environment variables. Secrets also accept a *_FILE variant pointing to a
// file (Kubernetes Secret mounted as a volume).
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"cleanarr/internal/secretbox"
)

// History sources.
const (
	HistoryTautulli = "tautulli"
	HistoryPlex     = "plex"
	HistoryAuto     = "auto"
)

// Config is the fully parsed application configuration.
type Config struct {
	ListenAddr string
	DataDir    string

	SonarrURL      string
	SonarrAPIKey   string
	RadarrURL      string
	RadarrAPIKey   string
	PlexURL        string
	PlexToken      string
	TautulliURL    string
	TautulliAPIKey string

	// Browser-facing URLs for "open in" links (default: the API URLs above;
	// Plex defaults to app.plex.tv).
	SonarrPublicURL string
	RadarrPublicURL string
	PlexPublicURL   string

	// HistorySource is the requested mode: "auto", "tautulli" or "plex".
	// It is resolved against the effective service configuration (which may
	// come from the UI) by the upstream package.
	HistorySource         string
	TautulliWebhookSecret string

	// MasterKey encrypts the credentials saved from the UI (nil: saving
	// credentials from the UI is refused).
	MasterKey []byte

	SyncTime               string // default daily sync time "HH:MM" (overridable in the UI)
	DryRun                 bool
	ProtectedTags          []string
	ConfirmByTyping        bool
	AbandonDays            int
	PlexRefreshAfterDelete bool

	AdminUsername      string
	CookieSecure       bool
	SessionIdleTimeout time.Duration
	SessionMaxAge      time.Duration
	TrustedProxies     []netip.Prefix
	LoginMaxPerIP      int

	LogFormat string
	LogLevel  slog.Level
	Location  *time.Location
}

// LookupFunc matches os.LookupEnv.
type LookupFunc func(key string) (string, bool)

// ReadFileFunc matches os.ReadFile.
type ReadFileFunc func(name string) ([]byte, error)

type loader struct {
	lookup   LookupFunc
	readFile ReadFileFunc
	errs     []error
}

func (l *loader) fail(format string, args ...any) {
	l.errs = append(l.errs, fmt.Errorf(format, args...))
}

func (l *loader) str(key, def string) string {
	if v, ok := l.lookup(key); ok {
		return strings.TrimSpace(v)
	}
	return def
}

// secret reads KEY or KEY_FILE; defining both is an error.
func (l *loader) secret(key string) string {
	v, hasValue := l.lookup(key)
	path, hasFile := l.lookup(key + "_FILE")
	v, path = strings.TrimSpace(v), strings.TrimSpace(path)
	hasValue = hasValue && v != ""
	hasFile = hasFile && path != ""
	switch {
	case hasValue && hasFile:
		l.fail("%s and %s_FILE are both set; use only one", key, key)
		return ""
	case hasFile:
		b, err := l.readFile(path)
		if err != nil {
			l.fail("%s_FILE: %w", key, err)
			return ""
		}
		return strings.TrimSpace(string(b))
	default:
		return v
	}
}

func (l *loader) boolean(key string, def bool) bool {
	v := l.str(key, "")
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		l.fail("%s: invalid boolean %q", key, v)
		return def
	}
	return b
}

func (l *loader) integer(key string, def, minimum int) int {
	v := l.str(key, "")
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < minimum {
		l.fail("%s: expected an integer >= %d, got %q", key, minimum, v)
		return def
	}
	return n
}

func (l *loader) duration(key string, def, minimum time.Duration) time.Duration {
	v := l.str(key, "")
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < minimum {
		l.fail("%s: expected a duration >= %s, got %q", key, minimum, v)
		return def
	}
	return d
}

func (l *loader) baseURL(key string) string {
	v := strings.TrimRight(l.str(key, ""), "/")
	if v == "" {
		return ""
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		l.fail("%s: expected an http(s) URL, got %q", key, v)
		return ""
	}
	return v
}

func splitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Load parses the configuration. Format errors are reported together; the
// presence of upstream services is checked separately by RequireUpstreams so
// that CLI commands (reset-password, healthcheck) do not need them.
func Load(lookup LookupFunc, readFile ReadFileFunc) (*Config, error) {
	l := &loader{lookup: lookup, readFile: readFile}
	c := &Config{
		ListenAddr: l.str("LISTEN_ADDR", ":8080"),
		DataDir:    l.str("DATA_DIR", "/data"),

		SonarrURL:      l.baseURL("SONARR_URL"),
		SonarrAPIKey:   l.secret("SONARR_API_KEY"),
		RadarrURL:      l.baseURL("RADARR_URL"),
		RadarrAPIKey:   l.secret("RADARR_API_KEY"),
		PlexURL:        l.baseURL("PLEX_URL"),
		PlexToken:      l.secret("PLEX_TOKEN"),
		TautulliURL:    l.baseURL("TAUTULLI_URL"),
		TautulliAPIKey: l.secret("TAUTULLI_API_KEY"),

		SonarrPublicURL: l.baseURL("SONARR_PUBLIC_URL"),
		RadarrPublicURL: l.baseURL("RADARR_PUBLIC_URL"),
		PlexPublicURL:   l.baseURL("PLEX_PUBLIC_URL"),

		TautulliWebhookSecret: l.secret("TAUTULLI_WEBHOOK_SECRET"),
		HistorySource:         strings.ToLower(l.str("HISTORY_SOURCE", HistoryAuto)),

		SyncTime:               l.str("SYNC_TIME", "04:00"),
		DryRun:                 l.boolean("DRY_RUN", true),
		ProtectedTags:          splitList(l.str("PROTECTED_TAGS", "keep")),
		ConfirmByTyping:        l.boolean("CONFIRM_BY_TYPING", true),
		AbandonDays:            l.integer("ABANDON_DAYS", 90, 1),
		PlexRefreshAfterDelete: l.boolean("PLEX_REFRESH_AFTER_DELETE", true),

		AdminUsername:      l.str("ADMIN_USERNAME", "admin"),
		CookieSecure:       l.boolean("COOKIE_SECURE", true),
		SessionIdleTimeout: l.duration("SESSION_IDLE_TIMEOUT", 12*time.Hour, time.Minute),
		SessionMaxAge:      l.duration("SESSION_MAX_AGE", 168*time.Hour, time.Minute),
		LoginMaxPerIP:      l.integer("LOGIN_MAX_PER_IP", 10, 1),

		LogFormat: strings.ToLower(l.str("LOG_FORMAT", "json")),
	}

	if _, err := time.Parse("15:04", c.SyncTime); err != nil || len(c.SyncTime) != 5 {
		l.fail("SYNC_TIME: expected HH:MM, got %q", c.SyncTime)
	}
	if c.DataDir == "" {
		l.fail("DATA_DIR must not be empty")
	}
	if c.AdminUsername == "" || len(c.AdminUsername) > 64 {
		l.fail("ADMIN_USERNAME must be 1 to 64 characters")
	}
	if c.SessionMaxAge < c.SessionIdleTimeout {
		l.fail("SESSION_MAX_AGE (%s) must be >= SESSION_IDLE_TIMEOUT (%s)", c.SessionMaxAge, c.SessionIdleTimeout)
	}
	if c.TautulliWebhookSecret != "" && len(c.TautulliWebhookSecret) < 32 {
		l.fail("TAUTULLI_WEBHOOK_SECRET must be at least 32 characters")
	}

	for _, p := range splitList(l.str("TRUSTED_PROXIES", "")) {
		prefix, err := netip.ParsePrefix(p)
		if err != nil {
			addr, aerr := netip.ParseAddr(p)
			if aerr != nil {
				l.fail("TRUSTED_PROXIES: invalid CIDR or address %q", p)
				continue
			}
			prefix = netip.PrefixFrom(addr.Unmap(), addr.Unmap().BitLen())
		}
		c.TrustedProxies = append(c.TrustedProxies, prefix.Masked())
	}

	switch c.LogFormat {
	case "json", "text":
	default:
		l.fail("LOG_FORMAT: expected json or text, got %q", c.LogFormat)
	}
	if err := c.LogLevel.UnmarshalText([]byte(l.str("LOG_LEVEL", "info"))); err != nil {
		l.fail("LOG_LEVEL: %w", err)
	}

	tz := l.str("TZ", "Europe/Paris")
	loc, err := time.LoadLocation(tz)
	if err != nil {
		l.fail("TZ: unknown time zone %q", tz)
		loc = time.UTC
	}
	c.Location = loc

	switch c.HistorySource {
	case HistoryAuto, HistoryTautulli, HistoryPlex:
	default:
		l.fail("HISTORY_SOURCE: expected tautulli, plex or auto, got %q", c.HistorySource)
	}

	if mk := l.secret("CLEANARR_MASTER_KEY"); mk != "" {
		key, err := secretbox.ParseKey(mk)
		if err != nil {
			l.fail("CLEANARR_MASTER_KEY: %w", err)
		}
		c.MasterKey = key
	}

	if len(l.errs) > 0 {
		return nil, errors.Join(l.errs...)
	}
	return c, nil
}

// RequireUpstreams checks that the mandatory upstream services are defined
// in the environment (capture-fixtures, which never reads the database).
func (c *Config) RequireUpstreams() error {
	var errs []error
	req := func(key, v string) {
		if v == "" {
			errs = append(errs, fmt.Errorf("%s is required", key))
		}
	}
	req("SONARR_URL", c.SonarrURL)
	req("SONARR_API_KEY", c.SonarrAPIKey)
	req("RADARR_URL", c.RadarrURL)
	req("RADARR_API_KEY", c.RadarrAPIKey)
	req("PLEX_URL", c.PlexURL)
	req("PLEX_TOKEN", c.PlexToken)
	return errors.Join(errs...)
}
