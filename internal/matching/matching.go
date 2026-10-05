// Package matching pairs Plex items with Sonarr/Radarr items (SPEC §4) and
// resolves history rows to media by title when rating keys are unknown.
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
}

// PlexItem is a Plex movie or show.
type PlexItem struct {
	Kind      string
	RatingKey string
	Title     string
	Year      int
	TvdbID    int64
	TmdbID    int64
	ImdbID    string
	AddedAt   int64
}

// Link pairs a media row with a Plex rating key.
type Link struct {
	MediaID   int64
	RatingKey string
	Method    string
}

type idKey struct {
	kind, scheme, value string
}

func idKeys(kind string, tvdb, tmdb int64, imdb string) []idKey {
	var keys []idKey
	itoa := func(n int64) string { return strconv.FormatInt(n, 10) }
	// Series: tvdb → tmdb → imdb; movies: tmdb → imdb (SPEC §4).
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

// Match pairs arr items with Plex items. Exact external ids are tried first
// for every item, so a fuzzy match can never steal an item that has an id
// match; remaining items are paired by normalized title and year ±1.
// It returns the links and the Plex items left unmatched (plex_only).
func Match(arr []ArrItem, plex []PlexItem) ([]Link, []PlexItem) {
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
				links = append(links, Link{MediaID: a.MediaID, RatingKey: plex[pi].RatingKey, Method: k.scheme})
				break
			}
		}
	}

	byTitle := map[string][]int{}
	for i, p := range plex {
		if !used[i] {
			key := p.Kind + "|" + NormalizeTitle(p.Title)
			byTitle[key] = append(byTitle[key], i)
		}
	}
	for ai, a := range arr {
		if matched[ai] {
			continue
		}
		best, bestKey, bestDiff := -1, "", 1<<30
		for _, pi := range byTitle[a.Kind+"|"+NormalizeTitle(a.Title)] {
			p := plex[pi]
			if used[pi] || !yearsCompatible(a.Year, p.Year) {
				continue
			}
			diff := 0
			if a.Year > 0 && p.Year > 0 {
				diff = abs(a.Year - p.Year)
			}
			if diff < bestDiff {
				best, bestKey, bestDiff = pi, p.RatingKey, diff
			}
		}
		if best >= 0 {
			used[best] = true
			links = append(links, Link{MediaID: a.MediaID, RatingKey: bestKey, Method: MethodFuzzy})
		}
	}

	var plexOnly []PlexItem
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
