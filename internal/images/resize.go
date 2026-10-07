package images

import (
	"bytes"
	"errors"
	"image"
	"image/draw"
	"image/jpeg"
	_ "image/png" // Plex and the CDNs may answer PNG
)

// Display sizes: wider images are downscaled before they are cached, so the
// cache holds what the pages show (fanarts are 1920 or 3840 px wide at the
// source, 1280 px is plenty for the detail page).
const (
	MaxPosterWidth = 500
	MaxFanartWidth = 1280
	// maxPixels refuses to decode larger images: a small file may declare
	// huge dimensions (decompression bomb). 25 Mpx (6K) is far above real
	// posters and fanarts (8 Mpx for 4K) and needs about 140 MB to decode.
	maxPixels   = 25_000_000
	jpegQuality = 85
)

var errTooLarge = errors.New("image dimensions too large")

// decoding serializes the decode-and-resize step: one full-size image in
// memory at a time, whatever the number of downloads in flight, so the pod
// stays well under its 512 MiB limit.
var decoding = make(chan struct{}, 1)

func maxWidth(variant string) int {
	if variant == "fanart" {
		return MaxFanartWidth
	}
	return MaxPosterWidth
}

// shrink returns b downscaled to width w (as JPEG). b is returned unchanged
// when it is narrow enough, in a format the standard library does not decode
// (it is still served), or when the result would not be smaller.
func shrink(b []byte, w int) ([]byte, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return b, nil //nolint:nilerr // unknown format (WebP…): served as is
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > maxPixels/max(cfg.Height, 1) {
		return nil, errTooLarge
	}
	if cfg.Width <= w {
		return b, nil
	}
	decoding <- struct{}{}
	defer func() { <-decoding }()
	src, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		return b, nil //nolint:nilerr // truncated or odd file: served as is, like before
	}
	h := max(1, (cfg.Height*w+cfg.Width/2)/cfg.Width)
	var out bytes.Buffer
	if err := jpeg.Encode(&out, downscale(src, w, h), &jpeg.Options{Quality: jpegQuality}); err != nil || out.Len() >= len(b) {
		return b, nil //nolint:nilerr // keep the original when re-encoding does not help
	}
	return out.Bytes(), nil
}

// downscale resizes src to w×h by averaging the source pixels covered by each
// destination pixel (box filter): sharp and alias-free when shrinking, every
// source pixel read once.
func downscale(src image.Image, w, h int) *image.RGBA {
	b := src.Bounds()
	rgba, ok := src.(*image.RGBA)
	if !ok || rgba.Rect.Min != (image.Point{}) {
		rgba = image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
		draw.Draw(rgba, rgba.Rect, src, b.Min, draw.Src) // fast path for JPEG's YCbCr
	}
	sw, sh := rgba.Rect.Dx(), rgba.Rect.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		y0, y1 := y*sh/h, max((y+1)*sh/h, y*sh/h+1)
		for x := range w {
			x0, x1 := x*sw/w, max((x+1)*sw/w, x*sw/w+1)
			var r, g, bl, a, n uint32
			for sy := y0; sy < y1; sy++ {
				row := rgba.Pix[sy*rgba.Stride:]
				for sx := x0; sx < x1; sx++ {
					p := row[sx*4 : sx*4+4 : sx*4+4]
					r, g, bl, a = r+uint32(p[0]), g+uint32(p[1]), bl+uint32(p[2]), a+uint32(p[3])
					n++
				}
			}
			d := dst.Pix[y*dst.Stride+x*4 : y*dst.Stride+x*4+4 : y*dst.Stride+x*4+4]
			d[0], d[1], d[2], d[3] = uint8(r/n), uint8(g/n), uint8(bl/n), uint8(a/n) //nolint:gosec // G115: averages of bytes
		}
	}
	return dst
}
