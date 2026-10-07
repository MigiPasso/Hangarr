package upstream

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hangarr/internal/i18n"
	"hangarr/internal/mockarr"
	"hangarr/internal/secretbox"
	"hangarr/internal/store"
)

const apiKey = "0123456789abcdef0123456789abcdef"

// fr: the assertions below check the French messages.
var fr = i18n.For(i18n.FR)

func newBox(t *testing.T, fill string) *secretbox.Box {
	t.Helper()
	b, err := secretbox.New([]byte(strings.Repeat(fill, secretbox.KeySize)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func newManager(t *testing.T, env Values, box *secretbox.Box) *Manager {
	t.Helper()
	st, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return &Manager{Store: st, Env: env, HistoryMode: "auto", Box: box, Registry: NewRegistry(nil),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func input(kv ...string) Input {
	in := Input{Values: map[string]string{}, Clear: map[string]bool{}}
	for i := 0; i+1 < len(kv); i += 2 {
		in.Values[kv[i]] = kv[i+1]
	}
	return in
}

func save(t *testing.T, m *Manager, svc string, in Input) []string {
	t.Helper()
	changed, err := m.Save(context.Background(), svc, in, store.AuthEvent{Username: "admin"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return changed
}

func state(t *testing.T, m *Manager, svc string) ServiceState {
	t.Helper()
	states, err := m.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range states {
		if s.Service == svc {
			return s
		}
	}
	t.Fatalf("no state for %s", svc)
	return ServiceState{}
}

func field(s ServiceState, key string) FieldState {
	for _, f := range s.Fields {
		if f.Key == key {
			return f
		}
	}
	return FieldState{}
}

func TestBuild(t *testing.T) {
	c := Build(Values{SonarrURL: "http://s", SonarrAPIKey: "k"}, "auto")
	if c.Sonarr == nil || c.Radarr != nil || c.Ready() {
		t.Fatalf("partial config: %+v", c)
	}
	if strings.Join(c.Missing, ",") != "Radarr,Plex" || c.HistorySource != "plex" {
		t.Errorf("missing=%v history=%s", c.Missing, c.HistorySource)
	}
	full := TestValues(map[string]string{Sonarr: "http://s", Radarr: "http://r", Plex: "http://p", Tautulli: "http://t"}, "k")
	if c := Build(full, "auto"); !c.Ready() || c.HistoryClient() == nil {
		t.Errorf("full config: ready=%v history=%s", c.Ready(), c.HistorySource)
	}
	if c := Build(full, "plex"); c.HistoryClient() != nil || c.Tautulli == nil {
		t.Error("HISTORY_SOURCE=plex keeps the Tautulli client but not as history source")
	}
	delete(full, TautulliAPIKey)
	if c := Build(full, "tautulli"); c.Ready() || c.Missing[0] != "Tautulli" {
		t.Errorf("HISTORY_SOURCE=tautulli without Tautulli: %v", c.Missing)
	}
	if c := NewRegistry(nil).Get(); c.Ready() {
		t.Error("an empty registry must not be ready")
	}
}

func TestSaveEncryptsAndReloads(t *testing.T) {
	m := newManager(t, nil, newBox(t, "k"))
	changed := save(t, m, Sonarr, input(SonarrURL, " http://sonarr:8989/ ", SonarrAPIKey, apiKey))
	if len(changed) != 2 {
		t.Fatalf("changed = %v", changed)
	}
	if c := m.Registry.Get(); c.Sonarr == nil || c.Values[SonarrURL] != "http://sonarr:8989" {
		t.Fatalf("clients not rebuilt: %+v", c.Values)
	}
	var raw string
	if err := m.Store.DB().QueryRow(`SELECT value FROM app_settings WHERE key = 'svc.sonarr.api_key'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, apiKey) || !strings.HasPrefix(raw, "v1:") {
		t.Errorf("credential stored in clear: %q", raw)
	}
	f := field(state(t, m, Sonarr), SonarrAPIKey)
	if f.Hint != "••••cdef" || f.Value != "" {
		t.Errorf("credential state: %+v", f)
	}
	var n int
	_ = m.Store.DB().QueryRow(`SELECT COUNT(*) FROM auth_events WHERE event = 'config_sonarr' AND username = 'admin'`).Scan(&n)
	if n != 1 {
		t.Errorf("security journal entries: %d", n)
	}

	// Empty credential = keep; same values = nothing written.
	if changed := save(t, m, Sonarr, input(SonarrURL, "http://sonarr:8989")); len(changed) != 0 {
		t.Errorf("unchanged form wrote %v", changed)
	}
	if m.Registry.Get().Values[SonarrAPIKey] != apiKey {
		t.Error("empty credential field must keep the saved value")
	}
	// « Effacer » checked while typing a new key: the typed key wins.
	in := input(SonarrURL, "http://sonarr:8989", SonarrAPIKey, "new-key-0123456789")
	in.Clear[SonarrAPIKey] = true
	save(t, m, Sonarr, in)
	if m.Registry.Get().Values[SonarrAPIKey] != "new-key-0123456789" {
		t.Error("a typed credential must win over « Effacer »")
	}
	// Clear.
	in = input(SonarrURL, "")
	in.Clear[SonarrAPIKey] = true
	save(t, m, Sonarr, in)
	if c := m.Registry.Get(); c.Sonarr != nil || c.Values[SonarrAPIKey] != "" || c.Values[SonarrURL] != "" {
		t.Errorf("cleared values still active: %+v", c.Values)
	}
}

func TestSaveValidation(t *testing.T) {
	m := newManager(t, nil, nil)
	_, err := m.Save(context.Background(), Tautulli, input(TautulliURL, "ftp://x", TautulliAPIKey, "key",
		TautulliWebhook, "short"), store.AuthEvent{}, time.Now())
	var verr ValidationError
	if !errors.As(err, &verr) || verr[TautulliURL] == "" || verr[TautulliAPIKey] == "" || verr[TautulliWebhook] == "" {
		t.Fatalf("expected errors on the three fields, got %v", err)
	}
	if !strings.Contains(verr[TautulliAPIKey], "master key") {
		t.Errorf("no master key message: %q", verr[TautulliAPIKey])
	}
	// URLs can be saved without a master key.
	save(t, m, Tautulli, input(TautulliURL, "http://tautulli:8181"))
	if m.Registry.Get().Values[TautulliURL] != "http://tautulli:8181" {
		t.Error("URL not saved without master key")
	}
	if _, err := m.Save(context.Background(), "prowlarr", input(), store.AuthEvent{}, time.Now()); err == nil {
		t.Error("unknown service must fail")
	}
}

func TestEnvironmentWins(t *testing.T) {
	m := newManager(t, Values{SonarrURL: "http://env-sonarr:8989", SonarrAPIKey: "env-key-0123456789"}, newBox(t, "k"))
	if changed := save(t, m, Sonarr, input(SonarrURL, "http://ui:8989", SonarrAPIKey, "ui-key")); len(changed) != 0 {
		t.Errorf("locked fields written: %v", changed)
	}
	st := state(t, m, Sonarr)
	if st.Locked || !st.Configured || field(st, SonarrURL).Value != "http://env-sonarr:8989" {
		t.Errorf("state: %+v", st)
	}
	if f := field(st, SonarrPublicURL); f.Locked {
		t.Error("public URL is not set in the environment and must stay editable")
	}
	if m.Registry.Get().Values[SonarrAPIKey] != "" {
		t.Error("registry loaded before Reload")
	}
	if _, err := m.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.Registry.Get().Values[SonarrAPIKey] != "env-key-0123456789" {
		t.Error("environment value not effective")
	}
}

func TestMasterKeyChanged(t *testing.T) {
	m := newManager(t, nil, newBox(t, "k"))
	save(t, m, Plex, input(PlexURL, "http://plex:32400", PlexToken, "token-0123456789"))
	m.Box = newBox(t, "z")
	c, err := m.Reload(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if c.Plex != nil {
		t.Error("an undecryptable token must not build a client")
	}
	if f := field(state(t, m, Plex), PlexToken); !f.Broken || f.Hint != "" {
		t.Errorf("token state: %+v", f)
	}
	save(t, m, Plex, input(PlexURL, "http://plex:32400", PlexToken, "token-0123456789"))
	if m.Registry.Get().Plex == nil {
		t.Error("re-entering the token must repair the configuration")
	}
}

func TestConnectionTest(t *testing.T) {
	mock := mockarr.New(os.DirFS("../../testdata"), apiKey)
	urls := map[string]string{}
	for name, h := range map[string]http.Handler{Sonarr: mock.Sonarr(), Radarr: mock.Radarr(), Plex: mock.Plex(), Tautulli: mock.Tautulli()} {
		ts := httptest.NewServer(h)
		t.Cleanup(ts.Close)
		urls[name] = ts.URL
	}
	m := newManager(t, nil, newBox(t, "k"))
	ctx := context.Background()
	for _, svc := range Services {
		in := input(svc+".url", urls[svc], FieldsOf(svc)[2].Key, apiKey)
		if svc == Tautulli {
			in = input(TautulliURL, urls[svc], TautulliAPIKey, apiKey)
		}
		v, err := m.Test(ctx, svc, in)
		if err != nil || !strings.Contains(v, "mock") {
			t.Errorf("%s: version %q, err %v", svc, v, err)
		}
	}
	_, err := m.Test(ctx, Sonarr, input(SonarrURL, urls[Sonarr], SonarrAPIKey, "wrong"))
	if msg := Describe(fr, err); !strings.Contains(msg, "Accès refusé (HTTP 401)") {
		t.Errorf("bad key: %q", msg)
	}
	_, err = m.Test(ctx, Sonarr, input(SonarrURL, urls[Radarr], SonarrAPIKey, apiKey))
	if msg := Describe(fr, err); !strings.Contains(msg, "Radarr, pas comme Sonarr") {
		t.Errorf("wrong service: %q", msg)
	}
	_, err = m.Test(ctx, Tautulli, input(TautulliURL, urls[Tautulli], TautulliAPIKey, "wrong"))
	if err == nil {
		t.Error("bad Tautulli key must fail")
	}
	if _, err := m.Test(ctx, Plex, input(PlexURL, "")); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("missing fields: %v", err)
	}
	// Testing never saves anything.
	if s := state(t, m, Sonarr); s.Configured {
		t.Error("Test must not save")
	}
}

func TestSetupKeyGeneratedThenMigrated(t *testing.T) {
	ctx := context.Background()
	keyFile := filepath.Join(t.TempDir(), KeyFileName)
	m := newManager(t, nil, nil)
	if err := m.SetupKey(ctx, nil, keyFile); err != nil {
		t.Fatal(err)
	}
	if !m.KeyInDataDir || !m.CanStoreSecrets() {
		t.Fatal("a key must be generated in the data directory")
	}
	save(t, m, Plex, input(PlexURL, "http://plex:32400", PlexToken, "token-0123456789"))

	// Restart without HANGARR_MASTER_KEY: same generated key.
	restart := func(envKey []byte) *Manager {
		t.Helper()
		m2 := &Manager{Store: m.Store, HistoryMode: "auto", Registry: NewRegistry(nil), Log: m.Log}
		if err := m2.SetupKey(ctx, envKey, keyFile); err != nil {
			t.Fatal(err)
		}
		if _, err := m2.Reload(ctx); err != nil {
			t.Fatal(err)
		}
		return m2
	}
	if m2 := restart(nil); m2.Registry.Get().Values[PlexToken] != "token-0123456789" {
		t.Fatal("the generated key must be reused after a restart")
	}

	// HANGARR_MASTER_KEY appears: credentials re-encrypted, file removed.
	envKey := []byte(strings.Repeat("e", 32))
	m3 := restart(envKey)
	if m3.KeyInDataDir || m3.Registry.Get().Values[PlexToken] != "token-0123456789" {
		t.Fatalf("migration: inDataDir=%v token=%q", m3.KeyInDataDir, m3.Registry.Get().Values[PlexToken])
	}
	if _, err := os.Stat(keyFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("generated key file must be removed: %v", err)
	}
	envBox, _ := secretbox.New(envKey)
	var raw string
	_ = m.Store.DB().QueryRow(`SELECT value FROM app_settings WHERE key = 'svc.plex.token'`).Scan(&raw)
	if v, err := envBox.Open(PlexToken, raw); err != nil || v != "token-0123456789" {
		t.Errorf("token not sealed with the environment key: %v", err)
	}
	// Next start: nothing left to migrate, the URL is untouched.
	if m4 := restart(envKey); m4.Registry.Get().Values[PlexURL] != "http://plex:32400" {
		t.Error("URL changed by the migration")
	}
}
