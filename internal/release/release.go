// Package release reads quality hints from a video file name.
package release

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Source is where a release comes from, best first.
type Source string

const (
	Unknown Source = ""
	Remux   Source = "Remux"
	BluRay  Source = "BluRay"
	WEBDL   Source = "WEB-DL"
	WEBRip  Source = "WEBRip"
	WEB     Source = "WEB" // "WEB" alone: WEB-DL or WEBRip, not said
	HDTV    Source = "HDTV"
	DVD     Source = "DVD"
)

// Info is what a file name says about a release.
type Info struct {
	Source Source
	Light  bool // re-encoded "light" release (4KLight, HDLight, mHD…)
}

// sourcePriority lists the sources from best to worst.
var sourcePriority = []Source{Remux, BluRay, WEBDL, WEBRip, WEB, HDTV, DVD}

// tokenSources maps one token to its source.
var tokenSources = []struct {
	token  string
	source Source
}{
	{"Remux", Remux},
	{"BDRemux", Remux},
	{"BluRay", BluRay},
	{"BDRip", BluRay},
	{"BRRip", BluRay},
	{"WEBDL", WEBDL},
	{"WEBRip", WEBRip},
	{"WEB", WEB},
	{"HDTV", HDTV},
	{"PDTV", HDTV},
	{"DSR", HDTV},
	{"DVD", DVD},
	{"DVDRip", DVD},
	{"DVDR", DVD},
	{"DVD9", DVD},
	{"DVD5", DVD},
}

// pairSources maps two consecutive tokens to their source.
var pairSources = []struct {
	first, second string
	source        Source
}{
	{"WEB", "DL", WEBDL},
	{"WEB", "Rip", WEBRip},
	{"Blu", "Ray", BluRay},
	{"DVD", "Rip", DVD},
}

// lightTokens mark re-encoded "light" releases.
var lightTokens = []string{"4KLight", "HDLight", "Light", "mHD", "miniHD", "1080pLight", "720pLight", "2160pLight"}

// Parse reads a file name or a path (only the base name is used).
func Parse(name string) Info {
	tokens := tokenize(stripExt(baseName(name)))
	return Info{Source: bestSource(tokens), Light: hasLight(tokens)}
}

// baseName keeps what follows the last "/" or "\".
func baseName(name string) string {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		return name[i+1:]
	}
	return name
}

// stripExt removes a last ".ext" made of 2 to 4 letters or digits.
func stripExt(name string) string {
	i := strings.LastIndexByte(name, '.')
	if i < 0 {
		return name
	}
	ext := name[i+1:]
	if n := utf8.RuneCountInString(ext); n < 2 || n > 4 {
		return name
	}
	for _, r := range ext {
		if !isAlnum(r) {
			return name
		}
	}
	return name[:i]
}

// tokenize splits on every character that is not a letter or a digit.
func tokenize(name string) []string {
	return strings.FieldsFunc(name, func(r rune) bool { return !isAlnum(r) })
}

func isAlnum(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

// bestSource returns the highest-priority source named by the tokens.
func bestSource(tokens []string) Source {
	found := map[Source]bool{}
	for i := 0; i < len(tokens); i++ {
		if i+1 < len(tokens) {
			if src := pairSource(tokens[i], tokens[i+1]); src != Unknown {
				found[src] = true
				i++ // both tokens are used
				continue
			}
		}
		if src := tokenSource(tokens[i]); src != Unknown {
			found[src] = true
		}
	}
	for _, src := range sourcePriority {
		if found[src] {
			return src
		}
	}
	return Unknown
}

func tokenSource(tok string) Source {
	for _, e := range tokenSources {
		if strings.EqualFold(tok, e.token) {
			return e.source
		}
	}
	return Unknown
}

func pairSource(a, b string) Source {
	for _, e := range pairSources {
		if strings.EqualFold(a, e.first) && strings.EqualFold(b, e.second) {
			return e.source
		}
	}
	return Unknown
}

func hasLight(tokens []string) bool {
	for _, tok := range tokens {
		for _, l := range lightTokens {
			if strings.EqualFold(tok, l) {
				return true
			}
		}
	}
	return false
}
