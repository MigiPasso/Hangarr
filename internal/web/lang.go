package web

import (
	"net/http"
	"net/url"
	"slices"
	"strings"

	"hangarr/internal/i18n"
)

// langCookie remembers the interface language chosen with the EN / FR link.
const langCookie = "hangarr_lang"

// loc returns the localizer of the request: the language cookie, otherwise
// DEFAULT_LANG (English by default).
func (s *Server) loc(r *http.Request) i18n.Localizer {
	if v, ok := s.readCookie(r, langCookie); ok {
		if l, ok := i18n.Parse(v); ok {
			return i18n.For(l)
		}
	}
	if l, ok := i18n.Parse(s.Config.DefaultLang); ok {
		return i18n.For(l)
	}
	return i18n.For(i18n.EN)
}

// localPath accepts only a path of this site ("/series?x=1"), never an
// absolute or scheme-relative URL: the language link is not an open redirect.
func localPath(next string) string {
	u, err := url.Parse(next)
	if err != nil || next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") ||
		strings.HasPrefix(next, "/\\") || u.Scheme != "" || u.Host != "" {
		return "/"
	}
	// The result is checked too: re-encoding may turn "/%2Fevil.com/ x" into
	// "//evil.com/%20x", another site (found by FuzzLocalPath).
	out := u.RequestURI()
	if !strings.HasPrefix(out, "/") || strings.HasPrefix(out, "//") || strings.HasPrefix(out, "/\\") {
		return "/"
	}
	return out
}

// styleCookie remembers the interface style chosen in Settings.
const styleCookie = "hangarr_style"

// styles are the optional interface styles (the standard one has no name).
var styles = []string{"catppuccin", "lumen", "y2k"}

func (s *Server) styleFrom(r *http.Request) string {
	if v, ok := s.readCookie(r, styleCookie); ok && slices.Contains(styles, v) {
		return v
	}
	return ""
}

// paletteCookie remembers the color palette of the Lumen style.
const paletteCookie = "hangarr_palette"

// palettes are the optional Lumen palettes (the default, Aurora, has no name).
var palettes = []string{"ocean", "ember", "jade"}

func (s *Server) paletteFrom(r *http.Request) string {
	if v, ok := s.readCookie(r, paletteCookie); ok && slices.Contains(palettes, v) {
		return v
	}
	return ""
}

// setPalette (GET /palette/{name}?next=/path) stores the Lumen palette
// ("aurora" removes it) and goes back.
func (s *Server) setPalette(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	switch {
	case name == "aurora":
		s.clearCookie(w, paletteCookie)
	case slices.Contains(palettes, name):
		s.setCookie(w, paletteCookie, name, "/", 365*24*60*60)
	default:
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, localPath(r.URL.Query().Get("next")), http.StatusSeeOther) //nolint:gosec // G710: sanitized by localPath
}

// setStyle (GET /style/{name}?next=/path) stores the interface style
// ("standard" removes it) and goes back.
func (s *Server) setStyle(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	switch {
	case name == "standard":
		s.clearCookie(w, styleCookie)
	case slices.Contains(styles, name):
		s.setCookie(w, styleCookie, name, "/", 365*24*60*60)
	default:
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, localPath(r.URL.Query().Get("next")), http.StatusSeeOther) //nolint:gosec // G710: sanitized by localPath
}

// setLang (GET /lang/{code}?next=/path) stores the language and goes back.
func (s *Server) setLang(w http.ResponseWriter, r *http.Request) {
	l, ok := i18n.Parse(r.PathValue("code"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.setCookie(w, langCookie, string(l), "/", 365*24*60*60)
	// localPath keeps only a path of this site (TestLanguageSwitch).
	http.Redirect(w, r, localPath(r.URL.Query().Get("next")), http.StatusSeeOther) //nolint:gosec // G710: sanitized by localPath
}
