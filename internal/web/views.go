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
	"time"

	"arrstats/internal/store"
)

//go:embed templates
var templatesFS embed.FS

//go:embed static
var staticFS embed.FS

// pages lists every page template under templates/pages.
var pages = []string{"login", "password", "account", "home", "notfound", "library", "settings", "detail", "delete", "journal",
	"stats", "userstats", "cleanup", "config"}

type views struct {
	pages  map[string]*template.Template
	assets map[string]string // file name -> content hash, for cache busting
}

func newViews(loc *time.Location, protectedTags []string, abandonDays int) (*views, error) {
	v := &views{pages: map[string]*template.Template{}, assets: map[string]string{}}
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

	funcs := template.FuncMap{
		"asset": func(name string) (string, error) {
			h, ok := v.assets[name]
			if !ok {
				return "", fmt.Errorf("unknown asset %q", name)
			}
			return "/static/" + name + "?v=" + h, nil
		},
		"datetime": func(unix int64) string { return time.Unix(unix, 0).In(loc).Format("02/01/2006 15:04") },
		"reltime":  func(unix int64) string { return relativeTime(time.Now(), time.Unix(unix, 0)) },
		"browser":  browserSummary,
		"relptr": func(unix *int64) string {
			if unix == nil {
				return ""
			}
			return relativeTime(time.Now(), time.Unix(*unix, 0))
		},
		"dateptr": func(unix *int64) string {
			if unix == nil {
				return ""
			}
			return time.Unix(*unix, 0).In(loc).Format("02/01/2006")
		},
		"bytes":      formatBytes,
		"stateLabel": stateLabel,
		"plural":     func(n int, one, many string) string { return plural(int64(n), one, many) },
		"word": func(n int, one, many string) string {
			if n > 1 {
				return many
			}
			return one
		},
		"protected": func(tags []string) bool { return isProtected(tags, protectedTags) },
		"has":       func(list []string, v string) bool { return slices.Contains(list, v) },
		"hasID":     func(list []int64, v int64) bool { return slices.Contains(list, v) },
		"list":      func(v ...string) []string { return v },
		"pairs":     pairs,
		"mediaPath": kindPath,
		"hours":     hours,
		"share":     func(f float64) string { return trimFloat(f*100) + " %" },
		"pctOf": func(a, b int64) string {
			if b <= 0 {
				return "0 %"
			}
			return trimFloat(float64(a)*100/float64(b)) + " %"
		},
		"num":         func(f float64) string { return trimFloat(f) },
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
		"stateName": func(s string) string {
			return map[string]string{"never": "Jamais vu", "in_progress": "En cours", "completed": "Terminé",
				"abandoned": "Abandonné"}[s]
		},
		"episode": func(season, episode *int) string {
			if season == nil || episode == nil {
				return ""
			}
			return episodeLabel(*season, *episode)
		},
		"duration":  formatDuration,
		"transcode": transcodeLabel,
		"authLabel": func(e string) string {
			if l, ok := authEventLabels[e]; ok {
				return l
			}
			return e
		},
		"deref": func(p *int) int {
			if p == nil {
				return 0
			}
			return *p
		},
	}
	for _, p := range pages {
		t, err := template.New("").Funcs(funcs).ParseFS(templatesFS,
			"templates/layout.html", "templates/partials/*.html", "templates/pages/"+p+".html")
		if err != nil {
			return nil, fmt.Errorf("parse page %s: %w", p, err)
		}
		v.pages[p] = t
	}
	return v, nil
}

// pageData is passed to every template.
type pageData struct {
	Title     string
	Active    string
	CSRFToken string
	Session   *store.Session
	DryRun    bool
	Version   string
	Sync      *syncView
	// Missing lists the services the synchronization still needs (banner).
	Missing []string
	// OOB marks the banner and the sync status for an HTMX out-of-band swap.
	OOB  bool
	Data any
}

// HXHeaders is the JSON for hx-headers: HTMX sends the CSRF token on every request.
func (p pageData) HXHeaders() string {
	b, _ := json.Marshal(map[string]string{csrfHeader: p.CSRFToken})
	return string(b)
}

func (s *Server) page(r *http.Request, title, active string, data any) pageData {
	pd := pageData{Title: title, Active: active, DryRun: s.Config.DryRun, Version: s.Version, Data: data}
	if sess := sessionFrom(r); sess != nil {
		pd.Session = sess
		pd.CSRFToken = sess.CSRFToken
		pd.Sync = s.syncStatus(r.Context())
		pd.Missing = s.Upstreams.Get().Missing
	}
	return pd
}

// render executes a full page (template "layout") or, when name is not
// empty, a single named template (HTMX partial). Output is buffered so a
// template error never produces a half-written page.
func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, page, name string, data pageData) {
	t, ok := s.views.pages[page]
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

func plural(n int64, one, many string) string {
	if n > 1 {
		return fmt.Sprintf("%d %s", n, many)
	}
	return fmt.Sprintf("%d %s", n, one)
}

// relativeTime formats t relative to now in French ("il y a 2 h").
func relativeTime(now, t time.Time) string {
	d := now.Sub(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return "à l'instant"
	case d < time.Hour:
		return fmt.Sprintf("il y a %d min", int64(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("il y a %d h", int64(d/time.Hour))
	case d < 30*24*time.Hour:
		return "il y a " + plural(int64(d/(24*time.Hour)), "jour", "jours")
	case d < 365*24*time.Hour:
		return fmt.Sprintf("il y a %d mois", int64(d/(30*24*time.Hour)))
	default:
		return "il y a " + plural(int64(d/(365*24*time.Hour)), "an", "ans")
	}
}

// browserSummary turns a User-Agent into "Firefox · Linux".
func browserSummary(ua string) string {
	browser := "Navigateur inconnu"
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

// formatBytes renders sizes the French way: "12,5 Go", "1,2 To".
func formatBytes(n int64) string {
	units := []string{"o", "Ko", "Mo", "Go", "To", "Po"}
	f := float64(n)
	i := 0
	for f >= 1000 && i < len(units)-1 {
		f /= 1000
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d o", n)
	}
	prec := 1
	if f >= 100 {
		prec = 0
	}
	return strings.Replace(strconv.FormatFloat(f, 'f', prec, 64), ".", ",", 1) + " " + units[i]
}

// stateLabel is the badge text: "Jamais vu", "En cours 3/10", "Terminé", "Abandonné".
func stateLabel(it store.LibraryItem) string {
	progress := ""
	if it.Kind == "series" && it.DownloadedEpisodes > 0 {
		progress = fmt.Sprintf(" %d/%d", it.WatchedEpisodes, it.DownloadedEpisodes)
	}
	switch it.State {
	case "in_progress":
		return "En cours" + progress
	case "completed":
		return "Terminé"
	case "abandoned":
		return "Abandonné" + progress
	default:
		return "Jamais vu"
	}
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

// formatDuration renders seconds as "1 h 05 min" or "42 min".
func formatDuration(secs int64) string {
	if secs < 60 {
		return fmt.Sprintf("%d s", secs)
	}
	h, m := secs/3600, secs%3600/60
	if h == 0 {
		return fmt.Sprintf("%d min", m)
	}
	return fmt.Sprintf("%d h %02d min", h, m)
}

// transcodeLabel translates Tautulli's transcode_decision.
func transcodeLabel(v string) string {
	switch strings.ToLower(v) {
	case "direct play":
		return "Direct"
	case "copy", "direct stream":
		return "Direct stream"
	case "transcode":
		return "Transcodage"
	default:
		return v
	}
}
