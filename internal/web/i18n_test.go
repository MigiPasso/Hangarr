package web

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"hangarr/internal/config"
	"hangarr/internal/i18n"
	"hangarr/internal/store"
	"hangarr/internal/upstream"
)

var (
	// {{t "key" …}} and (t "key" …) in templates.
	tmplT = regexp.MustCompile(`\bt "((?:[^"\\]|\\.)*)"`)
	// {{tn n "one" "many" …}}
	tmplTN = regexp.MustCompile(`\btn [^"]*?"((?:[^"\\]|\\.)*)" "((?:[^"\\]|\\.)*)"`)
	// {{range pairs "v" "Label" …}}: labels are translated with {{t .L}}.
	tmplPairs = regexp.MustCompile(`pairs((?: "(?:[^"\\]|\\.)*")+)`)
	quoted    = regexp.MustCompile(`"((?:[^"\\]|\\.)*)"`)
	// l.T("key" …) and l.N(n, "one", "many" …) in Go code.
	goT = regexp.MustCompile(`\.T\(\s*"((?:[^"\\]|\\.)*)"`)
	goN = regexp.MustCompile(`\.N\([^"]*?"((?:[^"\\]|\\.)*)",\s*"((?:[^"\\]|\\.)*)"`)
)

func unquote(t *testing.T, s string) string {
	t.Helper()
	v, err := strconv.Unquote(`"` + s + `"`)
	if err != nil {
		t.Fatalf("unquote %q: %v", s, err)
	}
	return v
}

// TestFrenchCatalogComplete checks that every message key of the interface
// has a French translation: templates, Go code, and the label tables.
func TestFrenchCatalogComplete(t *testing.T) {
	keys := map[string]string{} // key -> where
	add := func(k, where string) {
		if k != "" {
			keys[k] = where
		}
	}
	err := fs.WalkDir(templatesFS, "templates", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(templatesFS, p)
		if err != nil {
			return err
		}
		src := string(b)
		for _, m := range tmplT.FindAllStringSubmatch(src, -1) {
			add(unquote(t, m[1]), p)
		}
		for _, m := range tmplTN.FindAllStringSubmatch(src, -1) {
			add(unquote(t, m[1]), p)
			add(unquote(t, m[2]), p)
		}
		for _, m := range tmplPairs.FindAllStringSubmatch(src, -1) {
			args := quoted.FindAllStringSubmatch(m[1], -1)
			for i := 1; i < len(args); i += 2 {
				add(unquote(t, args[i][1]), p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{".", "../upstream", "../syncer"} {
		files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range goT.FindAllStringSubmatch(string(b), -1) {
				add(unquote(t, m[1]), f)
			}
			for _, m := range goN.FindAllStringSubmatch(string(b), -1) {
				add(unquote(t, m[1]), f)
				add(unquote(t, m[2]), f)
			}
		}
	}
	// Keys held in tables and translated where they are displayed.
	for _, v := range stateNames {
		add(v, "stateNames")
	}
	for _, v := range sourceLabels {
		add(v, "sourceLabels")
	}
	for _, v := range authEventLabels {
		add(v, "authEventLabels")
	}
	for _, v := range violationMessages {
		add(v, "violationMessages")
	}
	for _, e := range actionErrors {
		add(e.msg, "actionErrors")
	}
	for _, p := range periods {
		add(p.L, "periods")
	}
	for _, k := range append(append([]string{}, weekdayKeys...), monthKeys...) {
		add(k, "weekday/month keys")
	}
	for _, k := range append(append(weekdayNames[:], releaseTypes[:]...), monthNames[:]...) {
		add(k, "calendar keys")
	}
	for _, f := range upstream.Fields {
		add(f.Label, "upstream.Fields")
		add(f.Help, "upstream.Fields")
	}
	for _, k := range []string{"starting", "saving the catalog", "linking the history", "computing statistics",
		"history: %d / %d", "history: %d", // sync progress
		"not found", "in %s only", "already protected", "protected by a tag", // batch exclusions
		"No item selected.", "Invalid selection.", "No deletable item in the selection.",
		"The number typed does not match the number of items to delete.",
		"Expected an http(s) address, e.g. http://host:port.", "At least 32 characters.",
		"This is the address of a page (/login, /movie/…): keep only the address of the application.",
		"The address changed: type the credential again.",
		"The credential is set by the environment: change this address there too.",
		"Invalid or missing CSRF token. Reload the page.", "Internal error.", "Unknown session.",
		"Deletion unavailable.", "Protection unavailable.",
		"Cannot save: no master key (HANGARR_MASTER_KEY).", "Delete selection", "Protect selection",
		store.NoGenre, store.Unknown} {
		add(k, "Go literals")
	}
	if len(keys) < 300 {
		t.Fatalf("only %d keys found: the extraction is broken", len(keys))
	}
	for k, where := range keys {
		if !i18n.Has(i18n.FR, k) {
			t.Errorf("no French translation for %q (%s)", k, where)
		}
	}
}

func TestLanguageSwitch(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.DefaultLang = "en" })
	c := h.client()
	page := h.do(c, http.MethodGet, "/login", nil, nil)
	if !strings.Contains(page.body, `<html lang="en" class="no-js">`) || !strings.Contains(page.body, ">Sign in</button>") ||
		!strings.Contains(page.body, `href="/lang/fr?next=%2flogin"`) {
		t.Fatal("English by default, with a link to French")
	}
	r := h.do(c, http.MethodGet, "/lang/fr?next=/login", nil, nil)
	if r.status != http.StatusSeeOther || r.header.Get("Location") != "/login" {
		t.Fatalf("switch: %d %s", r.status, r.header.Get("Location"))
	}
	page = h.do(c, http.MethodGet, "/login", nil, nil)
	if !strings.Contains(page.body, `<html lang="fr" class="no-js">`) || !strings.Contains(page.body, ">Se connecter</button>") {
		t.Error("French after the switch (cookie)")
	}
	for _, next := range []string{"//evil.example", "https://evil.example/x", "/\\evil.example", "javascript:alert(1)", ""} {
		r := h.do(c, http.MethodGet, "/lang/en?next="+url.QueryEscape(next), nil, nil)
		if loc := r.header.Get("Location"); loc != "/" {
			t.Errorf("next=%q must not redirect elsewhere: %q", next, loc)
		}
	}
	if r := h.do(c, http.MethodGet, "/lang/de", nil, nil); r.status != http.StatusNotFound {
		t.Errorf("unknown language: %d", r.status)
	}
}

func TestStyleSwitch(t *testing.T) {
	h := newHarness(t, nil)
	c := h.client()
	page := h.do(c, http.MethodGet, "/login", nil, nil)
	if strings.Contains(page.body, "data-style") || strings.Contains(page.body, "y2k.css") {
		t.Fatal("standard style by default, without the Y2K stylesheet")
	}
	r := h.do(c, http.MethodGet, "/style/y2k?next=/login", nil, nil)
	if r.status != http.StatusSeeOther || r.header.Get("Location") != "/login" {
		t.Fatalf("switch: %d %s", r.status, r.header.Get("Location"))
	}
	page = h.do(c, http.MethodGet, "/login", nil, nil)
	if !strings.Contains(page.body, `data-style="y2k"`) || !strings.Contains(page.body, `href="/static/y2k.css?v=`) {
		t.Error("Y2K style after the switch (cookie)")
	}
	if f := h.do(c, http.MethodGet, "/static/fonts/chakra-petch-400.woff2", nil, nil); f.header.Get("Content-Type") != "font/woff2" {
		t.Errorf("font served as %q", f.header.Get("Content-Type"))
	}
	h.do(c, http.MethodGet, "/style/lumen?next=/login", nil, nil)
	if page = h.do(c, http.MethodGet, "/login", nil, nil); !strings.Contains(page.body, `data-style="lumen"`) ||
		!strings.Contains(page.body, `href="/static/lumen.css?v=`) || strings.Contains(page.body, "y2k.css") {
		t.Error("Lumen style: its stylesheet only")
	}
	h.do(c, http.MethodGet, "/palette/ember?next=/login", nil, nil)
	if page = h.do(c, http.MethodGet, "/login", nil, nil); !strings.Contains(page.body, `data-style="lumen" data-palette="ember"`) {
		t.Error("Lumen palette after the switch (cookie)")
	}
	h.do(c, http.MethodGet, "/palette/aurora?next=/login", nil, nil)
	if page = h.do(c, http.MethodGet, "/login", nil, nil); strings.Contains(page.body, "data-palette") {
		t.Error("back to the default palette")
	}
	if r := h.do(c, http.MethodGet, "/palette/bogus", nil, nil); r.status != http.StatusNotFound {
		t.Errorf("unknown palette: %d", r.status)
	}
	h.do(c, http.MethodGet, "/style/standard?next=/login", nil, nil)
	if page = h.do(c, http.MethodGet, "/login", nil, nil); strings.Contains(page.body, "data-style") {
		t.Error("back to the standard style")
	}
	if r := h.do(c, http.MethodGet, "/style/y2k?next="+url.QueryEscape("//evil.example"), nil, nil); r.header.Get("Location") != "/" {
		t.Errorf("next must stay local: %q", r.header.Get("Location"))
	}
	if r := h.do(c, http.MethodGet, "/style/bogus", nil, nil); r.status != http.StatusNotFound {
		t.Errorf("unknown style: %d", r.status)
	}
}

// With COOKIE_SECURE, the choices are written as __Host- cookies: they must
// be read back under that name and win over a cookie from before the prefixes.
func TestPreferencesWithCookiePrefix(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.CookieSecure = true })
	choose := func(path string) *http.Cookie {
		w, req := httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil)
		req.Host = "localhost"
		h.srv.Handler().ServeHTTP(w, req)
		cookies := w.Result().Cookies()
		if len(cookies) != 1 || !strings.HasPrefix(cookies[0].Name, hostPrefix) {
			t.Fatalf("%s: %v", path, cookies)
		}
		return cookies[0]
	}
	r := httptest.NewRequest(http.MethodGet, "/login", nil)
	r.AddCookie(&http.Cookie{Name: styleCookie, Value: "lumen"})
	r.AddCookie(&http.Cookie{Name: langCookie, Value: "en"})
	for _, p := range []string{"/style/y2k", "/palette/ember", "/lang/fr"} {
		r.AddCookie(choose(p))
	}
	if got := h.srv.styleFrom(r); got != "y2k" {
		t.Errorf("style: %q", got)
	}
	if got := h.srv.paletteFrom(r); got != "ember" {
		t.Errorf("palette: %q", got)
	}
	if got := h.srv.loc(r).Lang; got != i18n.FR {
		t.Errorf("lang: %q", got)
	}
}
