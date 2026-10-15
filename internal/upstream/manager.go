package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"hangarr/internal/clients/httpx"
	"hangarr/internal/clients/tautulli"
	"hangarr/internal/i18n"
	"hangarr/internal/secretbox"
	"hangarr/internal/store"
)

// settingPrefix namespaces the service settings in app_settings.
const settingPrefix = "svc."

// CheckTimeout bounds a connection test.
const CheckTimeout = 10 * time.Second

// Manager merges the environment with the values saved from the UI, stores
// changes and publishes fresh clients in Registry.
type Manager struct {
	Store       *store.Store
	Env         Values // values defined in the environment: they win and are locked
	HistoryMode string // HISTORY_SOURCE
	Box         *secretbox.Box
	// KeyInDataDir is set when the master key was generated in the data
	// directory (no HANGARR_MASTER_KEY): a backup of the volume exposes
	// the credentials, the UI says so.
	KeyInDataDir bool
	Registry     *Registry
	Log          *slog.Logger

	mu sync.Mutex // serializes Save
}

// KeyFileName is the master key generated in DATA_DIR when none is provided.
const KeyFileName = "master.key"

// SetupKey installs the master key. A key from the environment (envKey) wins:
// if a generated key file is still present, the saved credentials are
// re-encrypted with envKey and the file is removed. Without envKey, the key
// file is read or generated.
func (m *Manager) SetupKey(ctx context.Context, envKey []byte, keyFile string) error {
	if envKey == nil {
		key, created, err := secretbox.LoadOrCreateKeyFile(keyFile)
		if err != nil {
			return fmt.Errorf("master key file: %w", err)
		}
		if m.Box, err = secretbox.New(key); err != nil {
			return err
		}
		m.KeyInDataDir = true
		if created {
			m.Log.Warn("no HANGARR_MASTER_KEY: generated a master key in the data directory; "+
				"a backup of the volume exposes the saved credentials", "file", keyFile)
		}
		return nil
	}
	box, err := secretbox.New(envKey)
	if err != nil {
		return err
	}
	m.Box = box
	oldKey, err := secretbox.ReadKeyFile(keyFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		// Unreadable leftover: keep it, the credentials it protects cannot
		// be migrated and will be reported as undecryptable.
		m.Log.Error("cannot read the generated master key; saved credentials must be entered again", "err", err)
		return nil
	}
	old, err := secretbox.New(oldKey)
	if err != nil {
		return err
	}
	n, err := m.rekey(ctx, old)
	if err != nil {
		// The file stays: the migration is retried at the next start.
		m.Log.Error("re-encrypting credentials with HANGARR_MASTER_KEY failed", "err", err)
		return nil
	}
	if err := os.Remove(keyFile); err != nil {
		m.Log.Error("credentials re-encrypted but the generated key file could not be removed", "file", keyFile, "err", err)
		return nil
	}
	m.Log.Info("credentials re-encrypted with HANGARR_MASTER_KEY; generated key file removed", "count", n)
	return nil
}

// rekey re-encrypts with m.Box the credentials that open with old.
func (m *Manager) rekey(ctx context.Context, old *secretbox.Box) (int, error) {
	secret := map[string]bool{}
	for _, f := range Fields {
		secret[f.Key] = f.Kind == KindSecret
	}
	return m.Store.RewriteSettings(ctx, settingPrefix, time.Now(), func(key, value string) (string, bool, error) {
		if !secret[key] {
			return "", false, nil
		}
		if _, err := m.Box.Open(key, value); err == nil {
			return "", false, nil // already migrated
		}
		plain, err := old.Open(key, value)
		if err != nil {
			return "", false, nil // undecryptable with both keys: left as is
		}
		sealed, err := m.Box.Seal(key, plain)
		return sealed, err == nil, err
	})
}

// CanStoreSecrets reports whether a master key is available.
func (m *Manager) CanStoreSecrets() bool { return m.Box != nil }

// stored returns the saved values, credentials decrypted, and the keys of
// credentials that cannot be decrypted (no or different master key).
func (m *Manager) stored(ctx context.Context) (Values, map[string]bool, error) {
	raw, err := m.Store.SettingsWithPrefix(ctx, settingPrefix)
	if err != nil {
		return nil, nil, err
	}
	vals, broken := Values{}, map[string]bool{}
	for _, f := range Fields {
		v, ok := raw[f.Key]
		if !ok {
			continue
		}
		if f.Kind == KindSecret {
			if m.Box == nil {
				broken[f.Key] = true
				continue
			}
			plain, err := m.Box.Open(f.Key, v)
			if err != nil {
				broken[f.Key] = true
				continue
			}
			v = plain
		}
		vals[f.Key] = v
	}
	return vals, broken, nil
}

func (m *Manager) effective(saved Values) Values {
	out := Values{}
	for k, v := range saved {
		out[k] = v
	}
	for k, v := range m.Env {
		if v != "" {
			out[k] = v
		}
	}
	return out
}

// Reload rebuilds the clients from the environment and the database.
func (m *Manager) Reload(ctx context.Context) (*Clients, error) {
	saved, broken, err := m.stored(ctx)
	if err != nil {
		return nil, err
	}
	for k := range broken {
		if m.Env[k] == "" {
			m.Log.Warn("saved credential cannot be decrypted; enter it again in Configuration", "field", k)
		}
	}
	c := Build(m.effective(saved), m.HistoryMode)
	m.Registry.Set(c)
	return c, nil
}

// FieldState is a field as shown in the Settings › Services page. Credentials are
// never included, only a masked hint.
type FieldState struct {
	Field
	Value  string // current value of a URL field
	Hint   string // masked credential, "" when unset
	Locked bool   // defined by the environment
	Broken bool   // saved credential that cannot be decrypted
}

// ServiceState is the Settings › Services page section of one service.
type ServiceState struct {
	Service    string
	Label      string
	Fields     []FieldState
	Configured bool
	Locked     bool   // every field comes from the environment
	Disabled   string // why the service cannot be configured now (English key), "" when it can
}

// State describes every service for the Settings › Services page.
func (m *Manager) State(ctx context.Context) ([]ServiceState, error) {
	saved, broken, err := m.stored(ctx)
	if err != nil {
		return nil, err
	}
	eff := m.effective(saved)
	out := make([]ServiceState, 0, len(Services))
	for _, svc := range Services {
		s := ServiceState{Service: svc, Label: ServiceLabel(svc), Configured: eff.complete(svc), Locked: true}
		s.Disabled = Disabled(eff, svc)
		for _, f := range FieldsOf(svc) {
			fs := FieldState{Field: f, Locked: m.Env[f.Key] != ""}
			fs.Broken = !fs.Locked && broken[f.Key]
			if f.Kind == KindSecret {
				if v := eff[f.Key]; v != "" {
					fs.Hint = mask(v)
				}
			} else {
				fs.Value = eff[f.Key]
			}
			s.Locked = s.Locked && fs.Locked
			s.Fields = append(s.Fields, fs)
		}
		out = append(out, s)
	}
	return out, nil
}

// Input is a submitted Configuration form for one service. An empty
// credential keeps the current one; Clear removes it.
type Input struct {
	Values map[string]string
	Clear  map[string]bool
}

// ValidationError maps field keys to messages shown next to the fields.
type ValidationError map[string]string

func (e ValidationError) Error() string {
	keys := make([]string, 0, len(e))
	for k := range e {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return "invalid fields: " + strings.Join(keys, ", ")
}

func validURL(v string) bool {
	u, err := url.Parse(v)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil
}

// appSections are Sonarr/Radarr pages that take a sub-path (/movie/27205,
// /settings/general…).
var appSections = map[string]bool{"movie": true, "series": true, "settings": true, "system": true,
	"activity": true, "wanted": true, "calendar": true, "add": true}

// pageOfTheApp reports whether a public URL points to a page of the
// application instead of the application itself (a copied address bar:
// https://radarr…/login): links are built by appending to it, so they would
// open that page. A base path (https://host/radarr) is fine.
func pageOfTheApp(svc, raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return true
	}
	segs := strings.Split(strings.Trim(strings.ToLower(u.Path), "/"), "/")
	switch last := segs[len(segs)-1]; {
	case last == "login" || last == "logout" || last == "index.html":
		return true
	case (svc == Plex || svc == Jellyfin) && last == "web":
		return true
	}
	for i := 0; i+1 < len(segs); i++ {
		if appSections[segs[i]] {
			return true
		}
	}
	return false
}

// keptCredentials refuses to send a kept credential (empty field: keep the
// current value) to another server: when the API address of svc moves to
// another origin (scheme, host or port), its credentials must be typed
// again. Otherwise a session alone could point the service to a host it
// controls and receive the API key or Plex token, which the interface never
// shows. The webhook secret is not concerned (it is never sent out).
func (m *Manager) keptCredentials(svc string, current Values, in Input) ValidationError {
	apiURL := svc + ".url"
	next := normalizeURL(in.Values[apiURL])
	if m.Env[apiURL] != "" || next == "" || sameOrigin(next, current[apiURL]) {
		return nil
	}
	verr := ValidationError{}
	for _, f := range FieldsOf(svc) {
		if f.Kind != KindSecret || f.Key == TautulliWebhook || current[f.Key] == "" ||
			strings.TrimSpace(in.Values[f.Key]) != "" || in.Clear[f.Key] {
			continue
		}
		if m.Env[f.Key] != "" {
			verr[apiURL] = "The credential is set by the environment: change this address there too."
			continue
		}
		verr[f.Key] = "The address changed: type the credential again."
	}
	return verr
}

// sameOrigin reports whether two URLs share scheme, host and port.
func sameOrigin(a, b string) bool {
	ua, errA := url.Parse(a)
	ub, errB := url.Parse(b)
	if errA != nil || errB != nil || b == "" {
		return false
	}
	port := func(u *url.URL) string {
		if p := u.Port(); p != "" {
			return p
		}
		if strings.EqualFold(u.Scheme, "https") {
			return "443"
		}
		return "80"
	}
	return strings.EqualFold(ua.Scheme, ub.Scheme) && strings.EqualFold(ua.Hostname(), ub.Hostname()) && port(ua) == port(ub)
}

// apply computes the effective values of svc after the input, starting from
// saved, and the settings to write. Locked fields are ignored.
func (m *Manager) apply(svc string, saved Values, broken map[string]bool, in Input) (set map[string]string, del, changed []string, err error) {
	set = map[string]string{}
	verr := ValidationError{}
	for _, f := range FieldsOf(svc) {
		if m.Env[f.Key] != "" {
			continue
		}
		key := settingPrefix + f.Key
		_, has := saved[f.Key]
		has = has || broken[f.Key]
		if f.Kind == KindURL {
			v := normalizeURL(in.Values[f.Key])
			switch {
			case v != "" && !validURL(v):
				verr[f.Key] = "Expected an http(s) address, e.g. http://host:port."
			case v != "" && strings.HasSuffix(f.Key, ".public_url") && pageOfTheApp(svc, v):
				verr[f.Key] = "This is the address of a page (/login, /movie/…): keep only the address of the application."
			case v == saved[f.Key] && (v != "" || !has):
			case v == "":
				del, changed = append(del, key), append(changed, f.Key)
			default:
				set[key], changed = v, append(changed, f.Key)
			}
			continue
		}
		v := strings.TrimSpace(in.Values[f.Key])
		switch {
		case in.Clear[f.Key] && v == "": // a value typed at the same time wins
			if has {
				del, changed = append(del, key), append(changed, f.Key)
			}
		case v == "" || (v == saved[f.Key] && !broken[f.Key]):
		case f.Key == TautulliWebhook && len(v) < MinWebhookSecret:
			verr[f.Key] = "At least 32 characters." // MinWebhookSecret
		case m.Box == nil:
			verr[f.Key] = "Cannot save: no master key (HANGARR_MASTER_KEY)."
		default:
			sealed, serr := m.Box.Seal(f.Key, v)
			if serr != nil {
				return nil, nil, nil, serr
			}
			set[key], changed = sealed, append(changed, f.Key)
		}
	}
	for k, msg := range m.keptCredentials(svc, m.effective(saved), in) {
		if _, ok := verr[k]; !ok {
			verr[k] = msg
		}
	}
	if len(verr) > 0 {
		return nil, nil, nil, verr
	}
	return set, del, changed, nil
}

// Save validates and stores the editable fields of svc, records ev (its
// Event is set here) in the security journal, then installs fresh clients.
// It returns the keys that changed (none: nothing written).
func (m *Manager) Save(ctx context.Context, svc string, in Input, ev store.AuthEvent, now time.Time) ([]string, error) {
	if !slices.Contains(Services, svc) {
		return nil, fmt.Errorf("unknown service %q", svc)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	saved, broken, err := m.stored(ctx)
	if err != nil {
		return nil, err
	}
	set, del, changed, err := m.apply(svc, saved, broken, in)
	if err != nil || len(changed) == 0 {
		return nil, err
	}
	// Clearing a service is always allowed; only a value written is refused.
	if reason := Disabled(m.effective(saved), svc); len(set) > 0 && reason != "" {
		return nil, ValidationError{svc + ".url": reason}
	}
	ev.Event = "config_" + svc
	ev.At = now.Unix()
	if err := m.Store.ApplySettings(ctx, set, del, now, ev); err != nil {
		return nil, err
	}
	if _, err := m.Reload(ctx); err != nil {
		return nil, err
	}
	m.Log.Info("service configuration changed", "service", svc, "fields", changed, "by", ev.Username)
	return changed, nil
}

// Test checks the connection of svc with the submitted values (current
// values for empty credentials) without saving anything.
func (m *Manager) Test(ctx context.Context, svc string, in Input) (string, error) {
	saved, _, err := m.stored(ctx)
	if err != nil {
		return "", err
	}
	v := m.effective(saved)
	if reason := Disabled(v, svc); reason != "" {
		return "", ValidationError{svc + ".url": reason}
	}
	if verr := m.keptCredentials(svc, v, in); len(verr) > 0 {
		return "", verr
	}
	for _, f := range FieldsOf(svc) {
		if m.Env[f.Key] != "" {
			continue
		}
		val := strings.TrimSpace(in.Values[f.Key])
		switch {
		case f.Kind == KindURL:
			v[f.Key] = normalizeURL(val)
		case val != "":
			v[f.Key] = val
		}
	}
	if u := v[svc+".url"]; u != "" && !validURL(u) {
		return "", ValidationError{svc + ".url": "Expected an http(s) address, e.g. http://host:port."}
	}
	return Check(ctx, Build(v, m.HistoryMode), svc)
}

// Check tests one service of c and returns its version.
func Check(ctx context.Context, c *Clients, svc string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, CheckTimeout)
	defer cancel()
	switch svc {
	case Sonarr, Radarr:
		var app, version string
		switch {
		case svc == Sonarr && c.Sonarr != nil:
			s, err := c.Sonarr.SystemStatus(ctx)
			if err != nil {
				return "", err
			}
			app, version = s.AppName, s.Version
		case svc == Radarr && c.Radarr != nil:
			s, err := c.Radarr.SystemStatus(ctx)
			if err != nil {
				return "", err
			}
			app, version = s.AppName, s.Version
		default:
			return "", ErrNotConfigured
		}
		if app != "" && !strings.EqualFold(app, svc) {
			return "", &WrongServiceError{Want: ServiceLabel(svc), Got: app}
		}
		return version, nil
	case Plex:
		if c.Plex == nil {
			return "", ErrNotConfigured
		}
		return c.Plex.Check(ctx)
	case Jellyfin:
		if c.Jellyfin == nil {
			return "", ErrNotConfigured
		}
		info, err := c.Jellyfin.SystemInfo(ctx)
		if err != nil {
			return "", err
		}
		if info.ProductName != "" && !strings.Contains(strings.ToLower(info.ProductName), "jellyfin") {
			return "", &WrongServiceError{Want: ServiceLabel(Jellyfin), Got: info.ProductName}
		}
		return info.Version, nil
	case Tautulli:
		if c.Tautulli == nil {
			return "", ErrNotConfigured
		}
		return c.Tautulli.Info(ctx)
	}
	return "", fmt.Errorf("unknown service %q", svc)
}

// WrongServiceError is returned when the URL points to another application.
type WrongServiceError struct{ Want, Got string }

func (e *WrongServiceError) Error() string {
	return fmt.Sprintf("this URL serves %s, not %s", e.Got, e.Want)
}

// Describe explains a connection error for the UI. Messages never contain
// credentials (httpx strips URLs from errors).
func Describe(l i18n.Localizer, err error) string {
	var se *httpx.StatusError
	var te *tautulli.APIError
	var we *WrongServiceError
	var re *httpx.RedirectError
	var syn *json.SyntaxError
	var ut *json.UnmarshalTypeError
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrNotConfigured):
		return l.T("Required fields missing (URL and key).")
	case errors.As(err, &re) && re.Downgrade:
		return l.T("This address redirects from https to http: refused so the key does not travel in clear. Enter the final address.")
	case errors.As(err, &re):
		return l.T("This address redirects to another host (%s): refused so the key is not sent there. Enter the final address.", re.Host)
	case errors.As(err, &we):
		return l.T("This address answers as %s, not %s.", we.Got, we.Want)
	case errors.As(err, &se) && (se.Status == http.StatusUnauthorized || se.Status == http.StatusForbidden):
		return l.T("Access denied (HTTP %d): check the API key or token.", se.Status)
	case errors.As(err, &se) && se.Status == http.StatusNotFound:
		return l.T("Not found (HTTP 404): check the URL (port, base path).")
	case errors.As(err, &se):
		return l.T("Unexpected response (HTTP %d).", se.Status)
	case errors.As(err, &te):
		return l.T("Tautulli refused the request: %s.", te.Message)
	case errors.As(err, &syn), errors.As(err, &ut):
		return l.T("Unreadable response: this address does not look like the expected API.")
	case errors.Is(err, context.DeadlineExceeded):
		return l.T("No answer within %d s.", int(CheckTimeout/time.Second))
	default:
		return l.T("Unreachable: %s", err.Error())
	}
}
