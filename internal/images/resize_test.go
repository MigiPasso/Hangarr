package images

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"hangarr/internal/clients/httpx"
)

// photo is a w×h image with a gradient (compresses like a photo, not like a
// flat color).
func photo(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.SetRGBA(x, y, color.RGBA{R: uint8(x * 255 / w), G: uint8(y * 255 / h), B: uint8((x ^ y) & 0xff), A: 255}) //nolint:gosec // G115: test pattern
		}
	}
	return img
}

func encodeJPEG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func size(t *testing.T, b []byte) (int, int, string) {
	t.Helper()
	cfg, format, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Width, cfg.Height, format
}

func TestShrink(t *testing.T) {
	big := encodeJPEG(t, photo(3840, 2160))
	out, err := shrink(big, MaxFanartWidth)
	if err != nil {
		t.Fatal(err)
	}
	if w, h, f := size(t, out); w != 1280 || h != 720 || f != "jpeg" || len(out) >= len(big) {
		t.Errorf("4K fanart: %dx%d %s, %d → %d bytes", w, h, f, len(big), len(out))
	}

	small := encodeJPEG(t, photo(400, 600))
	if out, _ := shrink(small, MaxPosterWidth); !bytes.Equal(out, small) {
		t.Error("an image already narrow enough is kept as is")
	}

	var p bytes.Buffer
	if err := png.Encode(&p, photo(1000, 1500)); err != nil {
		t.Fatal(err)
	}
	if out, _ := shrink(p.Bytes(), MaxPosterWidth); func() bool { w, h, f := size(t, out); return w != 500 || h != 750 || f != "jpeg" }() {
		t.Error("PNG poster: 500×750 JPEG expected")
	}

	webp := []byte("RIFF\x00\x00\x00\x00WEBPVP8 ")
	if out, err := shrink(webp, MaxPosterWidth); err != nil || !bytes.Equal(out, webp) {
		t.Error("an undecodable format is served as is")
	}
	if _, err := shrink(bombPNG(t, 20000, 20000), MaxFanartWidth); !errors.Is(err, errTooLarge) {
		t.Errorf("decompression bomb: %v", err)
	}
	// 30 Mpx: above the limit; 4K (8.3 Mpx) stays well under it.
	if _, err := shrink(bombPNG(t, 7500, 4000), MaxFanartWidth); !errors.Is(err, errTooLarge) {
		t.Errorf("30 Mpx must be refused: %v", err)
	}
}

// bombPNG is a tiny PNG declaring w×h pixels (only its header is valid).
func bombPNG(t *testing.T, w, h uint32) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewGray(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	raw := b.Bytes()
	// Signature (8), IHDR length (4) and type (4), then width and height.
	binary.BigEndian.PutUint32(raw[16:], w)
	binary.BigEndian.PutUint32(raw[20:], h)
	binary.BigEndian.PutUint32(raw[29:], crc32.ChecksumIEEE(raw[12:29]))
	return raw
}

func TestDownscaleAverages(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 4, 2))
	for x := range 4 {
		v := uint8(0)
		if x%2 == 1 {
			v = 200
		}
		src.SetRGBA(x, 0, color.RGBA{v, v, v, 255})
		src.SetRGBA(x, 1, color.RGBA{v, v, v, 255})
	}
	dst := downscale(src, 2, 1)
	if c := dst.RGBAAt(0, 0); c.R != 100 || c.A != 255 {
		t.Errorf("box average: %+v", c)
	}
}

// The proxy caches the reduced fanart, and New removes the cache files of
// the previous version.
func TestProxyCachesReducedFanart(t *testing.T) {
	big := encodeJPEG(t, photo(1920, 1080))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(big)
	}))
	defer upstream.Close()
	dir := t.TempDir()
	old := filepath.Join(dir, "series-1-fanart.img")
	if err := os.WriteFile(old, big, 0o600); err != nil {
		t.Fatal(err)
	}
	client := httpx.New("sonarr", upstream.URL, nil)
	lookup := func(context.Context, string, int64, string) (Info, bool, error) { return Info{Title: "X"}, true, nil }
	p := New(dir, func(string) *httpx.Client { return client }, lookup, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("files of the previous cache version must be removed")
	}
	rec := httptest.NewRecorder()
	p.ServeImage(rec, httptest.NewRequest(http.MethodGet, "/img/series/1/fanart", nil), "series", 1, "fanart")
	if w, h, _ := size(t, rec.Body.Bytes()); rec.Code != http.StatusOK || w != 1280 || h != 720 {
		t.Fatalf("served fanart: %d, %dx%d", rec.Code, w, h)
	}
	cached, err := os.ReadFile(filepath.Join(dir, "series-1-fanart-"+cacheVersion+".img"))
	if err != nil || len(cached) >= len(big) {
		t.Errorf("cache: %d bytes (source %d), %v", len(cached), len(big), err)
	}
}
