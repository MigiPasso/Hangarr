// Package images proxies posters and fanarts from Sonarr/Radarr MediaCover
// (SPEC §8), or from Plex for items it alone knows, with a disk cache, so
// the browser never talks to the upstreams and API keys stay server side.
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
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"cleanarr/internal/clients/httpx"
)

// Tunables.
const (
	MaxConcurrentFetches = 4                   // upstream requests in flight
	MaxImageBytes        = 10 << 20            // larger responses are rejected
	RefreshAfter         = 30 * 24 * time.Hour // cached files older than this are refetched
	MissTTL              = time.Hour           // how long a missing image is not retried
	BrowserMaxAge        = 7 * 24 * time.Hour  // Cache-Control for images
	fetchTimeout         = 20 * time.Second
)

var errNoImage = errors.New("no image available")

// Info describes the media whose image is requested.
type Info struct {
	Title     string
	RemoteURL string // fallback when MediaCover has nothing
}

// Lookup returns Info for (kind, arrID, variant); ok is false when the media
// is unknown.
type Lookup func(ctx context.Context, kind string, arrID int64, variant string) (info Info, ok bool, err error)

// Proxy serves /img/{kind}/{arrId}/{variant}.
type Proxy struct {
	Dir string // DATA_DIR/cache/img
	// Client returns the Sonarr ("series") or Radarr ("movie") client, nil
	// when the service is not configured.
	Client func(kind string) *httpx.Client
	Remote *http.Client // for remoteUrl fallbacks
	Lookup Lookup
	Log    *slog.Logger
	Now    func() time.Time

	sem    chan struct{}
	group  singleflight.Group
	mu     sync.Mutex
	misses map[string]time.Time
}

// New builds a proxy caching into dir.
func New(dir string, client func(kind string) *httpx.Client, lookup Lookup, log *slog.Logger) *Proxy {
	return &Proxy{
		Dir: dir, Client: client, Lookup: lookup, Log: log,
		Remote: &http.Client{Timeout: fetchTimeout, CheckRedirect: httpsOnly},
		sem:    make(chan struct{}, MaxConcurrentFetches),
		misses: map[string]time.Time{},
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
// key), the photo transcoder (resized), then the original image.
func upstreamRequests(kind string, id int64, variant string) []httpx.Request {
	if kind == "plex" {
		src := "/library/metadata/" + strconv.FormatInt(id, 10) + "/thumb"
		width, height := "500", "750"
		if variant == "fanart" {
			src = "/library/metadata/" + strconv.FormatInt(id, 10) + "/art"
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
		out = append(out, httpx.Request{Path: "/api/v3/mediacover/" + strconv.FormatInt(id, 10) + "/" + f})
	}
	return out
}

// ServeImage handles a request; kind is "series", "movie" or "plex" (arrID
// is then the Plex rating key), variant "poster" or "fanart". Validation of
// the parameters is the caller's job.
func (p *Proxy) ServeImage(w http.ResponseWriter, r *http.Request, kind string, arrID int64, variant string) {
	key := fmt.Sprintf("%s-%d-%s", kind, arrID, variant)
	path := filepath.Join(p.Dir, key+".img")

	if st, err := os.Stat(path); err == nil && p.now().Sub(st.ModTime()) < RefreshAfter {
		p.serveFile(w, r, path)
		return
	}
	info, ok, err := p.Lookup(r.Context(), kind, arrID, variant)
	if err != nil {
		http.Error(w, "Erreur interne", http.StatusInternalServerError)
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
			return nil, p.fetch(ctx, kind, arrID, variant, info, path)
		})
		if err != nil {
			p.markMiss(key)
			if !errors.Is(err, errNoImage) {
				p.Log.Warn("image fetch failed", "kind", kind, "arr_id", arrID, "variant", variant, "err", err)
			}
		}
	}
	if _, err := os.Stat(path); err == nil {
		p.serveFile(w, r, path) // fresh, or stale if the refresh failed
		return
	}
	if variant == "fanart" {
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(MissTTL.Seconds())))
		_, _ = w.Write([]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 9"><rect width="16" height="9" fill="#2a2d33"/></svg>`))
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

func (p *Proxy) fetch(ctx context.Context, kind string, arrID int64, variant string, info Info, path string) error {
	select {
	case p.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-p.sem }()

	client := p.Client(kind)
	lastErr := errNoImage
	if client != nil {
		for _, req := range upstreamRequests(kind, arrID, variant) {
			b, err := client.Do(ctx, req)
			if err == nil && isImage(b) {
				return p.write(path, b)
			}
			var se *httpx.StatusError
			if err != nil && (!errors.As(err, &se) || se.Status != http.StatusNotFound) {
				lastErr = err
			}
		}
	}
	if b, err := p.fetchRemote(ctx, info.RemoteURL); err == nil {
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
	f, err := os.Open(path)
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
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(BrowserMaxAge.Seconds())))
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
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(MissTTL.Seconds())))
	_, _ = w.Write(b.Bytes())
}
