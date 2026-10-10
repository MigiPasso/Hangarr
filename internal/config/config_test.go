package config

import (
	"errors"
	"io/fs"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) LookupFunc {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func files(m map[string]string) ReadFileFunc {
	return func(name string) ([]byte, error) {
		v, ok := m[name]
		if !ok {
			return nil, fs.ErrNotExist
		}
		return []byte(v), nil
	}
}

func upstreams() map[string]string {
	return map[string]string{
		"SONARR_URL": "http://sonarr:8989/", "SONARR_API_KEY": "s",
		"RADARR_URL": "http://radarr:7878", "RADARR_API_KEY": "r",
		"PLEX_URL": "http://plex:32400", "PLEX_TOKEN": "p",
	}
}

func TestDefaults(t *testing.T) {
	c, err := Load(env(upstreams()), files(nil))
	if err != nil {
		t.Fatal(err)
	}
	if !c.DryRun || !c.CookieSecure || !c.ConfirmByTyping {
		t.Error("DRY_RUN, COOKIE_SECURE and CONFIRM_BY_TYPING must default to true")
	}
	if c.ListenAddr != ":8080" || c.DataDir != "/data" || c.AdminUsername != "admin" {
		t.Errorf("unexpected defaults: %+v", c)
	}
	if c.SessionIdleTimeout != 12*time.Hour || c.SessionMaxAge != 168*time.Hour {
		t.Errorf("unexpected session durations: %s %s", c.SessionIdleTimeout, c.SessionMaxAge)
	}
	if c.LoginMaxPerIP != 10 || c.SyncTime != "04:00" {
		t.Errorf("unexpected limits: %d %s", c.LoginMaxPerIP, c.SyncTime)
	}
	if len(c.ProtectedTags) != 1 || c.ProtectedTags[0] != "keep" {
		t.Errorf("PROTECTED_TAGS default: %v", c.ProtectedTags)
	}
	if c.Location.String() != "Europe/Paris" {
		t.Errorf("TZ default: %s", c.Location)
	}
	if c.SonarrURL != "http://sonarr:8989" {
		t.Errorf("trailing slash not trimmed: %s", c.SonarrURL)
	}
	if c.HistorySource != HistoryAuto || c.MasterKey != nil {
		t.Errorf("HISTORY_SOURCE and master key defaults: %s %v", c.HistorySource, c.MasterKey)
	}
}

func TestJellyfinSettings(t *testing.T) {
	m := upstreams()
	delete(m, "PLEX_URL")
	delete(m, "PLEX_TOKEN")
	m["JELLYFIN_URL"] = "http://jellyfin:8096/"
	m["JELLYFIN_API_KEY_FILE"] = "/run/secrets/jf"
	m["JELLYFIN_PUBLIC_URL"] = "https://jf.example.com/"
	c, err := Load(env(m), files(map[string]string{"/run/secrets/jf": "j\n"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.JellyfinURL != "http://jellyfin:8096" || c.JellyfinAPIKey != "j" || c.JellyfinPublicURL != "https://jf.example.com" {
		t.Errorf("Jellyfin settings: %q %q %q", c.JellyfinURL, c.JellyfinAPIKey, c.JellyfinPublicURL)
	}
	m["JELLYFIN_URL"] = "ftp://jellyfin"
	if _, err := Load(env(m), files(map[string]string{"/run/secrets/jf": "j"})); err == nil || !strings.Contains(err.Error(), "JELLYFIN_URL") {
		t.Errorf("invalid JELLYFIN_URL: %v", err)
	}
}

func TestPlexAndJellyfinExclusive(t *testing.T) {
	m := upstreams()
	m["JELLYFIN_URL"] = "http://jellyfin:8096"
	if _, err := Load(env(m), files(nil)); err == nil || !strings.Contains(err.Error(), "PLEX_* and JELLYFIN_*") {
		t.Errorf("PLEX_URL and JELLYFIN_URL: %v", err)
	}
	m = upstreams()
	delete(m, "PLEX_URL")
	m["JELLYFIN_API_KEY"] = "j"
	if _, err := Load(env(m), files(nil)); err == nil || !strings.Contains(err.Error(), "PLEX_* and JELLYFIN_*") {
		t.Errorf("PLEX_TOKEN and JELLYFIN_API_KEY: %v", err)
	}
}

func TestSecretFile(t *testing.T) {
	m := upstreams()
	delete(m, "SONARR_API_KEY")
	m["SONARR_API_KEY_FILE"] = "/run/secrets/sonarr"
	c, err := Load(env(m), files(map[string]string{"/run/secrets/sonarr": "from-file\n"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.SonarrAPIKey != "from-file" {
		t.Errorf("got %q", c.SonarrAPIKey)
	}
}

func TestSecretBothDefined(t *testing.T) {
	m := upstreams()
	m["SONARR_API_KEY_FILE"] = "/run/secrets/sonarr"
	_, err := Load(env(m), files(map[string]string{"/run/secrets/sonarr": "x"}))
	if err == nil || !strings.Contains(err.Error(), "both set") {
		t.Fatalf("expected conflict error, got %v", err)
	}
}

func TestSecretFileMissing(t *testing.T) {
	m := upstreams()
	delete(m, "PLEX_TOKEN")
	m["PLEX_TOKEN_FILE"] = "/nope"
	_, err := Load(env(m), files(nil))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("expected not-exist error, got %v", err)
	}
}

func TestHistorySource(t *testing.T) {
	m := upstreams()
	m["HISTORY_SOURCE"] = "Tautulli"
	c, err := Load(env(m), files(nil))
	if err != nil {
		t.Fatal(err)
	}
	// Tautulli may be configured later from the UI: no check at load time.
	if c.HistorySource != HistoryTautulli {
		t.Errorf("got %s", c.HistorySource)
	}
	m["HISTORY_SOURCE"] = "jellyfin"
	if _, err := Load(env(m), files(nil)); err == nil {
		t.Error("unknown HISTORY_SOURCE must fail")
	}
}

func TestMasterKey(t *testing.T) {
	m := upstreams()
	m["HANGARR_MASTER_KEY_FILE"] = "/run/secrets/master"
	key := strings.Repeat("ab", 32)
	c, err := Load(env(m), files(map[string]string{"/run/secrets/master": key + "\n"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.MasterKey) != 32 {
		t.Errorf("master key: %d bytes", len(c.MasterKey))
	}
	m = upstreams()
	m["HANGARR_MASTER_KEY"] = "too-short"
	_, err = Load(env(m), files(nil))
	if err == nil || strings.Contains(err.Error(), "too-short") {
		t.Errorf("invalid master key must fail without echoing it: %v", err)
	}
}

// CLEANARR_MASTER_KEY(_FILE), from before the rename, still works.
func TestLegacyMasterKey(t *testing.T) {
	key := strings.Repeat("cd", 32)
	m := upstreams()
	m["CLEANARR_MASTER_KEY_FILE"] = "/run/secrets/old"
	c, err := Load(env(m), files(map[string]string{"/run/secrets/old": key}))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.MasterKey) != 32 || len(c.Deprecations) != 1 || !strings.Contains(c.Deprecations[0], "rename it HANGARR_MASTER_KEY") {
		t.Errorf("legacy key: %d bytes, %q", len(c.MasterKey), c.Deprecations)
	}
	m["HANGARR_MASTER_KEY"] = strings.Repeat("ef", 32)
	if c, err = Load(env(m), files(map[string]string{"/run/secrets/old": key})); err != nil {
		t.Fatal(err)
	}
	if c.MasterKey[0] != 0xef || len(c.Deprecations) != 1 || !strings.Contains(c.Deprecations[0], "ignored") {
		t.Errorf("new name wins: %x %q", c.MasterKey[0], c.Deprecations)
	}
	m = upstreams()
	m["CLEANARR_MASTER_KEY"] = "too-short"
	if _, err := Load(env(m), files(nil)); err == nil || !strings.Contains(err.Error(), "CLEANARR_MASTER_KEY") {
		t.Errorf("invalid legacy key names the variable: %v", err)
	}
}

func TestInvalidValues(t *testing.T) {
	cases := map[string]string{
		"DRY_RUN":                 "maybe",
		"SYNC_TIME":               "25:00",
		"SONARR_URL":              "ftp://x",
		"TRUSTED_PROXIES":         "10.0.0.0/8,nope",
		"LOG_FORMAT":              "xml",
		"LOG_LEVEL":               "loud",
		"TZ":                      "Mars/Olympus",
		"TAUTULLI_WEBHOOK_SECRET": "short",
		"LOGIN_MAX_PER_IP":        "0",
		"SESSION_MAX_AGE":         "1h",
	}
	for k, v := range cases {
		t.Run(k, func(t *testing.T) {
			m := upstreams()
			m[k] = v
			if _, err := Load(env(m), files(nil)); err == nil || !strings.Contains(err.Error(), k) {
				t.Errorf("%s=%q: expected error mentioning the key, got %v", k, v, err)
			}
		})
	}
}

func TestAdminPassword(t *testing.T) {
	c, err := Load(env(upstreams()), files(nil))
	if err != nil || c.AdminPassword != "" {
		t.Fatalf("default: %q %v", c.AdminPassword, err)
	}
	m := upstreams()
	m["HANGARR_ADMIN_PASSWORD"] = " Spaces-Kept-1 "
	if c, err = Load(env(m), files(nil)); err != nil || c.AdminPassword != " Spaces-Kept-1 " {
		t.Fatalf("HANGARR_ADMIN_PASSWORD must be read as is: %q %v", c.AdminPassword, err)
	}
}

func TestTrustedProxies(t *testing.T) {
	m := upstreams()
	m["TRUSTED_PROXIES"] = "10.0.0.0/8, 192.168.1.10 ,fd00::/8"
	c, err := Load(env(m), files(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("192.168.1.10/32"),
		netip.MustParsePrefix("fd00::/8"),
	}
	if len(c.TrustedProxies) != len(want) {
		t.Fatalf("got %v", c.TrustedProxies)
	}
	for i := range want {
		if c.TrustedProxies[i] != want[i] {
			t.Errorf("proxy %d: got %s want %s", i, c.TrustedProxies[i], want[i])
		}
	}
}
