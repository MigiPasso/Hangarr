package mockarr

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestDevExtras(t *testing.T) {
	m := New(os.DirFS("../../testdata"), "k")
	base, err := m.catalog("sonarr")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.item("sonarr", 10); ok {
		t.Error("extras must stay out of the synthetic catalog")
	}
	m.DevExtras = true
	for _, svc := range []string{"sonarr", "radarr"} {
		all, err := m.catalog(svc)
		if err != nil {
			t.Fatal(err)
		}
		ids := map[float64]bool{}
		for _, it := range all {
			id, _ := it["id"].(float64)
			if ids[id] {
				t.Errorf("%s: duplicate id %v", svc, id)
			}
			ids[id] = true
		}
	}
	all, _ := m.catalog("sonarr")
	if len(all) <= len(base) {
		t.Errorf("extras: %d series, base %d", len(all), len(base))
	}
	if it, ok := m.item("sonarr", 10); !ok || it["title"] != "The Last of Us" {
		t.Errorf("extra series by id: %v %v", ok, it["title"])
	}
}

// Fake Jellyfin: the Authorization header of the mock (key "k") and the id of
// alice, who has played and in-progress items.
const (
	jellyfinAuth  = `MediaBrowser Token="k"`
	jellyfinAlice = "aaaa0000000000000000000000001001"
)

// jellyfinCall sends one request to the fake Jellyfin, with the Authorization
// header auth (none when empty).
func jellyfinCall(h http.Handler, method, target, auth string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// decodeJellyfinListing decodes a 200 response of GET /Items.
func decodeJellyfinListing(t *testing.T, rec *httptest.ResponseRecorder) jellyfinListing {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var list jellyfinListing
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	return list
}

func TestJellyfinAuth(t *testing.T) {
	h := New(os.DirFS("../../testdata"), "k").Jellyfin()
	if rec := jellyfinCall(h, http.MethodGet, "/System/Info", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no header: status %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/System/Info", nil)
	req.Header.Set("X-Emby-Token", "k")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("X-Emby-Token only: status %d", rec.Code)
	}
	rec = jellyfinCall(h, http.MethodGet, "/System/Info", jellyfinAuth)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid token: status %d", rec.Code)
	}
	var info map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info["Version"] != "12.0.1-mock" {
		t.Errorf("version: %v", info["Version"])
	}
}

func TestJellyfinItems(t *testing.T) {
	h := New(os.DirFS("../../testdata"), "k").Jellyfin()
	list := decodeJellyfinListing(t, jellyfinCall(h, http.MethodGet, "/Items?includeItemTypes=Movie", jellyfinAuth))
	if len(list.Items) != 5 || list.TotalRecordCount != 5 {
		t.Errorf("movies: %d items, total %d", len(list.Items), list.TotalRecordCount)
	}
	list = decodeJellyfinListing(t, jellyfinCall(h, http.MethodGet, "/Items?includeItemTypes=Movie&startIndex=2&limit=2", jellyfinAuth))
	if len(list.Items) != 2 || list.TotalRecordCount != 5 || list.StartIndex != 2 {
		t.Fatalf("page: %d items, total %d, start %d", len(list.Items), list.TotalRecordCount, list.StartIndex)
	}
	if list.Items[0]["Name"] != "Tenet" || list.Items[1]["Name"] != "Interstellar" {
		t.Errorf("page: %v, %v", list.Items[0]["Name"], list.Items[1]["Name"])
	}
	if rec := jellyfinCall(h, http.MethodGet, "/Items?includeItemTypes=Episode", jellyfinAuth); rec.Code != http.StatusBadRequest {
		t.Errorf("Episode: status %d", rec.Code)
	}
}

func TestJellyfinUserItems(t *testing.T) {
	h := New(os.DirFS("../../testdata"), "k").Jellyfin()
	played := decodeJellyfinListing(t, jellyfinCall(h, http.MethodGet, "/Items?userId="+jellyfinAlice+"&isPlayed=true", jellyfinAuth))
	if len(played.Items) != 11 {
		t.Errorf("alice played: %d items", len(played.Items))
	}
	resumable := decodeJellyfinListing(t, jellyfinCall(h, http.MethodGet, "/Items?userId="+jellyfinAlice+"&filters=IsResumable", jellyfinAuth))
	if len(resumable.Items) != 3 {
		t.Errorf("alice resumable: %d items", len(resumable.Items))
	}
	unknown := decodeJellyfinListing(t, jellyfinCall(h, http.MethodGet, "/Items?userId=aaaa0000000000000000000000009999&isPlayed=true", jellyfinAuth))
	if len(unknown.Items) != 0 || unknown.TotalRecordCount != 0 {
		t.Errorf("unknown user: %d items, total %d", len(unknown.Items), unknown.TotalRecordCount)
	}
	if rec := jellyfinCall(h, http.MethodGet, "/Items?userId=../x&isPlayed=true", jellyfinAuth); rec.Code != http.StatusBadRequest {
		t.Errorf("path in userId: status %d", rec.Code)
	}
}

func TestJellyfinShiftDates(t *testing.T) {
	m := New(os.DirFS("../../testdata"), "k")
	h := m.Jellyfin()
	firstPlayed := func() any {
		list := decodeJellyfinListing(t, jellyfinCall(h, http.MethodGet, "/Items?userId="+jellyfinAlice+"&isPlayed=true", jellyfinAuth))
		userData, _ := list.Items[0]["UserData"].(map[string]any)
		return userData["LastPlayedDate"]
	}
	if got := firstPlayed(); got != "2026-09-29T22:16:40.0000000Z" {
		t.Errorf("unshifted: %v", got)
	}
	m.ShiftSeconds = 86400
	if got := firstPlayed(); got != "2026-09-30T22:16:40.0000000Z" {
		t.Errorf("shifted: %v", got)
	}
}

func TestJellyfinWrites(t *testing.T) {
	m := New(os.DirFS("../../testdata"), "k")
	h := m.Jellyfin()
	const item = "/Items/f0000000000000000000000000000105"
	if rec := jellyfinCall(h, http.MethodDelete, item, jellyfinAuth); rec.Code != http.StatusNoContent {
		t.Errorf("delete: status %d", rec.Code)
	}
	calls := m.Calls()
	if len(calls) != 1 || calls[0].Service != "jellyfin" || calls[0].Method != http.MethodDelete || calls[0].Path != item {
		t.Errorf("calls after delete: %+v", calls)
	}
	m.JellyfinDeletionDenied = true
	if rec := jellyfinCall(h, http.MethodDelete, item, jellyfinAuth); rec.Code != http.StatusForbidden {
		t.Errorf("denied delete: status %d", rec.Code)
	}
	if rec := jellyfinCall(h, http.MethodPost, "/Library/Refresh", jellyfinAuth); rec.Code != http.StatusNoContent {
		t.Errorf("refresh: status %d", rec.Code)
	}
	calls = m.Calls()
	if last := calls[len(calls)-1]; last.Service != "jellyfin" || last.Method != http.MethodPost || last.Path != "/Library/Refresh" {
		t.Errorf("refresh not recorded: %+v", last)
	}
}

func TestJellyfinImages(t *testing.T) {
	h := New(os.DirFS("../../testdata"), "k").Jellyfin()
	const id = "/Items/f0000000000000000000000000000101/Images/"
	for _, target := range []string{id + "Primary", id + "Backdrop/0?maxWidth=1280&quality=90"} {
		rec := jellyfinCall(h, http.MethodGet, target, jellyfinAuth)
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/png" {
			t.Errorf("%s: status %d, %s", target, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
	if rec := jellyfinCall(h, http.MethodGet, "/Items/not-an-id/Images/Primary", jellyfinAuth); rec.Code != http.StatusNotFound {
		t.Errorf("invalid id: status %d", rec.Code)
	}
}

// plexCall sends one request to the fake Plex, with the token of the mock (key "k").
func plexCall(h http.Handler, method, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	req.Header.Set("X-Plex-Token", "k")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// plexMetadataResponse is the part of GET /library/metadata/{key} the tests read.
type plexMetadataResponse struct {
	MediaContainer struct {
		Size     int `json:"size"`
		Metadata []struct {
			RatingKey string `json:"ratingKey"`
			Title     string `json:"title"`
			Media     []struct {
				ID   int `json:"id"`
				Part []struct {
					Stream []map[string]any `json:"Stream"`
				} `json:"Part"`
			} `json:"Media"`
		} `json:"Metadata"`
	} `json:"MediaContainer"`
}

func TestPlexMetadata(t *testing.T) {
	h := New(os.DirFS("../../testdata"), "k").Plex()
	// 102 has its own fixture: its two versions, with the streams of each file.
	rec := plexCall(h, http.MethodGet, "/library/metadata/102")
	if rec.Code != http.StatusOK {
		t.Fatalf("102: status %d", rec.Code)
	}
	var dune plexMetadataResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &dune); err != nil {
		t.Fatal(err)
	}
	if len(dune.MediaContainer.Metadata) != 1 || dune.MediaContainer.Metadata[0].Title != "Dune" {
		t.Fatalf("102: %+v", dune.MediaContainer)
	}
	media := dune.MediaContainer.Metadata[0].Media
	if len(media) != 2 || len(media[0].Part[0].Stream) != 5 || len(media[1].Part[0].Stream) != 3 {
		t.Errorf("102: media %+v, want 2 versions with 5 and 3 streams", media)
	}

	// 101 is served from the movie section: no streams.
	rec = plexCall(h, http.MethodGet, "/library/metadata/101")
	var inception plexMetadataResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &inception); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("101: status %d, %v", rec.Code, err)
	}
	if got := inception.MediaContainer.Metadata; len(got) != 1 || got[0].RatingKey != "101" || got[0].Title != "Inception" ||
		len(got[0].Media) != 1 || len(got[0].Media[0].Part[0].Stream) != 0 {
		t.Errorf("101: %+v", got)
	}
	for _, target := range []string{"/library/metadata/999", "/library/metadata/abc"} {
		if rec := plexCall(h, http.MethodGet, target); rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d", target, rec.Code)
		}
	}
}

func TestPlexDeleteMedia(t *testing.T) {
	m := New(os.DirFS("../../testdata"), "k")
	h := m.Plex()
	const target = "/library/metadata/102/media/1022"
	if rec := plexCall(h, http.MethodDelete, target); rec.Code != http.StatusOK {
		t.Fatalf("delete: status %d", rec.Code)
	}
	calls := m.Calls()
	if len(calls) != 1 || calls[0].Service != "plex" || calls[0].Method != http.MethodDelete || calls[0].Path != target {
		t.Errorf("calls after delete: %+v", calls)
	}
	m.PlexDeletionDenied = true
	if rec := plexCall(h, http.MethodDelete, target); rec.Code != http.StatusForbidden {
		t.Errorf("denied delete: status %d", rec.Code)
	}
}

func TestJellyfinVersionsByIDs(t *testing.T) {
	m := New(os.DirFS("../../testdata"), "k")
	h := m.Jellyfin()
	// Dune (102) and the copy of Tenet (203): the copy is not in the library listing.
	const dune, tenetCopy = "f0000000000000000000000000000102", "f0000000000000000000000000000203"
	list := decodeJellyfinListing(t, jellyfinCall(h, http.MethodGet, "/Items?ids="+dune+","+tenetCopy+"&fields=MediaSources", jellyfinAuth))
	if len(list.Items) != 2 || list.TotalRecordCount != 2 {
		t.Fatalf("versions: %d items, total %d", len(list.Items), list.TotalRecordCount)
	}
	if list.Items[0]["Id"] != dune || list.Items[1]["Id"] != tenetCopy {
		t.Errorf("versions: %v, %v", list.Items[0]["Id"], list.Items[1]["Id"])
	}
	if got := m.Hits("jellyfin:versions"); got != 1 {
		t.Errorf("hits: %d", got)
	}
	if rec := jellyfinCall(h, http.MethodGet, "/Items?ids=../x", jellyfinAuth); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid id: status %d", rec.Code)
	}
}
