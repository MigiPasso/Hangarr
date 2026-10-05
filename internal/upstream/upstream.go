// Package upstream holds the effective configuration of the upstream services
// (Sonarr, Radarr, Plex, Tautulli) and the clients built from it.
//
// A value comes from the environment when the variable is set (it then wins
// and is locked in the UI), otherwise from the database (Configuration page;
// credentials encrypted with the master key). Clients are rebuilt and swapped
// atomically when the configuration changes: callers take a snapshot with
// Registry.Get at the start of each operation.
package upstream

import (
	"errors"
	"strings"
	"sync/atomic"

	"cleanarr/internal/clients/httpx"
	"cleanarr/internal/clients/plex"
	"cleanarr/internal/clients/radarr"
	"cleanarr/internal/clients/sonarr"
	"cleanarr/internal/clients/tautulli"
	"cleanarr/internal/config"
)

// Service names.
const (
	Sonarr   = "sonarr"
	Radarr   = "radarr"
	Plex     = "plex"
	Tautulli = "tautulli"
)

// Services lists the services in display order.
var Services = []string{Sonarr, Radarr, Plex, Tautulli}

// ServiceLabel is the display name of a service.
func ServiceLabel(svc string) string {
	switch svc {
	case Sonarr:
		return "Sonarr"
	case Radarr:
		return "Radarr"
	case Plex:
		return "Plex"
	case Tautulli:
		return "Tautulli"
	}
	return svc
}

// Field kinds.
const (
	KindURL    = "url"
	KindSecret = "secret"
)

// Field is one configurable value.
type Field struct {
	Key      string // "sonarr.url": settings key (with a prefix) and form field name
	Service  string
	Env      string // environment variable that overrides it
	Label    string
	Help     string
	Kind     string
	Required bool // needed for the service to count as configured
}

// Field keys.
const (
	SonarrURL        = "sonarr.url"
	SonarrPublicURL  = "sonarr.public_url"
	SonarrAPIKey     = "sonarr.api_key" //nolint:gosec // G101: field name, not a credential
	RadarrURL        = "radarr.url"
	RadarrPublicURL  = "radarr.public_url"
	RadarrAPIKey     = "radarr.api_key" //nolint:gosec // G101: field name, not a credential
	PlexURL          = "plex.url"
	PlexPublicURL    = "plex.public_url"
	PlexToken        = "plex.token"
	TautulliURL      = "tautulli.url"
	TautulliAPIKey   = "tautulli.api_key" //nolint:gosec // G101: field name, not a credential
	TautulliWebhook  = "tautulli.webhook_secret"
	MinWebhookSecret = 32
	publicURLHelp    = "Address your browser opens for the “open in” links. Default: the API URL."
)

// Fields lists every configurable value.
var Fields = []Field{
	{Key: SonarrURL, Service: Sonarr, Env: "SONARR_URL", Label: "API URL", Kind: KindURL, Required: true},
	{Key: SonarrPublicURL, Service: Sonarr, Env: "SONARR_PUBLIC_URL", Label: "Public URL", Help: publicURLHelp, Kind: KindURL},
	{Key: SonarrAPIKey, Service: Sonarr, Env: "SONARR_API_KEY", Label: "API key", Kind: KindSecret, Required: true,
		Help: "Settings → General → Security → API Key."},
	{Key: RadarrURL, Service: Radarr, Env: "RADARR_URL", Label: "API URL", Kind: KindURL, Required: true},
	{Key: RadarrPublicURL, Service: Radarr, Env: "RADARR_PUBLIC_URL", Label: "Public URL", Help: publicURLHelp, Kind: KindURL},
	{Key: RadarrAPIKey, Service: Radarr, Env: "RADARR_API_KEY", Label: "API key", Kind: KindSecret, Required: true,
		Help: "Settings → General → Security → API Key."},
	{Key: PlexURL, Service: Plex, Env: "PLEX_URL", Label: "Server URL", Kind: KindURL, Required: true},
	{Key: PlexPublicURL, Service: Plex, Env: "PLEX_PUBLIC_URL", Label: "Public URL", Kind: KindURL,
		Help: "Plex Web address for the “open in Plex” links. Default: app.plex.tv."},
	{Key: PlexToken, Service: Plex, Env: "PLEX_TOKEN", Label: "Token (X-Plex-Token)", Kind: KindSecret, Required: true,
		Help: "Plex Web: ⋯ → Get Info → View XML; the URL contains X-Plex-Token=…"},
	{Key: TautulliURL, Service: Tautulli, Env: "TAUTULLI_URL", Label: "API URL", Kind: KindURL, Required: true},
	{Key: TautulliAPIKey, Service: Tautulli, Env: "TAUTULLI_API_KEY", Label: "API key", Kind: KindSecret, Required: true,
		Help: "Settings → Web Interface → API (check “Enable API”)."},
	{Key: TautulliWebhook, Service: Tautulli, Env: "TAUTULLI_WEBHOOK_SECRET", Label: "Webhook secret", Kind: KindSecret,
		Help: "Optional, 32 characters or more. See Webhook below."},
}

// FieldsOf returns the fields of a service.
func FieldsOf(svc string) []Field {
	var out []Field
	for _, f := range Fields {
		if f.Service == svc {
			out = append(out, f)
		}
	}
	return out
}

// Values maps field keys to values.
type Values map[string]string

// EnvValues extracts the upstream values defined in the environment.
func EnvValues(c *config.Config) Values {
	v := Values{
		SonarrURL: c.SonarrURL, SonarrPublicURL: c.SonarrPublicURL, SonarrAPIKey: c.SonarrAPIKey,
		RadarrURL: c.RadarrURL, RadarrPublicURL: c.RadarrPublicURL, RadarrAPIKey: c.RadarrAPIKey,
		PlexURL: c.PlexURL, PlexPublicURL: c.PlexPublicURL, PlexToken: c.PlexToken,
		TautulliURL: c.TautulliURL, TautulliAPIKey: c.TautulliAPIKey, TautulliWebhook: c.TautulliWebhookSecret,
	}
	for k, val := range v {
		if val == "" {
			delete(v, k)
		}
	}
	return v
}

// complete reports whether every required field of svc is set.
func (v Values) complete(svc string) bool {
	for _, f := range FieldsOf(svc) {
		if f.Required && v[f.Key] == "" {
			return false
		}
	}
	return true
}

// Clients is an immutable snapshot of the effective configuration and the
// clients built from it. A client is nil when its service is incomplete.
type Clients struct {
	Values   Values // effective values, credentials included: never sent to the browser
	Sonarr   *sonarr.Client
	Radarr   *radarr.Client
	Plex     *plex.Client
	Tautulli *tautulli.Client
	// HistorySource is "merged", "tautulli" or "plex" (resolved HISTORY_SOURCE).
	HistorySource string
	// Missing lists the services the synchronization still needs.
	Missing []string
}

// Build creates the clients from effective values. mode is HISTORY_SOURCE
// (auto, tautulli or plex).
func Build(v Values, mode string) *Clients {
	c := &Clients{Values: v}
	if v.complete(Sonarr) {
		c.Sonarr = sonarr.New(v[SonarrURL], v[SonarrAPIKey])
	}
	if v.complete(Radarr) {
		c.Radarr = radarr.New(v[RadarrURL], v[RadarrAPIKey])
	}
	if v.complete(Plex) {
		c.Plex = plex.New(v[PlexURL], v[PlexToken])
	}
	if v.complete(Tautulli) {
		c.Tautulli = tautulli.New(v[TautulliURL], v[TautulliAPIKey])
	}
	switch {
	case mode == config.HistoryPlex:
		c.HistorySource = config.HistoryPlex
	case mode == config.HistoryTautulli:
		c.HistorySource = config.HistoryTautulli
	case mode == config.HistoryMerged || c.Tautulli != nil:
		c.HistorySource = config.HistoryMerged
	default:
		c.HistorySource = config.HistoryPlex
	}
	for _, svc := range []string{Sonarr, Radarr, Plex} {
		if !v.complete(svc) {
			c.Missing = append(c.Missing, ServiceLabel(svc))
		}
	}
	if c.HistorySource != config.HistoryPlex && c.Tautulli == nil {
		c.Missing = append(c.Missing, ServiceLabel(Tautulli))
	}
	return c
}

// Ready reports whether the synchronization can run.
func (c *Clients) Ready() bool { return len(c.Missing) == 0 }

// HistoryClient returns the Tautulli client when Tautulli feeds the history
// (tautulli or merged).
func (c *Clients) HistoryClient() *tautulli.Client {
	if c.HistorySource == config.HistoryPlex {
		return nil
	}
	return c.Tautulli
}

// PlexHistory reports whether the Plex history is read (plex or merged).
func (c *Clients) PlexHistory() bool {
	return c.Plex != nil && c.HistorySource != config.HistoryTautulli
}

// Merged reports whether both histories are read and merged.
func (c *Clients) Merged() bool { return c.HistorySource == config.HistoryMerged }

// SonarrWebURL is the Sonarr URL for browser links.
func (c *Clients) SonarrWebURL() string {
	return firstNonEmpty(c.Values[SonarrPublicURL], c.Values[SonarrURL])
}

// RadarrWebURL is the Radarr URL for browser links.
func (c *Clients) RadarrWebURL() string {
	return firstNonEmpty(c.Values[RadarrPublicURL], c.Values[RadarrURL])
}

// PlexWebURL is the Plex Web base URL ("" means app.plex.tv).
func (c *Clients) PlexWebURL() string { return c.Values[PlexPublicURL] }

// WebhookSecret is the Tautulli webhook secret ("" disables the webhook).
func (c *Clients) WebhookSecret() string { return c.Values[TautulliWebhook] }

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// Registry publishes the current Clients snapshot.
type Registry struct{ p atomic.Pointer[Clients] }

// NewRegistry returns a registry holding c.
func NewRegistry(c *Clients) *Registry {
	r := &Registry{}
	r.Set(c)
	return r
}

// Get returns the current snapshot (never nil: an empty registry reports
// every service as missing).
func (r *Registry) Get() *Clients {
	if c := r.p.Load(); c != nil {
		return c
	}
	return Build(Values{}, config.HistoryAuto)
}

// ArrHTTP returns the HTTP client of Sonarr ("series") or Radarr ("movie"),
// nil when the service is not configured (image proxy).
func (r *Registry) ArrHTTP(kind string) *httpx.Client {
	c := r.Get()
	switch {
	case kind == "movie" && c.Radarr != nil:
		return c.Radarr.HTTP
	case kind == "series" && c.Sonarr != nil:
		return c.Sonarr.HTTP
	}
	return nil
}

// Set replaces the snapshot.
func (r *Registry) Set(c *Clients) { r.p.Store(c) }

// ErrNotConfigured is returned when an operation needs a service that is not
// configured.
var ErrNotConfigured = errors.New("service not configured")

// mask renders a credential for display: "••••" plus its last 4 characters
// (only when it is long enough for that not to reveal much).
func mask(v string) string {
	if len(v) < 12 {
		return "••••"
	}
	return "••••" + v[len(v)-4:]
}

// normalizeURL trims spaces and trailing slashes.
func normalizeURL(v string) string { return strings.TrimRight(strings.TrimSpace(v), "/") }

// TestValues returns complete values for the four services at urls (keyed
// by service name), all with the same credential (tests and mockarr).
func TestValues(urls map[string]string, key string) Values {
	return Values{
		SonarrURL: urls[Sonarr], SonarrAPIKey: key, RadarrURL: urls[Radarr], RadarrAPIKey: key,
		PlexURL: urls[Plex], PlexToken: key, TautulliURL: urls[Tautulli], TautulliAPIKey: key,
	}
}
