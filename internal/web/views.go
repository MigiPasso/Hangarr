package web

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"hangarr/internal/i18n"
	"hangarr/internal/store"
)

//go:embed templates
var templatesFS embed.FS

//go:embed static
var staticFS embed.FS

// pages lists every page template under templates/pages.
var pages = []string{"login", "password", "account", "home", "notfound", "library", "settings", "detail", "delete", "journal",
	"stats", "userstats", "cleanup", "config", "health", "batch", "calendar"}

type views struct {
	pages  map[i18n.Lang]map[string]*template.Template
	assets map[string]string // file name -> content hash, for cache busting
}

func newViews(loc *time.Location, protectedTags []string, abandonDays int) (*views, error) {
	v := &views{pages: map[i18n.Lang]map[string]*template.Template{}, assets: map[string]string{}}
	err := fs.WalkDir(staticFS, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := staticFS.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		v.assets[strings.TrimPrefix(p, "static/")] = hex.EncodeToString(sum[:4])
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, lang := range i18n.Langs {
		funcs := v.funcs(i18n.For(lang), loc, protectedTags, abandonDays)
		v.pages[lang] = map[string]*template.Template{}
		for _, p := range pages {
			t, err := template.New("").Funcs(funcs).ParseFS(templatesFS,
				"templates/layout.html", "templates/partials/*.html", "templates/pages/"+p+".html")
			if err != nil {
				return nil, fmt.Errorf("parse page %s: %w", p, err)
			}
			v.pages[lang][p] = t
		}
	}
	return v, nil
}

// toInt64 accepts the integer types templates pass around.
func toInt64(n any) int64 {
	switch v := n.(type) {
	case int:
		return int64(v)
	case int64:
		return v
	case int32:
		return int64(v)
	}
	return 0
}

// funcs are the template functions of one language.
func (v *views) funcs(l i18n.Localizer, loc *time.Location, protectedTags []string, abandonDays int) template.FuncMap {
	at := func(unix int64) time.Time { return time.Unix(unix, 0).In(loc) }
	return template.FuncMap{
		"asset": func(name string) (string, error) {
			h, ok := v.assets[name]
			if !ok {
				return "", fmt.Errorf("unknown asset %q", name)
			}
			return "/static/" + name + "?v=" + h, nil
		},
		"lang": func() string { return string(l.Lang) },
		"t":    l.T,
		// tv translates a key with a slice of arguments (sync progress).
		"tv": func(key string, args []any) string { return l.T(key, args...) },
		// tn picks the singular or plural form: tn .Count "%d item" "%d items".
		"tn":       func(n any, one, many string, args ...any) string { return l.N(toInt64(n), one, many, args...) },
		"datetime": func(unix int64) string { return l.DateTime(at(unix)) },
		"reltime":  func(unix int64) string { return l.Relative(time.Now(), time.Unix(unix, 0)) },
		"browser":  func(ua string) string { return browserSummary(l, ua) },
		"relptr": func(unix *int64) string {
			if unix == nil {
				return ""
			}
			return l.Relative(time.Now(), time.Unix(*unix, 0))
		},
		"dateptr": func(unix *int64) string {
			if unix == nil {
				return ""
			}
			return l.Date(at(*unix))
		},
		"bytes":      l.Bytes,
		"stateLabel": func(it store.LibraryItem) string { return stateLabel(l, it) },
		"protected":  func(tags []string) bool { return isProtected(tags, protectedTags) },
		"has":        func(list []string, v string) bool { return slices.Contains(list, v) },
		"hasID":      func(list []int64, v int64) bool { return slices.Contains(list, v) },
		"list":       func(v ...string) []string { return v },
		"join":       strings.Join,
		"pairs":      pairs,
		"mediaPath":  kindPath,
		"imgURL":     imgURL,
		"hours":      l.Hours,
		"share":      l.PctShort,
		"pctOf": func(a, b int64) string {
			if b <= 0 {
				return l.PctShort(0)
			}
			return l.PctShort(float64(a) / float64(b))
		},
		"num":         l.Num,
		"dec":         func(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) },
		"list2":       func(v ...any) []any { return v },
		"inc":         func(i int) int { return i + 1 },
		"abandonDays": func() int { return abandonDays },
		"minutesOfHour": func() []int {
			m := make([]int, 60)
			for i := range m {
				m[i] = i
			}
			return m
		},
		"hoursOfDay": func() []int {
			h := make([]int, 24)
			for i := range h {
				h[i] = i
			}
			return h
		},
		"stateName": func(s string) string { return l.T(stateNames[s]) },
		"episode": func(season, episode *int) string {
			if season == nil || episode == nil {
				return ""
			}
			return episodeLabel(*season, *episode)
		},
		"duration": l.Duration,
		"sourceLabel": func(src string) string {
			if name, ok := sourceLabels[src]; ok {
				return l.T(name)
			}
			return src
		},
		"runDuration": func(r store.SyncRun) string {
			if r.FinishedAt == nil {
				return l.T("running")
			}
			return l.Duration(*r.FinishedAt - r.StartedAt)
		},
		"transcode": func(v string) string { return transcodeLabel(l, v) },
		"authLabel": func(e string) string {
			if name, ok := authEventLabels[e]; ok {
				return l.T(name)
			}
			return e
		},
		// explain turns an actions error into a sentence (batch report).
		"explain": func(err error) string { return explain(l, err, "Failed: %s") },
		// label translates the placeholder labels of the store ("(none)").
		"label": func(v string) string {
			if v == store.NoGenre || v == store.Unknown {
				return l.T(v)
			}
			return v
		},
		"calendarURL": calendarListURL,
		"deref": func(p *int) int {
			if p == nil {
				return 0
			}
			return *p
		},
	}
}

// stateNames are the watch states (translated by the templates).
var stateNames = map[string]string{"never": "Never watched", "in_progress": "In progress", "completed": "Completed",
	"abandoned": "Abandoned"}

// sourceLabels name the sync sources.
var sourceLabels = map[string]string{"sonarr": "Sonarr", "radarr": "Radarr", "plex": "Plex (catalog)",
	"tautulli": "Tautulli (history)", "plex_history": "Plex (history)"}

// pageData is passed to every template.
type pageData struct {
	Title     string
	Active    string
	CSRFToken string
	Session   *store.Session
	DryRun    bool
	Version   string
	sync      *lazySync // read through Sync()
	// Missing lists the services the synchronization still needs (banner).
	Missing []string
	// OOB marks the banner and the sync status for an HTMX out-of-band swap.
	OOB bool
	// Theme is "light" or "dark" when chosen with the theme button, "" to
	// follow the system.
	Theme string
	// SidebarCollapsed keeps the menu reduced to its icons (button, cookie).
	SidebarCollapsed bool
	// Lang is the interface language; Path the current path (language links).
	Lang i18n.Lang
	Path string
	Data any
}

// lazySync computes the sync status at most once, and only when a template
// shows it (layout, status partial): fragments such as the infinite scroll
// or a filter change skip its work (LastSyncAt query until the first sync).
type lazySync struct {
	once sync.Once
	get  func() *syncView
	v    *syncView
}

// Sync is the sync status of the top bar, nil without a session.
func (p pageData) Sync() *syncView {
	if p.sync == nil {
		return nil
	}
	p.sync.once.Do(func() { p.sync.v = p.sync.get() })
	return p.sync.v
}

// sidebarCookie remembers the collapsed menu (set by app.js).
const sidebarCookie = "hangarr_sidebar"

func sidebarCollapsed(r *http.Request) bool {
	c, err := r.Cookie(sidebarCookie)
	return err == nil && c.Value == "collapsed"
}

// themeCookie remembers the theme chosen with the button (set by app.js).
const themeCookie = "hangarr_theme"

func themeFrom(r *http.Request) string {
	if c, err := r.Cookie(themeCookie); err == nil && (c.Value == "light" || c.Value == "dark") {
		return c.Value
	}
	return ""
}

// HXHeaders is the JSON for hx-headers: HTMX sends the CSRF token on every request.
func (p pageData) HXHeaders() string {
	b, _ := json.Marshal(map[string]string{csrfHeader: p.CSRFToken})
	return string(b)
}

// page builds the data of a page; title is an English key, translated here.
func (s *Server) page(r *http.Request, title, active string, data any) pageData {
	return s.pageRaw(r, s.loc(r).T(title), active, data)
}

// pageRaw is page with a title that is not translated (a media title, a
// user name).
func (s *Server) pageRaw(r *http.Request, title, active string, data any) pageData {
	l := s.loc(r)
	pd := pageData{Title: title, Active: active, DryRun: s.Config.DryRun, Version: s.Version, Data: data,
		Theme: themeFrom(r), SidebarCollapsed: sidebarCollapsed(r), Lang: l.Lang, Path: r.URL.Path}
	if sess := sessionFrom(r); sess != nil {
		pd.Session = sess
		pd.CSRFToken = sess.CSRFToken
		pd.sync = &lazySync{get: func() *syncView { return s.syncStatus(r.Context()) }}
		pd.Missing = s.Upstreams.Get().Missing
	}
	return pd
}

// render executes a full page (template "layout") or, when name is not
// empty, a single named template (HTMX partial). Output is buffered so a
// template error never produces a half-written page.
func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, page, name string, data pageData) {
	t, ok := s.views.pages[data.Lang][page]
	if !ok {
		t, ok = s.views.pages[i18n.EN][page]
	}
	if !ok {
		s.serverError(w, r, fmt.Errorf("unknown page %q", page))
		return
	}
	if name == "" {
		name = "layout"
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, name, data); err != nil {
		s.serverError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

func (s *Server) static() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	files := http.StripPrefix("/static/", http.FileServerFS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/static/")
		if h, ok := s.views.assets[name]; ok && r.URL.Query().Get("v") == h {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=300")
		}
		files.ServeHTTP(w, r)
	})
}

// browserSummary turns a User-Agent into "Firefox · Linux".
func browserSummary(l i18n.Localizer, ua string) string {
	browser := l.T("Unknown browser")
	for _, b := range []struct{ token, name string }{
		{"Edg/", "Edge"}, {"OPR/", "Opera"}, {"Firefox/", "Firefox"}, {"Chrome/", "Chrome"},
		{"Safari/", "Safari"}, {"curl/", "curl"},
	} {
		if strings.Contains(ua, b.token) {
			browser = b.name
			break
		}
	}
	for _, o := range []struct{ token, name string }{
		{"Android", "Android"}, {"iPhone", "iOS"}, {"iPad", "iPadOS"}, {"Windows", "Windows"},
		{"Mac OS X", "macOS"}, {"Linux", "Linux"},
	} {
		if strings.Contains(ua, o.token) {
			return browser + " · " + o.name
		}
	}
	return browser
}

// stateLabel is the badge text: "Never watched", "In progress 3/10"…
func stateLabel(l i18n.Localizer, it store.LibraryItem) string {
	progress := ""
	if it.Kind == "series" && it.DownloadedEpisodes > 0 {
		progress = fmt.Sprintf(" %d/%d", it.WatchedEpisodes, it.DownloadedEpisodes)
	}
	switch it.State {
	case "in_progress":
		return l.T("In progress") + progress
	case "completed":
		return l.T("Completed")
	case "abandoned":
		return l.T("Abandoned") + progress
	default:
		return l.T("Never watched")
	}
}

// imgURL is the proxied image of an item: Sonarr/Radarr when it manages the
// item, else Plex (plex_only items); "" when there is none.
func imgURL(kind string, arrID int64, plexKey, variant string) string {
	if arrID > 0 {
		return fmt.Sprintf("/img/%s/%d/%s", kind, arrID, variant)
	}
	if n, err := strconv.ParseInt(plexKey, 10, 64); err == nil && n > 0 {
		return fmt.Sprintf("/img/plex/%d/%s", n, variant)
	}
	return ""
}

// isProtected reports whether tags contain a PROTECTED_TAGS entry (case-insensitive).
func isProtected(tags, protected []string) bool {
	for _, t := range tags {
		for _, p := range protected {
			if strings.EqualFold(t, p) {
				return true
			}
		}
	}
	return false
}

// pair is a value/label option for selects and checkbox lists.
type pair struct{ V, L string }

// pairs turns "v1", "l1", "v2", "l2"… into ordered options.
func pairs(kv ...string) []pair {
	out := make([]pair, 0, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		out = append(out, pair{V: kv[i], L: kv[i+1]})
	}
	return out
}

// transcodeLabel translates Tautulli's transcode_decision.
func transcodeLabel(l i18n.Localizer, v string) string {
	switch strings.ToLower(v) {
	case "direct play":
		return l.T("Direct play")
	case "copy", "direct stream":
		return l.T("Direct stream")
	case "transcode":
		return l.T("Transcode")
	default:
		return v
	}
}
