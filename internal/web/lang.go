package web

import (
	"net/http"
	"net/url"
	"strings"

	"cleanarr/internal/i18n"
)

// langCookie remembers the interface language chosen with the EN / FR link.
const langCookie = "cleanarr_lang"

// loc returns the localizer of the request: the language cookie, otherwise
// DEFAULT_LANG (English by default).
func (s *Server) loc(r *http.Request) i18n.Localizer {
	if c, err := r.Cookie(langCookie); err == nil {
		if l, ok := i18n.Parse(c.Value); ok {
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
	return u.RequestURI()
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
