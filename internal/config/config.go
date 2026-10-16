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

	"hangarr/internal/secretbox"
)

// History sources (HISTORY_SOURCE). auto resolves to merged when Tautulli
// is configured, plex otherwise.
const (
	HistoryTautulli = "tautulli" // Tautulli only
	HistoryPlex     = "plex"     // Plex history only (no durations)
	HistoryMerged   = "merged"   // Plex history as reference, enriched by Tautulli
	HistoryAuto     = "auto"
	// HistoryJellyfin is not a HISTORY_SOURCE value: the source whenever
	// Jellyfin is the media server (HISTORY_SOURCE is then ignored).
	HistoryJellyfin = "jellyfin"
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
	JellyfinURL    string
	JellyfinAPIKey string

	// Browser-facing URLs for "open in" links (default: the API URLs above;
	// Plex defaults to app.plex.tv).
	SonarrPublicURL   string
	RadarrPublicURL   string
	PlexPublicURL     string
	JellyfinPublicURL string

	// HistorySource is the requested mode: "auto", "merged", "tautulli" or "plex".
	// It is resolved against the effective service configuration (which may
	// come from the UI) by the upstream package.
	HistorySource         string
	TautulliWebhookSecret string

	// MasterKey encrypts the credentials saved from the UI (nil: saving
	// credentials from the UI is refused).
	MasterKey []byte
	// Deprecations are notices about settings from Cleanarr (the former
	// name) still in use; logged at startup.
	Deprecations []string

	SyncTime               string // default daily sync time "HH:MM" (overridable in the UI)
	DryRun                 bool
	ProtectedTags          []string
	ConfirmByTyping        bool
	AbandonDays            int
	PlexRefreshAfterDelete bool

	AdminUsername string
	AdminPassword string // HANGARR_ADMIN_PASSWORD: chosen first-start password instead of a generated one
	CookieSecure  bool
	// HSTSIncludeSubdomains adds includeSubDomains to HSTS (off: HTTPS would
	// be forced on every service under the domain for a year).
	HSTSIncludeSubdomains bool
	SessionIdleTimeout    time.Duration
	SessionMaxAge         time.Duration
	TrustedProxies        []netip.Prefix
	LoginMaxPerIP         int
	// Retention of the security log and of the actions log (0: kept).
	AuthEventsRetentionDays int
	ActionLogRetentionDays  int

	// DefaultLang is the interface language when the user has not chosen one
	// ("en" or "fr").
	DefaultLang string

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

// password reads KEY or KEY_FILE like secret, without trimming spaces: only
// the line break that ends the file is dropped.
func (l *loader) password(key string) string {
	v, hasValue := l.lookup(key)
	path, hasFile := l.lookup(key + "_FILE")
	path = strings.TrimSpace(path)
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
		return strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
	}
	return v
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

// Load parses the configuration. Format errors are reported together. The
// upstream services are optional here: they may be configured from the UI,
// and CLI commands (reset-password, healthcheck) do not need them.
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
		JellyfinURL:    l.baseURL("JELLYFIN_URL"),
		JellyfinAPIKey: l.secret("JELLYFIN_API_KEY"),

		SonarrPublicURL:   l.baseURL("SONARR_PUBLIC_URL"),
		RadarrPublicURL:   l.baseURL("RADARR_PUBLIC_URL"),
		PlexPublicURL:     l.baseURL("PLEX_PUBLIC_URL"),
		JellyfinPublicURL: l.baseURL("JELLYFIN_PUBLIC_URL"),

		TautulliWebhookSecret: l.secret("TAUTULLI_WEBHOOK_SECRET"),
		HistorySource:         strings.ToLower(l.str("HISTORY_SOURCE", HistoryAuto)),

		SyncTime:               l.str("SYNC_TIME", "04:00"),
		DryRun:                 l.boolean("DRY_RUN", true),
		ProtectedTags:          splitList(l.str("PROTECTED_TAGS", "keep")),
		ConfirmByTyping:        l.boolean("CONFIRM_BY_TYPING", true),
		AbandonDays:            l.integer("ABANDON_DAYS", 90, 1),
		PlexRefreshAfterDelete: l.boolean("PLEX_REFRESH_AFTER_DELETE", true),

		AdminUsername:         l.str("ADMIN_USERNAME", "admin"),
		CookieSecure:          l.boolean("COOKIE_SECURE", true),
		HSTSIncludeSubdomains: l.boolean("HSTS_INCLUDE_SUBDOMAINS", false),
		SessionIdleTimeout:    l.duration("SESSION_IDLE_TIMEOUT", 12*time.Hour, time.Minute),
		SessionMaxAge:         l.duration("SESSION_MAX_AGE", 168*time.Hour, time.Minute),
		LoginMaxPerIP:         l.integer("LOGIN_MAX_PER_IP", 10, 1),

		AuthEventsRetentionDays: l.integer("AUTH_EVENTS_RETENTION_DAYS", 365, 0),
		ActionLogRetentionDays:  l.integer("ACTION_LOG_RETENTION_DAYS", 0, 0),

		DefaultLang: strings.ToLower(l.str("DEFAULT_LANG", "en")),

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
	// Not trimmed: spaces may be part of the password. The policy is checked
	// when the account is created (auth.Bootstrap).
	c.AdminPassword = l.password("HANGARR_ADMIN_PASSWORD")
	if c.SessionMaxAge < c.SessionIdleTimeout {
		l.fail("SESSION_MAX_AGE (%s) must be >= SESSION_IDLE_TIMEOUT (%s)", c.SessionMaxAge, c.SessionIdleTimeout)
	}
	if c.TautulliWebhookSecret != "" && len(c.TautulliWebhookSecret) < 32 {
		l.fail("TAUTULLI_WEBHOOK_SECRET must be at least 32 characters")
	}
	if (c.PlexURL != "" || c.PlexToken != "") && (c.JellyfinURL != "" || c.JellyfinAPIKey != "") {
		l.fail("PLEX_* and JELLYFIN_* are both set: Hangarr uses a single media server, set only one of them")
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

	if c.DefaultLang != "en" && c.DefaultLang != "fr" {
		l.fail("DEFAULT_LANG: expected en or fr, got %q", c.DefaultLang)
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
	case HistoryAuto, HistoryTautulli, HistoryPlex, HistoryMerged:
	default:
		l.fail("HISTORY_SOURCE: expected auto, merged, tautulli or plex, got %q", c.HistorySource)
	}

	// CLEANARR_MASTER_KEY(_FILE): the name before the rename to Hangarr,
	// still read when the new one is not set.
	name, mk := "HANGARR_MASTER_KEY", l.secret("HANGARR_MASTER_KEY")
	if legacy := l.secret("CLEANARR_MASTER_KEY"); legacy != "" {
		if mk == "" {
			name, mk = "CLEANARR_MASTER_KEY", legacy
			c.Deprecations = append(c.Deprecations, "CLEANARR_MASTER_KEY(_FILE) is deprecated: rename it HANGARR_MASTER_KEY(_FILE)")
		} else {
			c.Deprecations = append(c.Deprecations, "CLEANARR_MASTER_KEY(_FILE) is ignored: HANGARR_MASTER_KEY(_FILE) is set")
		}
	}
	if mk != "" {
		key, err := secretbox.ParseKey(mk)
		if err != nil {
			l.fail("%s: %w", name, err)
		}
		c.MasterKey = key
	}

	if len(l.errs) > 0 {
		return nil, errors.Join(l.errs...)
	}
	return c, nil
}
