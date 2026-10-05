// Package mockarr impersonates Sonarr, Radarr, Plex and Tautulli
// from fixture files (see testdata/README.md). It backs `make dev`, the e2e
// tests and the sync integration tests. Write calls (DELETE, PUT, POST) are
// recorded and exposed on GET /_calls; POST /_reset clears them.
package mockarr

import (
	"bytes"
	"encoding/json"
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

	mu    sync.Mutex
	calls []Call
	hits  map[string]int
}

// New returns a mock serving fixtures from fsys.
func New(fsys fs.FS, apiKey string) *Mock {
	return &Mock{Fixtures: fsys, APIKey: apiKey, hits: map[string]int{}}
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
	mux.HandleFunc("GET /api/v3/mediacover/{id}/{file}", func(w http.ResponseWriter, r *http.Request) {
		m.hit(service + ":mediacover")
		m.mediaCover(w, r, service)
	})
	for _, res := range []string{"series", "movie", "tag", "qualityprofile", "diskspace", "health"} {
		mux.HandleFunc("GET /api/v3/"+res, func(w http.ResponseWriter, _ *http.Request) {
			m.hit(service + ":" + res)
			m.file(w, service+"/"+res+".json")
		})
	}
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
	file := "series"
	if service == "radarr" {
		file = "movie"
	}
	var items []struct {
		ID     int64 `json:"id"`
		Images []struct {
			CoverType string `json:"coverType"`
		} `json:"images"`
	}
	if err := m.readJSON(service+"/"+file+".json", &items); err != nil {
		http.NotFound(w, r)
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	cover, _, _ := strings.Cut(r.PathValue("file"), ".")
	cover, _, _ = strings.Cut(cover, "-") // poster-500 → poster
	for _, it := range items {
		if it.ID != id {
			continue
		}
		for _, img := range it.Images {
			if img.CoverType == cover {
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
func (m *Mock) item(service string, id int64) (map[string]any, bool) {
	file := map[string]string{"sonarr": "sonarr/series.json", "radarr": "radarr/movie.json"}[service]
	var items []map[string]any
	if err := m.readJSON(file, &items); err != nil {
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
