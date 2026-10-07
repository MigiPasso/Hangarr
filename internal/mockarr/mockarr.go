// Package mockarr impersonates Sonarr, Radarr, Plex and Tautulli
// from fixture files (see testdata/README.md). It backs `make dev`, the e2e
// tests and the sync integration tests. Write calls (DELETE, PUT, POST) are
// recorded and exposed on GET /_calls; POST /_reset clears them.
package mockarr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"io/fs"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Anchor is the reference date of the synthetic fixtures (2026-10-01 UTC).
const Anchor int64 = 1790812800

// Call is a recorded destructive request.
type Call struct {
	Service string    `json:"service"`
	Method  string    `json:"method"`
	Path    string    `json:"path"`
	Query   string    `json:"query"`
	Body    string    `json:"body"`
	At      time.Time `json:"at"`
}

// Mock serves fixtures.
type Mock struct {
	Fixtures fs.FS
	APIKey   string
	// ShiftSeconds is added to history timestamps (keeps dev data recent).
	ShiftSeconds int64
	// PlexDeletionDenied makes Plex refuse deletions (403), like a server
	// without "Allow media deletion".
	PlexDeletionDenied bool
	// DevExtras adds dev/{sonarr/series,radarr/movie}.json to the catalog
	// (the series and movies of the calendars; `make dev` only, the tests
	// rely on the synthetic catalog alone).
	DevExtras bool
	// RealPosters serves the posters listed in posters.json, downloaded
	// once (`make dev` only: the tests never reach the Internet).
	RealPosters bool

	mu      sync.Mutex
	calls   []Call
	hits    map[string]int
	posters map[string]poster // "sonarr/3" -> downloaded poster
}

type poster struct {
	body        []byte
	contentType string
}

// New returns a mock serving fixtures from fsys.
func New(fsys fs.FS, apiKey string) *Mock {
	return &Mock{Fixtures: fsys, APIKey: apiKey, hits: map[string]int{}, posters: map[string]poster{}}
}

// ShiftToNow sets ShiftSeconds so that the anchor becomes today (whole days).
func (m *Mock) ShiftToNow(now time.Time) {
	const day = 24 * 60 * 60
	m.ShiftSeconds = (now.Unix() - Anchor) / day * day
}

// Calls returns recorded destructive calls.
func (m *Mock) Calls() []Call {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Call{}, m.calls...)
}

// Hits returns how many times a route key was requested ("tautulli:get_history").
func (m *Mock) Hits(key string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hits[key]
}

func (m *Mock) hit(key string) {
	m.mu.Lock()
	m.hits[key]++
	m.mu.Unlock()
}

// Reset clears recorded calls and hit counters.
func (m *Mock) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls, m.hits = nil, map[string]int{}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (m *Mock) file(w http.ResponseWriter, name string) {
	b, err := fs.ReadFile(m.Fixtures, name)
	if err != nil {
		http.Error(w, `{"error":"fixture not found"}`, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b) //nolint:gosec // G705: JSON fixture served by a test double
}

func (m *Mock) readJSON(name string, v any) error {
	b, err := fs.ReadFile(m.Fixtures, name)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// wrap adds authentication, call recording and the /_calls, /_reset routes.
func (m *Mock) wrap(service string, auth func(*http.Request) bool, mux *http.ServeMux) http.Handler {
	mux.HandleFunc("GET /_calls", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, m.Calls()) })
	mux.HandleFunc("POST /_reset", func(w http.ResponseWriter, _ *http.Request) {
		m.Reset()
		w.WriteHeader(http.StatusNoContent)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_calls" && r.URL.Path != "/_reset" {
			if !auth(r) {
				http.Error(w, `{"error":"Unauthorized"}`, http.StatusUnauthorized)
				return
			}
			if r.Method == http.MethodDelete || r.Method == http.MethodPut || r.Method == http.MethodPost {
				body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
				r.Body = io.NopCloser(bytes.NewReader(body))
				m.mu.Lock()
				m.calls = append(m.calls, Call{Service: service, Method: r.Method, Path: r.URL.Path,
					Query: r.URL.RawQuery, Body: string(body), At: time.Now()})
				m.mu.Unlock()
			}
		}
		mux.ServeHTTP(w, r)
	})
}

func (m *Mock) apiKey(r *http.Request) bool {
	return r.Header.Get("X-Api-Key") == m.APIKey || r.URL.Query().Get("apikey") == m.APIKey
}

func (m *Mock) arr(service, appName string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v3/system/status", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"appName": appName, "version": "4.0.0.0-mock"})
	})
	m.arrItems(mux, service)
	mux.HandleFunc("GET /api/v3/calendar", func(w http.ResponseWriter, r *http.Request) {
		m.hit(service + ":calendar")
		m.calendar(w, r, service)
	})
	mux.HandleFunc("GET /api/v3/mediacover/{id}/{file}", func(w http.ResponseWriter, r *http.Request) {
		m.hit(service + ":mediacover")
		m.mediaCover(w, r, service)
	})
	mux.HandleFunc("GET /api/v3/indexerstatus", func(w http.ResponseWriter, _ *http.Request) {
		m.hit(service + ":indexerstatus")
		var rows []map[string]any
		if err := m.readJSON(service+"/indexerstatus.json", &rows); err != nil {
			writeJSON(w, []any{})
			return
		}
		for _, row := range rows {
			for _, k := range []string{"initialFailure", "mostRecentFailure", "disabledTill"} {
				if t, err := time.Parse(time.RFC3339, fmt.Sprint(row[k])); err == nil {
					row[k] = t.Add(todayShift()).Format(time.RFC3339)
				}
			}
		}
		writeJSON(w, rows)
	})
	for _, res := range []string{"tag", "qualityprofile", "diskspace", "health", "indexer"} {
		mux.HandleFunc("GET /api/v3/"+res, func(w http.ResponseWriter, _ *http.Request) {
			m.hit(service + ":" + res)
			m.file(w, service+"/"+res+".json")
		})
	}
	res := map[string]string{"sonarr": "series", "radarr": "movie"}[service]
	mux.HandleFunc("GET /api/v3/"+res, func(w http.ResponseWriter, r *http.Request) {
		m.hit(service + ":" + res)
		items, err := m.catalog(service)
		if err != nil {
			http.Error(w, `{"error":"fixture not found"}`, http.StatusNotFound)
			return
		}
		writeJSON(w, items)
	})
	return m.wrap(service, m.apiKey, mux)
}

// Sonarr returns the fake Sonarr.
func (m *Mock) Sonarr() http.Handler { return m.arr("sonarr", "Sonarr") }

// Radarr returns the fake Radarr.
func (m *Mock) Radarr() http.Handler { return m.arr("radarr", "Radarr") }

func (m *Mock) shift(row map[string]any, keys ...string) {
	if m.ShiftSeconds == 0 {
		return
	}
	for _, k := range keys {
		if v, ok := row[k].(float64); ok && v > 0 {
			row[k] = v + float64(m.ShiftSeconds)
		}
	}
}

func intParam(r *http.Request, name string, def int) int {
	v := r.URL.Query().Get(name)
	if v == "" {
		v = r.Header.Get(name)
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 0 {
		return n
	}
	return def
}

// plexPage serves a MediaContainer fixture, honoring X-Plex-Container-Start
// and -Size (query or header) and shifting the given timestamp fields.
func (m *Mock) plexPage(w http.ResponseWriter, r *http.Request, name string, shiftKeys ...string) {
	var doc map[string]map[string]any
	if err := m.readJSON(name, &doc); err != nil {
		http.Error(w, `{"error":"fixture not found"}`, http.StatusNotFound)
		return
	}
	mc := doc["MediaContainer"]
	items, _ := mc["Metadata"].([]any)
	for _, it := range items {
		if row, ok := it.(map[string]any); ok {
			m.shift(row, shiftKeys...)
		}
	}
	start := intParam(r, "X-Plex-Container-Start", 0)
	size := intParam(r, "X-Plex-Container-Size", len(items))
	start = min(start, len(items))
	end := min(start+size, len(items))
	mc["Metadata"] = items[start:end]
	mc["offset"], mc["size"], mc["totalSize"] = start, end-start, len(items)
	writeJSON(w, doc)
}

// Plex returns the fake Plex server.
func (m *Mock) Plex() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /identity", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"MediaContainer": map[string]any{"machineIdentifier": "mock", "version": "1.41.0-mock"}})
	})
	mux.HandleFunc("GET /library/sections", func(w http.ResponseWriter, _ *http.Request) {
		m.hit("plex:sections")
		m.file(w, "plex/sections.json")
	})
	mux.HandleFunc("GET /library/sections/{key}/all", func(w http.ResponseWriter, r *http.Request) {
		m.hit("plex:items")
		m.plexPage(w, r, "plex/section-"+r.PathValue("key")+"-type-"+r.URL.Query().Get("type")+".json")
	})
	mux.HandleFunc("GET /library/sections/{key}/refresh", func(w http.ResponseWriter, _ *http.Request) {
		m.hit("plex:refresh")
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("PUT /library/sections/{key}/emptyTrash", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	// Posters: the item's thumb/art, or the photo transcoder (resized).
	plexImage := func(w http.ResponseWriter, key, variant string) {
		id, err := strconv.ParseInt(key, 10, 64)
		if err != nil || (variant != "thumb" && variant != "art") {
			http.NotFound(w, nil)
			return
		}
		cover := map[string]string{"thumb": "poster", "art": "fanart"}[variant]
		w.Header().Set("Content-Type", "image/png")
		_ = png.Encode(w, coverImage("plex", id, cover))
	}
	mux.HandleFunc("GET /library/metadata/{key}/{variant}", func(w http.ResponseWriter, r *http.Request) {
		m.hit("plex:image")
		plexImage(w, r.PathValue("key"), r.PathValue("variant"))
	})
	mux.HandleFunc("GET /photo/:/transcode", func(w http.ResponseWriter, r *http.Request) {
		m.hit("plex:transcode")
		rest, ok := strings.CutPrefix(r.URL.Query().Get("url"), "/library/metadata/")
		key, variant, _ := strings.Cut(rest, "/")
		if !ok {
			http.NotFound(w, r)
			return
		}
		plexImage(w, key, variant)
	})
	mux.HandleFunc("DELETE /library/metadata/{key}", func(w http.ResponseWriter, _ *http.Request) {
		if m.PlexDeletionDenied {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /accounts", func(w http.ResponseWriter, _ *http.Request) {
		m.hit("plex:accounts")
		m.file(w, "plex/accounts.json")
	})
	mux.HandleFunc("GET /status/sessions/history/all", func(w http.ResponseWriter, r *http.Request) {
		m.hit("plex:history")
		m.plexPage(w, r, "plex/history.json", "viewedAt")
	})
	return m.wrap("plex", func(r *http.Request) bool {
		return r.Header.Get("X-Plex-Token") == m.APIKey || r.URL.Query().Get("X-Plex-Token") == m.APIKey
	}, mux)
}

func tautulliError(w http.ResponseWriter, msg string) {
	writeJSON(w, map[string]any{"response": map[string]any{"result": "error", "message": msg, "data": map[string]any{}}})
}

// Tautulli returns the fake Tautulli (API v2).
func (m *Mock) Tautulli() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v2", func(w http.ResponseWriter, r *http.Request) {
		cmd := r.URL.Query().Get("cmd")
		m.hit("tautulli:" + cmd)
		switch cmd {
		case "status", "arnold":
			writeJSON(w, map[string]any{"response": map[string]any{"result": "success", "message": nil, "data": map[string]any{}}})
		case "get_tautulli_info":
			writeJSON(w, map[string]any{"response": map[string]any{"result": "success", "message": nil,
				"data": map[string]any{"tautulli_version": "v2.15.0-mock"}}})
		case "get_users", "get_libraries":
			m.file(w, "tautulli/"+cmd+".json")
		case "get_history":
			m.tautulliHistory(w, r)
		default:
			tautulliError(w, "Unknown command: "+cmd)
		}
	})
	return m.wrap("tautulli", m.apiKey, mux)
}

func (m *Mock) tautulliHistory(w http.ResponseWriter, r *http.Request) {
	var doc struct {
		Response struct {
			Data struct {
				Data []map[string]any `json:"data"`
			} `json:"data"`
		} `json:"response"`
	}
	if err := m.readJSON("tautulli/get_history.json", &doc); err != nil {
		tautulliError(w, err.Error())
		return
	}
	rows := doc.Response.Data.Data
	for _, row := range rows {
		m.shift(row, "date", "started", "stopped")
	}
	date := func(row map[string]any) float64 { f, _ := row["date"].(float64); return f }
	sort.SliceStable(rows, func(i, j int) bool { return date(rows[i]) > date(rows[j]) })
	start := min(intParam(r, "start", 0), len(rows))
	end := min(start+intParam(r, "length", 25), len(rows))
	writeJSON(w, map[string]any{"response": map[string]any{"result": "success", "message": nil, "data": map[string]any{
		"recordsTotal": len(rows), "recordsFiltered": len(rows), "draw": 1, "data": rows[start:end],
	}}})
}

// mediaCover serves a small generated PNG for items whose fixture declares
// a matching image (poster/fanart), 404 otherwise.
func (m *Mock) mediaCover(w http.ResponseWriter, r *http.Request, service string) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	cover, _, _ := strings.Cut(r.PathValue("file"), ".")
	cover, _, _ = strings.Cut(cover, "-") // poster-500 → poster
	if m.RealPosters && cover == "poster" {
		if p, ok := m.realPoster(service + "/" + strconv.FormatInt(id, 10)); ok {
			w.Header().Set("Content-Type", p.contentType)
			_, _ = w.Write(p.body)
			return
		}
	}
	rows, err := m.catalog(service)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	for _, it := range rows {
		if v, _ := it["id"].(float64); int64(v) != id {
			continue
		}
		imgs, _ := it["images"].([]any)
		for _, img := range imgs {
			if c, _ := img.(map[string]any); c["coverType"] == cover {
				w.Header().Set("Content-Type", "image/png")
				_ = png.Encode(w, coverImage(service, id, cover))
				return
			}
		}
	}
	http.NotFound(w, r)
}

// coverImage draws a vertical gradient whose hue depends on the item.
func coverImage(service string, id int64, cover string) image.Image {
	width, height := 100, 150
	if cover == "fanart" {
		width, height = 320, 180
	}
	seed := uint8((id*53 + int64(len(service))*17) % 256) //nolint:gosec // G115: bounded by the modulo
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		shade := uint8(40 + y*120/height) //nolint:gosec // G115: 40..160
		c := color.RGBA{R: seed/2 + shade/3, G: shade / 2, B: 255 - seed/2 - shade/4, A: 255}
		for x := range width {
			img.SetRGBA(x, y, c)
		}
	}
	return img
}

// item returns the raw fixture object of a series/movie by id.
// catalog is the series (Sonarr) or movie (Radarr) list, with the dev
// extras when enabled.
func (m *Mock) catalog(service string) ([]map[string]any, error) {
	file := map[string]string{"sonarr": "sonarr/series.json", "radarr": "radarr/movie.json"}[service]
	var items []map[string]any
	if err := m.readJSON(file, &items); err != nil {
		return nil, err
	}
	if m.DevExtras {
		var extras []map[string]any
		if err := m.readJSON("dev/"+file, &extras); err == nil {
			items = append(items, extras...)
		}
	}
	return items, nil
}

func (m *Mock) item(service string, id int64) (map[string]any, bool) {
	items, err := m.catalog(service)
	if err != nil {
		return nil, false
	}
	for _, it := range items {
		if v, _ := it["id"].(float64); int64(v) == id {
			return it, true
		}
	}
	return nil, false
}

type mockEpisode struct {
	ID            int64  `json:"id"`
	SeriesID      int64  `json:"seriesId"`
	SeasonNumber  int    `json:"seasonNumber"`
	EpisodeNumber int    `json:"episodeNumber"`
	Title         string `json:"title"`
	AirDateUTC    string `json:"airDateUtc"`
	HasFile       bool   `json:"hasFile"`
	Monitored     bool   `json:"monitored"`
	EpisodeFileID int64  `json:"episodeFileId"`
}

// episodes generates a series' episodes from its season statistics: the
// first episodeFileCount episodes of each season have a file.
func episodes(series map[string]any) ([]mockEpisode, map[int64]int64) {
	sid := int64(series["id"].(float64))
	sizes := map[int64]int64{}
	var out []mockEpisode
	seasons, _ := series["seasons"].([]any)
	for _, s := range seasons {
		season, _ := s.(map[string]any)
		n := int(season["seasonNumber"].(float64))
		monitored, _ := season["monitored"].(bool)
		stats, _ := season["statistics"].(map[string]any)
		total, _ := stats["totalEpisodeCount"].(float64)
		files, _ := stats["episodeFileCount"].(float64)
		size, _ := stats["sizeOnDisk"].(float64)
		for e := 1; e <= int(total); e++ {
			id := sid*10000 + int64(n)*100 + int64(e)
			ep := mockEpisode{ID: id, SeriesID: sid, SeasonNumber: n, EpisodeNumber: e,
				Title: "Épisode " + strconv.Itoa(e), AirDateUTC: "2020-01-01T00:00:00Z", Monitored: monitored}
			if e <= int(files) {
				ep.HasFile, ep.EpisodeFileID = true, id
				sizes[id] = int64(size / files)
			}
			out = append(out, ep)
		}
	}
	return out, sizes
}

// arrItems adds item, episode and write endpoints. Writes are recorded (see
// wrap) and answered with success; the fixtures do not change.
func (m *Mock) arrItems(mux *http.ServeMux, service string) {
	resource := map[string]string{"sonarr": "series", "radarr": "movie"}[service]
	mux.HandleFunc("GET /api/v3/"+resource+"/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		it, ok := m.item(service, id)
		if !ok {
			http.Error(w, `{"message":"NotFound"}`, http.StatusNotFound)
			return
		}
		writeJSON(w, it)
	})
	mux.HandleFunc("DELETE /api/v3/"+resource+"/{id}", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{})
	})
	mux.HandleFunc("PUT /api/v3/"+resource+"/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		writeJSON(w, map[string]any{"id": r.PathValue("id")})
	})
	// New tags get ids after the fixture ones.
	mux.HandleFunc("POST /api/v3/tag", func(w http.ResponseWriter, r *http.Request) {
		var t struct {
			Label string `json:"label"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&t)
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, map[string]any{"id": 100, "label": t.Label})
	})
	if service != "sonarr" {
		return
	}
	mux.HandleFunc("GET /api/v3/episode", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.URL.Query().Get("seriesId"), 10, 64)
		it, ok := m.item(service, id)
		if !ok {
			writeJSON(w, []any{})
			return
		}
		eps, _ := episodes(it)
		writeJSON(w, eps)
	})
	mux.HandleFunc("GET /api/v3/episodefile", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.URL.Query().Get("seriesId"), 10, 64)
		it, ok := m.item(service, id)
		files := []map[string]any{}
		if ok {
			eps, sizes := episodes(it)
			for _, e := range eps {
				if e.HasFile {
					files = append(files, map[string]any{"id": e.EpisodeFileID, "seriesId": e.SeriesID,
						"seasonNumber": e.SeasonNumber, "size": sizes[e.ID],
						"relativePath": "Season " + strconv.Itoa(e.SeasonNumber) + "/E" + strconv.Itoa(e.EpisodeNumber) + ".mkv"})
				}
			}
		}
		writeJSON(w, files)
	})
	mux.HandleFunc("DELETE /api/v3/episodefile/bulk", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{})
	})
}

// todayShift moves the anchor to today (UTC): the calendars and the indexer
// failures are about now, whatever ShiftSeconds says.
func todayShift() time.Duration {
	return time.Now().UTC().Truncate(24 * time.Hour).Sub(time.Unix(Anchor, 0).UTC())
}

// calendarDates are the date fields of the calendar fixtures.
var calendarDates = map[string][]string{"sonarr": {"airDateUtc"}, "radarr": {"inCinemas", "digitalRelease", "physicalRelease"}}

// calendar serves {service}/calendar.json with its dates moved so that the
// anchor is today (a calendar of the past shows nothing, so this does not
// depend on ShiftSeconds), keeping the entries with a date in [start, end).
func (m *Mock) calendar(w http.ResponseWriter, r *http.Request, service string) {
	var rows []map[string]any
	if err := m.readJSON(service+"/calendar.json", &rows); err != nil {
		writeJSON(w, []any{})
		return
	}
	start, errS := time.Parse(time.RFC3339, r.URL.Query().Get("start"))
	end, errE := time.Parse(time.RFC3339, r.URL.Query().Get("end"))
	shift := todayShift()
	out := []map[string]any{}
	for _, row := range rows {
		in := false
		for _, k := range calendarDates[service] {
			v, _ := row[k].(string)
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				continue
			}
			t = t.Add(shift)
			row[k] = t.Format(time.RFC3339)
			if k == "airDateUtc" {
				row["airDate"] = t.Format(time.DateOnly)
			}
			in = in || ((errS != nil || !t.Before(start)) && (errE != nil || t.Before(end)))
		}
		if in {
			out = append(out, row)
		}
	}
	writeJSON(w, out)
}

// realPoster downloads the poster of key ("sonarr/3") listed in posters.json,
// once; ok is false when there is none or the download failed.
func (m *Mock) realPoster(key string) (poster, bool) {
	m.mu.Lock()
	p, ok := m.posters[key]
	m.mu.Unlock()
	if ok {
		return p, p.body != nil
	}
	var urls map[string]string
	_ = m.readJSON("posters.json", &urls)
	p = poster{}
	if u := urls[key]; strings.HasPrefix(u, "https://") {
		if b, ct, err := download(u); err == nil {
			p = poster{body: b, contentType: ct}
		}
	}
	m.mu.Lock()
	m.posters[key] = p // failures too: no retry storm
	m.mu.Unlock()
	return p, p.body != nil
}

func download(u string) ([]byte, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", "hangarr-mockarr/1.0")
	res, err := (&http.Client{Timeout: 20 * time.Second}).Do(req) //nolint:gosec // G107: fixed URLs from posters.json, dev only
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = res.Body.Close() }()
	ct := res.Header.Get("Content-Type")
	if res.StatusCode != http.StatusOK || !strings.HasPrefix(ct, "image/") {
		return nil, "", fmt.Errorf("poster %s: %d %s", u, res.StatusCode, ct)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, 5<<20))
	return b, ct, err
}
