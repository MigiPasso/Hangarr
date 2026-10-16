package media

import (
	"testing"
	"unicode/utf8"
)

// BUG-08: numbers of three digits or more were cut digit by digit.
func TestEpisodeCode(t *testing.T) {
	for _, tc := range []struct {
		s, e int
		want string
	}{
		{1, 2, "S01E02"}, {0, 0, "S00E00"}, {1, 105, "S01E105"}, {12, 1000, "S12E1000"}, {2024, 7, "S2024E07"},
	} {
		if got := EpisodeCode(tc.s, tc.e); got != tc.want {
			t.Errorf("EpisodeCode(%d, %d) = %q, want %q", tc.s, tc.e, got, tc.want)
		}
	}
}

func TestPaths(t *testing.T) {
	for _, tc := range []struct{ in, base, dir string }{
		{"/m/Dune (2021)/Dune.mkv", "Dune.mkv", "/m/Dune (2021)"},
		{`D:\Films\Dune\Dune.mkv`, "Dune.mkv", "D:/Films/Dune"},
		{"Dune.mkv", "Dune.mkv", ""},
	} {
		if b, d := Base(tc.in), Dir(tc.in); b != tc.base || d != tc.dir {
			t.Errorf("%q: base %q dir %q", tc.in, b, d)
		}
	}
}

// BUG-12: truncation never leaves invalid UTF-8.
func TestTruncate(t *testing.T) {
	for _, tc := range []struct {
		in       string
		n        int
		cut, ell string
	}{
		{"abc", 5, "abc", "abc"},
		{"abcdef", 3, "abc", "abc…"},
		{"ééééé", 2, "éé", "éé…"},
		{"日本語テキスト", 3, "日本語", "日本語…"},
		{"🎬🎬", 1, "🎬", "🎬…"},
	} {
		if got := Truncate(tc.in, tc.n); got != tc.cut || !utf8.ValidString(got) {
			t.Errorf("Truncate(%q, %d) = %q", tc.in, tc.n, got)
		}
		if got := Ellipsis(tc.in, tc.n); got != tc.ell {
			t.Errorf("Ellipsis(%q, %d) = %q", tc.in, tc.n, got)
		}
	}
}

func FuzzTruncate(f *testing.F) {
	for _, s := range []string{"", "abc", "ééé", "日本語", "\xff\xfe"} {
		f.Add(s, 2)
	}
	f.Fuzz(func(t *testing.T, s string, n int) {
		if n < 0 {
			n = -n
		}
		got := Truncate(s, n)
		if utf8.ValidString(s) && !utf8.ValidString(got) {
			t.Fatalf("Truncate(%q, %d) = %q: invalid UTF-8", s, n, got)
		}
		if len(got) > len(s) || got != s[:len(got)] {
			t.Fatalf("Truncate(%q, %d) = %q: not a prefix", s, n, got)
		}
	})
}
