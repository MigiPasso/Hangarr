// Command capture-fixtures records anonymized responses of the real
// Sonarr, Radarr, Plex and Tautulli instances (configured like cleanarr,
// through environment variables) into testdata/captured/, using the layout
// served by mockarr. Run it with `make capture-fixtures`, then review the
// files before committing them.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"cleanarr/internal/clients/httpx"
	"cleanarr/internal/config"
)

func main() {
	out := flag.String("out", "testdata/captured", "output directory")
	limit := flag.Int("limit", 30, "maximum items per list (series, movies, Plex items, history rows)")
	flag.Parse()
	if err := run(*out, *limit); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

type capturer struct {
	ctx   context.Context
	out   string
	limit int
	anon  *anonymizer
	files int
}

func run(out string, limit int) error {
	cfg, err := config.Load(os.LookupEnv, os.ReadFile)
	if err != nil {
		return err
	}
	if err := cfg.RequireUpstreams(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	c := &capturer{ctx: ctx, out: out, limit: limit, anon: newAnonymizer()}

	arrAuth := func(key string) func(*http.Request) {
		return func(r *http.Request) { r.Header.Set("X-Api-Key", key) }
	}
	sonarr := httpx.New("sonarr", cfg.SonarrURL, arrAuth(cfg.SonarrAPIKey))
	radarr := httpx.New("radarr", cfg.RadarrURL, arrAuth(cfg.RadarrAPIKey))
	plex := httpx.New("plex", cfg.PlexURL, func(r *http.Request) { r.Header.Set("X-Plex-Token", cfg.PlexToken) })

	steps := []struct {
		file   string
		client *httpx.Client
		path   string
		query  url.Values
		trim   bool
	}{
		{"sonarr/series.json", sonarr, "/api/v3/series", nil, true},
		{"sonarr/tag.json", sonarr, "/api/v3/tag", nil, false},
		{"sonarr/qualityprofile.json", sonarr, "/api/v3/qualityprofile", nil, false},
		{"radarr/movie.json", radarr, "/api/v3/movie", nil, true},
		{"radarr/tag.json", radarr, "/api/v3/tag", nil, false},
		{"radarr/qualityprofile.json", radarr, "/api/v3/qualityprofile", nil, false},
		{"plex/sections.json", plex, "/library/sections", nil, false},
		{"plex/accounts.json", plex, "/accounts", nil, false},
		{"plex/history.json", plex, "/status/sessions/history/all", c.plexPage(url.Values{"sort": {"viewedAt:desc"}}), false},
	}
	for _, s := range steps {
		if err := c.capture(s.file, s.client, s.path, s.query, s.trim); err != nil {
			return err
		}
	}

	// One file per Plex movie/show section and item type.
	var sections struct {
		MediaContainer struct {
			Directory []struct {
				Key  httpx.String `json:"key"`
				Type string       `json:"type"`
			}
		}
	}
	if err := plex.JSON(ctx, httpx.Request{Path: "/library/sections"}, &sections); err != nil {
		return err
	}
	for _, sec := range sections.MediaContainer.Directory {
		types := map[string][]int{"movie": {1}, "show": {2, 3}}[sec.Type]
		for _, t := range types {
			q := c.plexPage(url.Values{"type": {strconv.Itoa(t)}, "includeGuids": {"1"}})
			file := fmt.Sprintf("plex/section-%s-type-%d.json", sec.Key, t)
			if err := c.capture(file, plex, "/library/sections/"+url.PathEscape(string(sec.Key))+"/all", q, false); err != nil {
				return err
			}
		}
	}

	if cfg.TautulliURL != "" && cfg.TautulliAPIKey != "" {
		tautulli := httpx.New("tautulli", cfg.TautulliURL, func(r *http.Request) {
			q := r.URL.Query()
			q.Set("apikey", cfg.TautulliAPIKey)
			r.URL.RawQuery = q.Encode()
		})
		history := url.Values{"cmd": {"get_history"}, "grouping": {"0"}, "order_column": {"date"},
			"order_dir": {"desc"}, "start": {"0"}, "length": {strconv.Itoa(limit)}}
		if err := c.capture("tautulli/get_history.json", tautulli, "/api/v2", history, false); err != nil {
			return err
		}
		if err := c.capture("tautulli/get_users.json", tautulli, "/api/v2", url.Values{"cmd": {"get_users"}}, false); err != nil {
			return err
		}
	}
	fmt.Printf("%d fichiers écrits dans %s : relisez-les avant de les commiter.\n", c.files, c.out)
	return nil
}

func (c *capturer) plexPage(q url.Values) url.Values {
	q.Set("X-Plex-Container-Start", "0")
	q.Set("X-Plex-Container-Size", strconv.Itoa(c.limit))
	return q
}

func (c *capturer) capture(file string, client *httpx.Client, p string, q url.Values, trim bool) error {
	b, err := client.Do(c.ctx, httpx.Request{Path: p, Query: q})
	if err != nil {
		return err
	}
	var doc any
	if err := json.Unmarshal(b, &doc); err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}
	if list, ok := doc.([]any); ok && trim && len(list) > c.limit {
		doc = list[:c.limit]
	}
	doc = c.anon.walk(doc, strings.HasPrefix(file, "plex/accounts"))
	pretty, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	dst := filepath.Join(c.out, filepath.FromSlash(file))
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	if err := os.WriteFile(dst, append(pretty, '\n'), 0o644); err != nil { //nolint:gosec // fixtures are meant to be read
		return err
	}
	c.files++
	fmt.Println("écrit", dst)
	return nil
}

// anonymizer replaces personal data with stable pseudonyms, so that the same
// user keeps the same alias across files.
type anonymizer struct {
	users map[string]string
	ips   map[string]string
}

func newAnonymizer() *anonymizer {
	return &anonymizer{users: map[string]string{}, ips: map[string]string{}}
}

func (a *anonymizer) user(v string) string {
	if v == "" || strings.EqualFold(v, "local") {
		return v
	}
	if p, ok := a.users[v]; ok {
		return p
	}
	p := fmt.Sprintf("user%d", len(a.users)+1)
	a.users[v] = p
	return p
}

func (a *anonymizer) ip(v string) string {
	if v == "" {
		return v
	}
	if p, ok := a.ips[v]; ok {
		return p
	}
	p := fmt.Sprintf("192.0.2.%d", len(a.ips)+1) // TEST-NET-1
	a.ips[v] = p
	return p
}

func (a *anonymizer) walk(v any, accounts bool) any {
	switch x := v.(type) {
	case []any:
		for i := range x {
			x[i] = a.walk(x[i], accounts)
		}
	case map[string]any:
		// Sorted keys: pseudonyms (user1, user2…) are deterministic across runs.
		for _, k := range slices.Sorted(maps.Keys(x)) {
			val := x[k]
			s, isString := val.(string)
			switch lk := strings.ToLower(k); {
			case !isString:
				x[k] = a.walk(val, accounts)
			case lk == "user" || lk == "username" || lk == "friendly_name" || (accounts && lk == "name"):
				x[k] = a.user(s)
			case lk == "email":
				if s != "" {
					x[k] = "redacted@example.invalid"
				}
			case strings.HasPrefix(lk, "ip_address") || lk == "ip" || lk == "address" || lk == "remotepublicaddress":
				x[k] = a.ip(s)
			case lk == "user_thumb" || (lk == "thumb" && strings.Contains(s, "plex.tv")):
				x[k] = ""
			case lk == "path" || lk == "folderpath" || lk == "rootfolderpath" || lk == "relativepath":
				if s != "" {
					x[k] = "/media/" + path.Base(s)
				}
			case lk == "machineidentifier" || lk == "uuid" || lk == "session_key" || lk == "machine_id":
				x[k] = "redacted"
			}
		}
	}
	return v
}
