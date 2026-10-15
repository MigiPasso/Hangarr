// Package matching pairs media server items (Plex or Jellyfin) with
// Sonarr/Radarr items and resolves history rows to media by title
// when rating keys are unknown.
package matching

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Media kinds.
const (
	KindMovie  = "movie"
	KindSeries = "series"
)

// Match methods.
const (
	MethodTVDB  = "tvdb"
	MethodTMDB  = "tmdb"
	MethodIMDB  = "imdb"
	MethodFile  = "file"
	MethodFuzzy = "fuzzy"
)

var (
	trailingYear = regexp.MustCompile(`\s*\(\d{4}\)\s*$`)
	articles     = map[string]bool{
		"the": true, "a": true, "an": true,
		"le": true, "la": true, "les": true, "l": true, "un": true, "une": true, "des": true,
	}
)

// NormalizeTitle lowercases, removes accents, punctuation, a trailing
// "(YYYY)" and a leading article: "The Office (US)" and "office us" match.
func NormalizeTitle(s string) string {
	s = trailingYear.ReplaceAllString(s, "")
	var b strings.Builder
	for _, r := range norm.NFD.String(s) {
		switch {
		case unicode.Is(unicode.Mn, r):
			// combining accent: dropped
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(unicode.ToLower(r))
		case r == '&':
			b.WriteString(" and ")
		default:
			b.WriteRune(' ')
		}
	}
	words := strings.Fields(b.String())
	if len(words) > 1 && articles[words[0]] {
		words = words[1:]
	}
	return strings.Join(words, " ")
}

// ArrItem is a Sonarr series or Radarr movie already stored as media.
type ArrItem struct {
	MediaID int64
	Kind    string
	TvdbID  int64
	TmdbID  int64
	ImdbID  string
	Title   string
	Year    int
	// Other titles (original, other languages) for the title match.
	AltTitles []string
	// Folder (series or movie directory) and movie file, any path form.
	Folder string
	File   string
}

// ServerItem is a movie or show of the media server (Plex or Jellyfin).
type ServerItem struct {
	Kind    string
	Key     string
	Title   string
	Year    int
	TvdbID  int64
	TmdbID  int64
	ImdbID  string
	AddedAt int64
	// OriginalTitle is used for the title match too.
	OriginalTitle string
	// Folders (show location, movie file directory) and movie files.
	Folders []string
	Files   []string
}

// baseName returns the last element of a Unix or Windows path, lowercased:
// The media server and the *arr apps may see the same file under different mounts.
func baseName(p string) string {
	p = strings.TrimRight(p, `/\`)
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		p = p[i+1:]
	}
	return strings.ToLower(strings.TrimSpace(p))
}

// dirName returns the base name of the directory holding a file.
func dirName(p string) string {
	p = strings.TrimRight(p, `/\`)
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return baseName(p[:i])
	}
	return ""
}

// Link pairs a media row with a media server key.
type Link struct {
	MediaID int64
	Key     string
	Method  string
}

type idKey struct {
	kind, scheme, value string
}

func idKeys(kind string, tvdb, tmdb int64, imdb string) []idKey {
	var keys []idKey
	itoa := func(n int64) string { return strconv.FormatInt(n, 10) }
	// Series: tvdb → tmdb → imdb; movies: tmdb → imdb.
	if kind == KindSeries && tvdb > 0 {
		keys = append(keys, idKey{kind, MethodTVDB, itoa(tvdb)})
	}
	if tmdb > 0 {
		keys = append(keys, idKey{kind, MethodTMDB, itoa(tmdb)})
	}
	if imdb != "" {
		keys = append(keys, idKey{kind, MethodIMDB, imdb})
	}
	return keys
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// yearsCompatible accepts ±1 year, or an unknown year on either side.
func yearsCompatible(a, b int) bool {
	return a == 0 || b == 0 || abs(a-b) <= 1
}

// Match pairs arr items with server items. Exact external ids are tried first
// for every item, so a weaker match can never steal an item that has an id
// match; then the same file or folder on disk; then normalized title (or
// original / alternate title) and year ±1.
// It returns the links and the server items left unmatched (server_only).
func Match(arr []ArrItem, plex []ServerItem) ([]Link, []ServerItem) {
	byID := map[idKey]int{}
	for i, p := range plex {
		for _, k := range idKeys(p.Kind, p.TvdbID, p.TmdbID, p.ImdbID) {
			if _, dup := byID[k]; !dup {
				byID[k] = i
			}
		}
	}
	used := make([]bool, len(plex))
	matched := make([]bool, len(arr))
	var links []Link

	for ai, a := range arr {
		for _, k := range idKeys(a.Kind, a.TvdbID, a.TmdbID, a.ImdbID) {
			if pi, ok := byID[k]; ok && !used[pi] {
				used[pi], matched[ai] = true, true
				links = append(links, Link{MediaID: a.MediaID, Key: plex[pi].Key, Method: k.scheme})
				break
			}
		}
	}

	// Same file or folder on disk: works whatever the metadata says (no ids
	// on the server side, titles in another language, wrong year). A name
	// shared by several items on either side is ignored.
	byFile, byFolder := map[string]int{}, map[string]int{}
	index := func(m map[string]int, key string, i int) {
		if prev, ok := m[key]; ok && prev != i {
			m[key] = -1
		} else if !ok {
			m[key] = i
		}
	}
	for i, p := range plex {
		if used[i] {
			continue
		}
		for _, f := range p.Files {
			if n := baseName(f); n != "" {
				index(byFile, p.Kind+"|"+n, i)
			}
			if d := dirName(f); d != "" {
				index(byFolder, p.Kind+"|"+d, i)
			}
		}
		for _, d := range p.Folders {
			if n := baseName(d); n != "" {
				index(byFolder, p.Kind+"|"+n, i)
			}
		}
	}
	arrFolders := map[string]int{}
	for _, a := range arr {
		if n := baseName(a.Folder); n != "" {
			arrFolders[a.Kind+"|"+n]++
		}
	}
	for ai, a := range arr {
		if matched[ai] {
			continue
		}
		pi := -1
		if n := baseName(a.File); n != "" {
			if i, ok := byFile[a.Kind+"|"+n]; ok {
				pi = i
			}
		}
		if n := baseName(a.Folder); pi < 0 && n != "" && arrFolders[a.Kind+"|"+n] == 1 {
			if i, ok := byFolder[a.Kind+"|"+n]; ok {
				pi = i
			}
		}
		if pi >= 0 && !used[pi] {
			used[pi], matched[ai] = true, true
			links = append(links, Link{MediaID: a.MediaID, Key: plex[pi].Key, Method: MethodFile})
		}
	}

	// Title and year ±1, with the original and alternate titles too.
	byTitle := map[string][]int{}
	for i, p := range plex {
		if used[i] {
			continue
		}
		seen := map[string]bool{}
		for _, t := range []string{p.Title, p.OriginalTitle} {
			if n := NormalizeTitle(t); n != "" && !seen[n] {
				seen[n] = true
				byTitle[p.Kind+"|"+n] = append(byTitle[p.Kind+"|"+n], i)
			}
		}
	}
	for ai, a := range arr {
		if matched[ai] {
			continue
		}
		best, bestKey, bestDiff := -1, "", 1<<30
		for _, title := range append([]string{a.Title}, a.AltTitles...) {
			for _, pi := range byTitle[a.Kind+"|"+NormalizeTitle(title)] {
				p := plex[pi]
				if used[pi] || !yearsCompatible(a.Year, p.Year) {
					continue
				}
				diff := 0
				if a.Year > 0 && p.Year > 0 {
					diff = abs(a.Year - p.Year)
				}
				if diff < bestDiff {
					best, bestKey, bestDiff = pi, p.Key, diff
				}
			}
			if best >= 0 {
				break // the main title wins over alternate ones
			}
		}
		if best >= 0 {
			used[best] = true
			links = append(links, Link{MediaID: a.MediaID, Key: bestKey, Method: MethodFuzzy})
		}
	}

	var plexOnly []ServerItem
	for i, p := range plex {
		if !used[i] {
			plexOnly = append(plexOnly, p)
		}
	}
	return links, plexOnly
}

// TitleCandidate is a media row that history rows can fall back to.
type TitleCandidate struct {
	MediaID int64
	Kind    string
	Title   string
	Year    int
}

// TitleIndex resolves history titles to media when rating keys changed.
type TitleIndex struct {
	byTitle map[string][]TitleCandidate
}

// NewTitleIndex indexes candidates by kind and normalized title.
func NewTitleIndex(cands []TitleCandidate) *TitleIndex {
	idx := &TitleIndex{byTitle: map[string][]TitleCandidate{}}
	for _, c := range cands {
		k := c.Kind + "|" + NormalizeTitle(c.Title)
		idx.byTitle[k] = append(idx.byTitle[k], c)
	}
	return idx
}

// Find returns the media id for a title and optional year. With a year,
// the closest candidate within ±1 year wins; without one, the title must be
// unambiguous. Zero means no match.
func (idx *TitleIndex) Find(kind, title string, year int) int64 {
	cands := idx.byTitle[kind+"|"+NormalizeTitle(title)]
	if year == 0 {
		if len(cands) == 1 {
			return cands[0].MediaID
		}
		return 0
	}
	var best int64
	bestDiff := 2
	for _, c := range cands {
		if c.Year == 0 {
			if len(cands) == 1 {
				return c.MediaID
			}
			continue
		}
		if d := abs(c.Year - year); d < bestDiff {
			best, bestDiff = c.MediaID, d
		}
	}
	return best
}
