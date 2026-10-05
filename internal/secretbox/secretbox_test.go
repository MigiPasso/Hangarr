package secretbox

import (
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newBox(t *testing.T, fill byte) *Box {
	t.Helper()
	b, err := New([]byte(strings.Repeat(string(fill), KeySize)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSealOpen(t *testing.T) {
	b := newBox(t, 'k')
	s1, err := b.Seal("sonarr.api_key", "secret-value")
	if err != nil {
		t.Fatal(err)
	}
	s2, _ := b.Seal("sonarr.api_key", "secret-value")
	if s1 == s2 {
		t.Error("two seals of the same value must differ (random nonce)")
	}
	if strings.Contains(s1, "secret-value") {
		t.Error("sealed value leaks the plaintext")
	}
	got, err := b.Open("sonarr.api_key", s1)
	if err != nil || got != "secret-value" {
		t.Fatalf("Open = %q, %v", got, err)
	}
}

func TestOpenFailures(t *testing.T) {
	b := newBox(t, 'k')
	sealed, _ := b.Seal("sonarr.api_key", "v")
	cases := map[string]struct {
		box    *Box
		name   string
		sealed string
	}{
		"other key":    {newBox(t, 'x'), "sonarr.api_key", sealed},
		"other name":   {b, "radarr.api_key", sealed},
		"tampered":     {b, "sonarr.api_key", sealed[:len(sealed)-2] + "AA"},
		"no prefix":    {b, "sonarr.api_key", strings.TrimPrefix(sealed, "v1:")},
		"not base64":   {b, "sonarr.api_key", "v1:!!!"},
		"too short":    {b, "sonarr.api_key", "v1:AAAA"},
		"empty string": {b, "sonarr.api_key", ""},
	}
	for name, c := range cases {
		if _, err := c.box.Open(c.name, c.sealed); !errors.Is(err, ErrUndecryptable) {
			t.Errorf("%s: err = %v, want ErrUndecryptable", name, err)
		}
	}
}

func TestParseKey(t *testing.T) {
	raw := []byte(strings.Repeat("a", KeySize))
	k, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	if b, err := ParseKey(k); err != nil || len(b) != KeySize {
		t.Fatalf("ParseKey(NewKey()) = %d bytes, %v", len(b), err)
	}
	if b, err := ParseKey(" " + hex.EncodeToString(raw) + "\n"); err != nil || string(b) != string(raw) {
		t.Fatalf("hex key: %v", err)
	}
	for _, bad := range []string{"", "short", hex.EncodeToString(raw[:16]), "QUJD"} {
		if _, err := ParseKey(bad); err == nil {
			t.Errorf("ParseKey(%q) should fail", bad)
		}
	}
}

func TestLoadOrCreateKeyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.key")
	k1, created, err := LoadOrCreateKeyFile(path)
	if err != nil || !created || len(k1) != KeySize {
		t.Fatalf("create: %d bytes, created=%v, %v", len(k1), created, err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode: %v %v", fi.Mode(), err)
	}
	k2, created, err := LoadOrCreateKeyFile(path)
	if err != nil || created || string(k2) != string(k1) {
		t.Fatalf("reload must return the same key (created=%v, %v)", created, err)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("temporary file left behind: %v", entries)
	}
	if err := os.WriteFile(path, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadOrCreateKeyFile(path); err == nil {
		t.Error("a corrupt key file must not be silently replaced")
	}
}
