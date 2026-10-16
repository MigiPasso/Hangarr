package web

import (
	"net/url"
	"strings"
	"testing"
)

// localPath never returns an address of another site (open redirect).
func FuzzLocalPath(f *testing.F) {
	for _, s := range []string{"/series?x=1", "//evil.com", "/\\evil.com", "https://evil.com", "/%2F%2Fevil", "", "/\t/evil"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got := localPath(s)
		u, err := url.Parse(got)
		if err != nil || !strings.HasPrefix(got, "/") || strings.HasPrefix(got, "//") || strings.HasPrefix(got, "/\\") ||
			u.Host != "" || u.Scheme != "" {
			t.Fatalf("localPath(%q) = %q", s, got)
		}
	})
}

func TestLocalPath(t *testing.T) {
	for in, want := range map[string]string{
		"/series?x=1":      "/series?x=1",
		"//evil.com":       "/",
		"/\\evil.com":      "/",
		"https://evil.com": "/",
		"/%2Fevil.com/ x":  "/", // re-encoded as //evil.com/%20x (found by FuzzLocalPath)
		"/%2Fevil.com":     "/%2Fevil.com",
		"":                 "/",
	} {
		if got := localPath(in); got != want {
			t.Errorf("localPath(%q) = %q, want %q", in, got, want)
		}
	}
}
