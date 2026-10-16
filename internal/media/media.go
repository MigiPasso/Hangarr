// Package media holds the small helpers shared by the store, the actions and
// the web layer: episode codes, file paths of the media servers, and text
// truncation that keeps UTF-8 valid. One rule, one place: duplicated copies
// end up disagreeing.
package media

import (
	"fmt"
	"path"
	"strings"
	"unicode/utf8"
)

// EpisodeCode is the S01E02 code of an episode; numbers of three digits or
// more are kept whole (S01E105).
func EpisodeCode(season, episode int) string {
	return fmt.Sprintf("S%02dE%02d", season, episode)
}

// Base is the file name of a path written with / or \ (Windows servers).
func Base(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

// Dir is the folder of a path written with / or \ ("" when there is none).
func Dir(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return path.Clean(strings.ReplaceAll(p[:i], `\`, "/"))
	}
	return ""
}

// Truncate keeps the first n characters (runes) of s: never cut inside a
// UTF-8 sequence.
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i, count := 0, 0
	for i < len(s) && count < n {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
		count++
	}
	return s[:i]
}

// Ellipsis is Truncate with "…" when something was cut.
func Ellipsis(s string, n int) string {
	if t := Truncate(s, n); len(t) < len(s) {
		return t + "…"
	}
	return s
}
