package secretbox

import "testing"

func FuzzParseKey(f *testing.F) {
	k, _ := NewKey()
	for _, s := range []string{k, "", "zz", "0000000000000000000000000000000000000000000000000000000000000000"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		key, err := ParseKey(s)
		if err == nil && len(key) != KeySize {
			t.Fatalf("ParseKey(%q): %d bytes", s, len(key))
		}
	})
}
