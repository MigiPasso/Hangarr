// Command capture-fixtures records anonymized responses of the real Sonarr,
// Radarr, Plex, Tautulli and Jellyfin instances (configured like hangarr,
// through environment variables) into testdata/captured/, using the layout
// served by mockarr. Every configured service is recorded; the others are
// skipped. Run it with `make capture-fixtures` (or `make jellyfin-lab-capture`
// for the Jellyfin lab), then review the files before committing them.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"hangarr/internal/clients/httpx"
	"hangarr/internal/clients/jellyfin"
	"hangarr/internal/config"
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

// step is one listing recorded into one file.
type step struct {
	file   string
	client *httpx.Client
	path   string
	query  url.Values
	trim   bool
}

// maxVersionDetails bounds the Plex movies with several versions recorded one
// by one, per section.
const maxVersionDetails = 5

func run(out string, limit int) error {
	cfg, err := config.Load(os.LookupEnv, os.ReadFile)
	if err != nil {
		return err
	}
	return captureAll(cfg, out, limit)
}

// captureAll records every service configured in cfg. It fails before writing
// anything when none is configured.
func captureAll(cfg *config.Config, out string, limit int) error {
	sonarrOK := cfg.SonarrURL != "" && cfg.SonarrAPIKey != ""
	radarrOK := cfg.RadarrURL != "" && cfg.RadarrAPIKey != ""
	plexOK := cfg.PlexURL != "" && cfg.PlexToken != ""
	tautulliOK := cfg.TautulliURL != "" && cfg.TautulliAPIKey != ""
	jellyfinOK := cfg.JellyfinURL != "" && cfg.JellyfinAPIKey != ""
	if !sonarrOK && !radarrOK && !plexOK && !tautulliOK && !jellyfinOK {
		return errors.New("no service configured: set SONARR_URL and SONARR_API_KEY, RADARR_URL and RADARR_API_KEY, " +
			"PLEX_URL and PLEX_TOKEN, TAUTULLI_URL and TAUTULLI_API_KEY, or JELLYFIN_URL and JELLYFIN_API_KEY")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	c := &capturer{ctx: ctx, out: out, limit: limit, anon: newAnonymizer()}

	arrAuth := func(key string) func(*http.Request) {
		return func(r *http.Request) { r.Header.Set("X-Api-Key", key) }
	}
	now := time.Now().UTC()
	window := url.Values{"start": {now.Format(time.RFC3339)}, "end": {now.AddDate(0, 0, 90).Format(time.RFC3339)}}
	sonarrCal := url.Values{"includeSeries": {"true"}}
	for k, v := range window {
		sonarrCal[k] = v
	}

	var steps []step
	if sonarrOK {
		sonarr := httpx.New("sonarr", cfg.SonarrURL, arrAuth(cfg.SonarrAPIKey))
		steps = append(steps,
			step{"sonarr/series.json", sonarr, "/api/v3/series", nil, true},
			step{"sonarr/tag.json", sonarr, "/api/v3/tag", nil, false},
			step{"sonarr/qualityprofile.json", sonarr, "/api/v3/qualityprofile", nil, false},
			step{"sonarr/calendar.json", sonarr, "/api/v3/calendar", sonarrCal, true},
		)
	} else {
		skip("sonarr")
	}
	if radarrOK {
		radarr := httpx.New("radarr", cfg.RadarrURL, arrAuth(cfg.RadarrAPIKey))
		steps = append(steps,
			step{"radarr/movie.json", radarr, "/api/v3/movie", nil, true},
			step{"radarr/tag.json", radarr, "/api/v3/tag", nil, false},
			step{"radarr/qualityprofile.json", radarr, "/api/v3/qualityprofile", nil, false},
			step{"radarr/calendar.json", radarr, "/api/v3/calendar", window, true},
		)
	} else {
		skip("radarr")
	}
	var plex *httpx.Client
	if plexOK {
		plex = httpx.New("plex", cfg.PlexURL, func(r *http.Request) { r.Header.Set("X-Plex-Token", cfg.PlexToken) })
		steps = append(steps,
			step{"plex/sections.json", plex, "/library/sections", nil, false},
			step{"plex/accounts.json", plex, "/accounts", nil, false},
			step{"plex/history.json", plex, "/status/sessions/history/all", c.plexPage(url.Values{"sort": {"viewedAt:desc"}}), false},
		)
	} else {
		skip("plex")
	}
	for _, s := range steps {
		if err := c.capture(s.file, s.client, s.path, s.query, s.trim); err != nil {
			return err
		}
	}

	// One file per Plex movie/show section and item type.
	if plex != nil {
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
			if !safeKey.MatchString(string(sec.Key)) {
				fmt.Fprintf(os.Stderr, "warning: plex section key %q skipped (not alphanumeric)\n", string(sec.Key))
				continue
			}
			types := map[string][]int{"movie": {1}, "show": {2, 3}}[sec.Type]
			for _, t := range types {
				q := c.plexPage(url.Values{"type": {strconv.Itoa(t)}, "includeGuids": {"1"}})
				file := fmt.Sprintf("plex/section-%s-type-%d.json", sec.Key, t)
				if err := c.capture(file, plex, "/library/sections/"+url.PathEscape(string(sec.Key))+"/all", q, false); err != nil {
					return err
				}
			}
		}

		// Plex movies with several versions (files and streams), detailed one by
		// one, and one single-version movie as a reference.
		for _, sec := range sections.MediaContainer.Directory {
			if sec.Type != "movie" {
				continue
			}
			multi, single, err := c.plexMultiVersion(plex, string(sec.Key))
			if err != nil {
				return err
			}
			detailed := 0
			for _, key := range multi {
				if detailed == maxVersionDetails {
					break
				}
				if !safeKey.MatchString(key) {
					continue
				}
				file := fmt.Sprintf("plex/metadata-%s.json", key)
				if err := c.capture(file, plex, "/library/metadata/"+url.PathEscape(key), nil, false); err != nil {
					return err
				}
				detailed++
			}
			if safeKey.MatchString(single) {
				file := fmt.Sprintf("plex/metadata-%s.json", single)
				if err := c.capture(file, plex, "/library/metadata/"+url.PathEscape(single), nil, false); err != nil {
					return err
				}
			}
			fmt.Printf("plex section %s: %d movies with several versions (%d detailed)\n", string(sec.Key), len(multi), detailed)
		}
	}

	if tautulliOK {
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
	} else {
		skip("tautulli")
	}

	if jellyfinOK {
		if err := c.captureJellyfin(jellyfin.New(cfg.JellyfinURL, cfg.JellyfinAPIKey)); err != nil {
			return err
		}
	} else {
		skip("jellyfin")
	}
	fmt.Printf("%d files written to %s: review them before committing.\n", c.files, c.out)
	return nil
}

func skip(service string) {
	fmt.Printf("skipped %s: not configured\n", service)
}

// captureJellyfin records the server identity, the accounts and, for each
// account, the items it played and the ones it can resume. The listings use
// the parameters of the jellyfin client, with the page size of -limit.
func (c *capturer) captureJellyfin(jf *jellyfin.Client) error {
	if err := c.capture("jellyfin/system-info.json", jf.HTTP, "/System/Info", nil, false); err != nil {
		return err
	}
	if err := c.capture("jellyfin/users.json", jf.HTTP, "/Users", nil, false); err != nil {
		return err
	}
	users, err := jf.Users(c.ctx)
	if err != nil {
		return err
	}
	page := func(q url.Values) url.Values {
		q.Set("startIndex", "0")
		q.Set("limit", strconv.Itoa(c.limit))
		return q
	}
	for _, typ := range []string{jellyfin.TypeMovie, jellyfin.TypeSeries, jellyfin.TypeSeason} {
		q := page(url.Values{
			"recursive":        {"true"},
			"includeItemTypes": {typ},
			"fields":           {"ProviderIds,Path,DateCreated,OriginalTitle"},
			"sortBy":           {"SortName"},
			"enableImages":     {"false"},
		})
		if err := c.capture("jellyfin/items-"+typ+".json", jf.HTTP, "/Items", q, false); err != nil {
			return err
		}
	}
	versions := page(url.Values{
		"recursive":        {"true"},
		"includeItemTypes": {jellyfin.TypeMovie},
		"fields":           {"ProviderIds,Path,DateCreated,OriginalTitle,MediaSources,MediaSourceCount"},
		"sortBy":           {"SortName"},
		"enableImages":     {"false"},
	})
	if err := c.capture("jellyfin/items-"+jellyfin.TypeMovie+"-versions.json", jf.HTTP, "/Items", versions, false); err != nil {
		return err
	}
	movieAndEpisode := jellyfin.TypeMovie + "," + jellyfin.TypeEpisode
	for _, u := range users {
		played := page(url.Values{
			"userId":           {u.ID},
			"recursive":        {"true"},
			"includeItemTypes": {movieAndEpisode},
			"isPlayed":         {"true"},
			"sortBy":           {"DatePlayed"},
			"sortOrder":        {"Descending"},
			"enableUserData":   {"true"},
			"enableImages":     {"false"},
		})
		if err := c.capture("jellyfin/played-"+u.ID+".json", jf.HTTP, "/Items", played, false); err != nil {
			return err
		}
		resumable := page(url.Values{
			"userId":           {u.ID},
			"recursive":        {"true"},
			"includeItemTypes": {movieAndEpisode},
			"filters":          {"IsResumable"},
			"enableUserData":   {"true"},
			"enableImages":     {"false"},
		})
		if err := c.capture("jellyfin/resumable-"+u.ID+".json", jf.HTTP, "/Items", resumable, false); err != nil {
			return err
		}
	}
	return nil
}

// plexMultiVersion scans a whole Plex movie section, page by page, and returns
// the ratingKeys of the movies with several versions, and the first movie with
// a single version (empty if none).
func (c *capturer) plexMultiVersion(plex *httpx.Client, sectionKey string) (multi []string, single string, err error) {
	const scanPage = 500
	const maxPages = 200
	type metadata struct {
		RatingKey httpx.String `json:"ratingKey"`
		Media     []struct {
			ID httpx.Int `json:"id"`
		} `json:"Media"`
	}
	type result struct {
		MediaContainer struct {
			Metadata []metadata `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	listPath := "/library/sections/" + url.PathEscape(sectionKey) + "/all"
	var first string
	for n := 0; n < maxPages; n++ {
		var list result
		q := url.Values{
			"type":                   {"1"},
			"X-Plex-Container-Start": {strconv.Itoa(n * scanPage)},
			"X-Plex-Container-Size":  {strconv.Itoa(scanPage)},
		}
		if err := plex.JSON(c.ctx, httpx.Request{Path: listPath, Query: q}, &list); err != nil {
			return nil, "", err
		}
		items := list.MediaContainer.Metadata
		if len(items) > 0 {
			if n == 0 {
				first = string(items[0].RatingKey)
			} else if string(items[0].RatingKey) == first {
				fmt.Fprintf(os.Stderr, "warning: plex section %s: paging ignored, scan stopped after %d items\n", sectionKey, n*scanPage)
				return multi, single, nil
			}
		}
		for _, m := range items {
			switch {
			case len(m.Media) >= 2:
				multi = append(multi, string(m.RatingKey))
			case len(m.Media) == 1 && single == "":
				single = string(m.RatingKey)
			}
		}
		if len(items) < scanPage {
			return multi, single, nil
		}
	}
	fmt.Fprintf(os.Stderr, "warning: plex section %s: more than %d pages, scan stopped\n", sectionKey, maxPages)
	return multi, single, nil
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
	accounts := strings.HasPrefix(file, "plex/accounts") || file == "jellyfin/users.json"
	doc = c.anon.walk(doc, accounts)
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
	fmt.Println("wrote", dst)
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

// secretName reports a field that holds a credential (Tautulli may return
// server tokens, *arr API keys…). Plex's "key", "ratingKey", "parentKey"
// are identifiers the fixtures need: a plain "key" does not count.
func secretName(lk string) bool {
	for _, w := range []string{"token", "secret", "password", "passwd", "apikey", "api_key", "accesskey", "access_key",
		"privatekey", "private_key", "authorization", "cookie"} {
		if strings.Contains(lk, w) {
			return true
		}
	}
	return false
}

// safeKey is the form of a Plex key used in a file name: digits and letters
// only, so that an unexpected answer cannot write outside the output folder.
var safeKey = regexp.MustCompile(`^[0-9A-Za-z]+$`)

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
			case lk == "path" || lk == "file" || lk == "folderpath" || lk == "rootfolderpath" || lk == "relativepath":
				if s != "" {
					x[k] = "/media/" + path.Base(s)
				}
			case lk == "machineidentifier" || lk == "uuid" || lk == "session_key" || lk == "machine_id" || lk == "servername" ||
				lk == "localaddress": // Jellyfin's LAN URL
				x[k] = "redacted"
			case secretName(lk):
				if s != "" {
					x[k] = "redacted"
				}
			}
		}
	}
	return v
}
