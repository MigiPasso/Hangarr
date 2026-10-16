// Package images proxies posters and fanarts from Sonarr/Radarr MediaCover
// or from Plex or Jellyfin for items they alone know, with a disk
// cache, so the browser never talks to the upstreams and API keys stay server
// side.
package images

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"hangarr/internal/clients/httpx"
)

// Tunables.
const (
	MaxConcurrentFetches = 4                   // upstream requests in flight
	MaxImageBytes        = 10 << 20            // larger responses are rejected
	RefreshAfter         = 30 * 24 * time.Hour // cached files older than this are refetched
	MissTTL              = time.Hour           // how long a missing image is not retried
	BrowserMaxAge        = 7 * 24 * time.Hour  // Cache-Control for images
	fetchTimeout         = 20 * time.Second
	// cacheVersion is part of the cached file names: files of an older
	// version (full-size images) are removed by New.
	cacheVersion = "v2"
)

var errNoImage = errors.New("no image available")

// validID matches the ids that can name a cached file: Sonarr/Radarr ids,
// Plex rating keys and Jellyfin ids (lowercase hexadecimal, never a path).
var validID = regexp.MustCompile(`^[0-9a-f]{1,64}$`)

// Info describes the media whose image is requested.
type Info struct {
	Title     string
	RemoteURL string // fallback when MediaCover has nothing
}

// Lookup returns Info for (kind, id, variant); id is a Sonarr/Radarr id, a
// Plex rating key or a Jellyfin id. ok is false when the media is unknown.
type Lookup func(ctx context.Context, kind, id, variant string) (info Info, ok bool, err error)

// Proxy serves /img/{kind}/{id}/{variant}.
type Proxy struct {
	Dir string // DATA_DIR/cache/img
	// Client returns the client of the kind: Sonarr ("series"), Radarr
	// ("movie"), Plex ("plex") or Jellyfin ("jellyfin"), nil when the service
	// is not configured.
	Client func(kind string) *httpx.Client
	Remote *http.Client // for remoteUrl fallbacks
	Lookup Lookup
	Log    *slog.Logger
	Now    func() time.Time

	sem      chan struct{}
	decoding chan struct{} // one image decoded at a time (shrink)
	group    singleflight.Group
	mu       sync.Mutex
	misses   map[string]time.Time
}

// New builds a proxy caching into dir.
func New(dir string, client func(kind string) *httpx.Client, lookup Lookup, log *slog.Logger) *Proxy {
	p := &Proxy{
		Dir: dir, Client: client, Lookup: lookup, Log: log,
		Remote:   &http.Client{Timeout: fetchTimeout, CheckRedirect: httpsOnly},
		sem:      make(chan struct{}, MaxConcurrentFetches),
		decoding: make(chan struct{}, 1),
		misses:   map[string]time.Time{},
	}
	p.pruneOldVersions()
	return p
}

// pruneOldVersions removes the cached files of an older cacheVersion.
func (p *Proxy) pruneOldVersions() {
	entries, err := os.ReadDir(p.Dir)
	if err != nil {
		return // no cache yet
	}
	n := 0
	for _, e := range entries {
		if name := e.Name(); strings.HasSuffix(name, ".img") && !strings.HasSuffix(name, "-"+cacheVersion+".img") {
			if os.Remove(filepath.Join(p.Dir, name)) == nil {
				n++
			}
		}
	}
	if n > 0 && p.Log != nil {
		p.Log.Info("image cache: removed files of an older version", "count", n)
	}
}

func (p *Proxy) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// upstreamRequests lists the requests to try, smallest suitable first:
// MediaCover files for Sonarr/Radarr; for Plex ("plex" kind, id = rating
// key), the photo transcoder (resized), then the original image; for Jellyfin
// ("jellyfin" kind, id = item id), its image endpoint, which resizes itself.
func upstreamRequests(kind, id, variant string) []httpx.Request {
	if kind == "jellyfin" {
		req := httpx.Request{Path: "/Items/" + id + "/Images/Primary",
			Query: url.Values{"maxWidth": {"500"}, "quality": {"90"}}}
		if variant == "fanart" {
			req = httpx.Request{Path: "/Items/" + id + "/Images/Backdrop/0",
				Query: url.Values{"maxWidth": {"1280"}, "quality": {"90"}}}
		}
		return []httpx.Request{req}
	}
	if kind == "plex" {
		src := "/library/metadata/" + id + "/thumb"
		width, height := "500", "750"
		if variant == "fanart" {
			src = "/library/metadata/" + id + "/art"
			width, height = "1280", "720"
		}
		return []httpx.Request{
			{Path: "/photo/:/transcode", Query: url.Values{"url": {src}, "width": {width}, "height": {height},
				"minSize": {"1"}, "upscale": {"1"}}},
			{Path: src},
		}
	}
	files := []string{"fanart.jpg"}
	if variant == "poster" {
		files = []string{"poster-500.jpg", "poster.jpg"}
	}
	out := make([]httpx.Request, 0, len(files))
	for _, f := range files {
		out = append(out, httpx.Request{Path: "/api/v3/mediacover/" + id + "/" + f})
	}
	return out
}

// ServeImage handles a request; kind is "series", "movie", "plex" (id is then
// the Plex rating key) or "jellyfin" (id is then the Jellyfin item id), variant
// "poster" or "fanart". Validation of the parameters is the caller's job; an
// id that is not lowercase hexadecimal is refused anyway, as it names a file.
func (p *Proxy) ServeImage(w http.ResponseWriter, r *http.Request, kind, id, variant string) {
	if !validID.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	key := fmt.Sprintf("%s-%s-%s-%s", kind, id, variant, cacheVersion)
	path := filepath.Join(p.Dir, key+".img")

	if st, err := os.Stat(path); err == nil && p.now().Sub(st.ModTime()) < RefreshAfter {
		p.serveFile(w, r, path)
		return
	}
	info, ok, err := p.Lookup(r.Context(), kind, id, variant)
	if err != nil {
		http.Error(w, "Internal error.", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	if !p.recentMiss(key) {
		_, err, _ = p.group.Do(key, func() (any, error) {
			// Another flight may have just filled the cache.
			if st, err := os.Stat(path); err == nil && p.now().Sub(st.ModTime()) < RefreshAfter {
				return nil, nil
			}
			ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), fetchTimeout)
			defer cancel()
			return nil, p.fetch(ctx, kind, id, variant, info, path)
		})
		if err != nil {
			p.markMiss(key)
			if !errors.Is(err, errNoImage) {
				p.Log.Warn("image fetch failed", "kind", kind, "id", id, "variant", variant, "err", err)
			}
		}
	}
	if _, err := os.Stat(path); err == nil {
		p.serveFile(w, r, path) // fresh, or stale if the refresh failed
		return
	}
	if variant == "fanart" {
		// Transparent: the detail page shows the blurred poster behind it.
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", fmt.Sprintf("private, max-age=%d", int(MissTTL.Seconds())))
		_, _ = w.Write([]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 9"/>`))
		return
	}
	servePlaceholder(w, info.Title)
}

func (p *Proxy) recentMiss(key string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	t, ok := p.misses[key]
	return ok && p.now().Sub(t) < MissTTL
}

func (p *Proxy) markMiss(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.misses) > 10_000 {
		p.misses = map[string]time.Time{}
	}
	p.misses[key] = p.now()
}

func (p *Proxy) fetch(ctx context.Context, kind, id, variant string, info Info, path string) error {
	select {
	case p.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-p.sem }()

	client := p.Client(kind)
	lastErr := errNoImage
	if client != nil {
		for _, req := range upstreamRequests(kind, id, variant) {
			req.MaxBytes = MaxImageBytes // stop reading a larger answer right away
			b, err := client.Do(ctx, req)
			if err == nil && isImage(b) {
				if b, err = p.shrink(b, maxWidth(variant)); err == nil {
					return p.write(path, b)
				}
			}
			var se *httpx.StatusError
			if err != nil && (!errors.As(err, &se) || se.Status != http.StatusNotFound) {
				lastErr = err
			}
		}
	}
	b, err := p.fetchRemote(ctx, info.RemoteURL)
	if err == nil {
		b, err = p.shrink(b, maxWidth(variant))
	}
	if err == nil {
		return p.write(path, b)
	} else if !errors.Is(err, errNoImage) {
		lastErr = err
	}
	return lastErr
}

// httpsOnly is the redirect policy of remote image downloads: at most 3
// redirects, all https (no downgrade to a plain-HTTP or internal service).
func httpsOnly(req *http.Request, via []*http.Request) error {
	if len(via) >= 3 {
		return errors.New("too many redirects")
	}
	if req.URL.Scheme != "https" {
		return errors.New("redirect to a non-https URL refused")
	}
	return nil
}

// fetchRemote downloads the *arr remoteUrl (TVDB/TMDB CDN), https only.
func (p *Proxy) fetchRemote(ctx context.Context, raw string) ([]byte, error) {
	u, err := url.Parse(raw)
	if raw == "" || err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, errNoImage
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, errNoImage
	}
	res, err := p.Remote.Do(req) //nolint:gosec // G704: https URL provided by Sonarr/Radarr metadata
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // drop the URL from the message
		}
		return nil, fmt.Errorf("remote image: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return nil, errNoImage
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, MaxImageBytes+1))
	if err != nil || len(b) > MaxImageBytes || !isImage(b) {
		return nil, errNoImage
	}
	return b, nil
}

func isImage(b []byte) bool {
	return len(b) > 0 && len(b) <= MaxImageBytes && strings.HasPrefix(http.DetectContentType(b), "image/")
}

// write stores the image atomically (temp file + rename).
func (p *Proxy) write(path string, b []byte) error {
	if err := os.MkdirAll(p.Dir, 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(p.Dir, ".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func (p *Proxy) serveFile(w http.ResponseWriter, r *http.Request, path string) {
	f, err := os.Open(path) //nolint:gosec // G304: cache dir + kind/variant allowlisted by the web handler, id checked by validID
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	w.Header().Set("Content-Type", http.DetectContentType(head[:n]))
	w.Header().Set("Cache-Control", fmt.Sprintf("private, max-age=%d", int(BrowserMaxAge.Seconds())))
	w.Header().Set("ETag", fmt.Sprintf(`"%x-%x"`, st.Size(), st.ModTime().Unix()))
	http.ServeContent(w, r, "", st.ModTime(), f)
}

// servePlaceholder renders a neutral SVG poster with the title.
func servePlaceholder(w http.ResponseWriter, title string) {
	var lines []string
	line := ""
	for _, word := range strings.Fields(title) {
		if line != "" && len([]rune(line+" "+word)) > 14 {
			lines = append(lines, line)
			line = ""
		}
		line = strings.TrimSpace(line + " " + word)
	}
	if line != "" {
		lines = append(lines, line)
	}
	if len(lines) > 4 {
		lines = append(lines[:3], "…")
	}
	var b bytes.Buffer
	b.WriteString(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 200 300" width="200" height="300">` +
		`<rect width="200" height="300" fill="#2a2d33"/>` +
		`<text x="100" text-anchor="middle" font-family="system-ui,sans-serif" font-size="18" fill="#9aa0aa">`)
	y := 150 - (len(lines)-1)*12
	for i, l := range lines {
		fmt.Fprintf(&b, `<tspan x="100" y="%d">`, y+i*24)
		_ = xml.EscapeText(&b, []byte(l))
		b.WriteString(`</tspan>`)
	}
	b.WriteString(`</text></svg>`)
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", fmt.Sprintf("private, max-age=%d", int(MissTTL.Seconds())))
	_, _ = w.Write(b.Bytes())
}
