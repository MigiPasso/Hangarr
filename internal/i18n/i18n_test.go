package i18n

import "testing"

func TestHours(t *testing.T) {
	for secs, want := range map[int64]string{
		0: "0m", 29: "0m", 45 * 60: "45m", 3600: "1h", 2*3600 + 20*60: "2h20m",
		2*3600 + 5*60: "2h05m", 36 * 3600: "36h", 3600 + 59*60 + 45: "2h",
	} {
		if got := For(FR).Hours(secs); got != want {
			t.Errorf("Hours(%d) = %q, want %q", secs, got, want)
		}
	}
}
