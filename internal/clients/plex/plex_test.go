package plex

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

const duneMetadata = `{"MediaContainer":{"Metadata":[{"ratingKey":"6894","title":"Dune: Part One","year":2021,"Media":[{"id":11195,"videoResolution":"4k","videoCodec":"hevc","audioCodec":"truehd","container":"mkv","bitrate":10960,"Part":[{"id":17402,"file":"/m/Dune (2021) MULTi 2160p.mkv","size":12777212193,"Stream":[{"streamType":1,"codec":"hevc","languageCode":"eng","displayTitle":"4K DoVi/HDR10"},{"streamType":2,"codec":"dca","languageCode":"fra","displayTitle":"Français (DTS-HD MA 5.1)"},{"streamType":3,"codec":"srt","languageCode":"fra","displayTitle":"Français Forced","forced":true}]}]},{"id":11205,"videoResolution":"4k","videoCodec":"hevc","audioCodec":"truehd","container":"mkv","bitrate":67272,"Part":[{"id":17403,"file":"/m/Dune 2021 REMUX.mkv","size":78427175765}]}]}]}}`

// newTestClient serves h and returns a client talking to it.
func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return New(ts.URL, "k")
}

func TestPlexMetadata(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/library/metadata/6894" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("X-Plex-Token") == "" {
			t.Error("X-Plex-Token missing")
		}
		_, _ = w.Write([]byte(duneMetadata))
	})
	item, err := c.Metadata(context.Background(), "6894")
	if err != nil {
		t.Fatal(err)
	}
	if len(item.Media) != 2 || item.Media[0].ID != 11195 || item.Media[1].ID != 11205 {
		t.Fatalf("media = %+v", item.Media)
	}
	if size := item.Media[1].Part[0].Size; size != 78427175765 {
		t.Errorf("size = %d", size)
	}
	streams := item.Media[0].Part[0].Stream
	if len(streams) != 3 {
		t.Fatalf("streams = %d, want 3", len(streams))
	}
	if sub := streams[2]; sub.StreamType != StreamSubtitle || !sub.Forced {
		t.Errorf("subtitle = %+v", sub)
	}
	want := []string{"/m/Dune (2021) MULTi 2160p.mkv", "/m/Dune 2021 REMUX.mkv"}
	if got := item.Files(); !slices.Equal(got, want) {
		t.Errorf("files = %q, want %q", got, want)
	}
}

func TestPlexMetadataEmpty(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"MediaContainer":{"Metadata":[]}}`))
	})
	if _, err := c.Metadata(context.Background(), "6894"); err == nil {
		t.Error("want an error for empty metadata")
	}
}

func TestPlexDeleteMedia(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/library/metadata/6894/media/11205" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("X-Plex-Token") == "" {
			t.Error("X-Plex-Token missing")
		}
	})
	if err := c.DeleteMedia(context.Background(), "6894", "11205"); err != nil {
		t.Fatal(err)
	}
}
