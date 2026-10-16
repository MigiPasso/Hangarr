package auth

import (
	"strings"
	"testing"
)

func FuzzParsePHC(f *testing.F) {
	f.Add("$argon2id$v=19$m=65536,t=3,p=2$c2FsdHNhbHQ$aGFzaGhhc2hoYXNo")
	f.Add("$argon2id$v=19$m=0,t=0,p=0$$")
	f.Add("$bcrypt$x")
	f.Fuzz(func(t *testing.T, s string) {
		p, salt, key, err := parsePHC(s)
		if err != nil {
			return
		}
		if p.Memory == 0 || p.Time == 0 || p.Threads == 0 || len(salt) == 0 || len(key) == 0 || !strings.HasPrefix(s, "$argon2id$") {
			t.Fatalf("accepted %q: %+v salt=%d key=%d", s, p, len(salt), len(key))
		}
	})
}

func FuzzClientKey(f *testing.F) {
	for _, s := range []string{"192.0.2.1", "2001:db8::1", "[::1]:80", "x", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if k := ClientKey(s); ClientKey(k) != k && !strings.Contains(k, "/") {
			t.Fatalf("ClientKey not stable: %q → %q → %q", s, k, ClientKey(k))
		}
	})
}
