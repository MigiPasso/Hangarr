package jellyfin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

const (
	testAlice     = "aaaa0000000000000000000000001001"
	testBob       = "aaaa0000000000000000000000001002"
	testInception = "f0000000000000000000000000000101"
	testDune      = "f0000000000000000000000000000102"
	testTenet     = "f0000000000000000000000000000103"
)

// newTestClient serves h and returns a client talking to it.
func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return New(ts.URL, "k")
}

func TestAuthHeader(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/System/Info" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != `MediaBrowser Token="k"` {
			t.Errorf("Authorization = %q", got)
		}
		if r.Header.Get("X-Emby-Token") != "" || r.URL.Query().Has("api_key") {
			t.Error("token sent as X-Emby-Token or api_key")
		}
		_, _ = w.Write([]byte(`{"ServerName":"jellyfin-test","Version":"12.0.1","ProductName":"Jellyfin Server","Id":"5e0c1e6a3b2f4d7c9a8b1c2d3e4f5a6b"}`))
	})
	info, err := c.SystemInfo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.Version != "12.0.1" || info.ProductName != "Jellyfin Server" || info.ID != "5e0c1e6a3b2f4d7c9a8b1c2d3e4f5a6b" {
		t.Errorf("SystemInfo = %+v", info)
	}
}

func TestItemsPagination(t *testing.T) {
	const movies = 501
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		q := r.URL.Query()
		if r.URL.Path != "/Items" || q.Get("recursive") != "true" || q.Get("includeItemTypes") != TypeMovie || q.Get("limit") != "500" {
			t.Errorf("unexpected query: %s", r.URL.RawQuery)
		}
		if fields := strings.Split(q.Get("fields"), ","); !slices.Contains(fields, "ProviderIds") || !slices.Contains(fields, "Path") {
			t.Errorf("fields = %q", q.Get("fields"))
		}
		start, _ := strconv.Atoi(q.Get("startIndex"))
		var items []map[string]string
		for i := start; i < min(start+pageSize, movies); i++ {
			items = append(items, map[string]string{"Id": fmt.Sprintf("%032x", i+1), "Name": fmt.Sprintf("Movie %d", i+1), "Type": TypeMovie})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"Items": items, "TotalRecordCount": movies, "StartIndex": start})
	})
	items, err := c.Items(context.Background(), TypeMovie)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != movies {
		t.Errorf("got %d items, want %d", len(items), movies)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want 2 pages", calls.Load())
	}
}

func TestPlayedQuery(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/Items" {
			t.Errorf("path = %s", r.URL.Path)
		}
		want := map[string]string{
			"userId":           testAlice,
			"recursive":        "true",
			"includeItemTypes": "Movie,Episode",
			"isPlayed":         "true",
			"sortBy":           "DatePlayed",
			"sortOrder":        "Descending",
			"enableUserData":   "true",
			"startIndex":       "20",
			"limit":            "500",
		}
		for k, v := range want {
			if q.Get(k) != v {
				t.Errorf("%s = %q, want %q", k, q.Get(k), v)
			}
		}
		_, _ = w.Write([]byte(`{"Items":[{"Id":"` + testInception + `","Name":"Inception","Type":"Movie","UserData":{"Played":true,"PlayCount":1}}],"TotalRecordCount":42,"StartIndex":20}`))
	})
	items, total, err := c.Played(context.Background(), testAlice, 20)
	if err != nil {
		t.Fatal(err)
	}
	if total != 42 || len(items) != 1 || items[0].Name != "Inception" || items[0].UserData == nil || !items[0].UserData.Played {
		t.Errorf("total=%d items=%+v", total, items)
	}
}

func TestResumableQuery(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/Items" || q.Get("userId") != testBob || q.Get("filters") != "IsResumable" || q.Get("includeItemTypes") != "Movie,Episode" || q.Get("enableUserData") != "true" {
			t.Errorf("unexpected query: %s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"Items":[{"Id":"` + testTenet + `","Name":"Tenet","Type":"Movie","UserData":{"Played":false,"PlaybackPositionTicks":6300000000,"PlayedPercentage":7}}],"TotalRecordCount":1,"StartIndex":0}`))
	})
	items, err := c.Resumable(context.Background(), testBob)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].UserData == nil || items[0].UserData.PlaybackPositionTicks != 6300000000 || items[0].UserData.PlayedPercentage != 7 {
		t.Errorf("items = %+v", items)
	}
}

func TestDeleteAndRefresh(t *testing.T) {
	var mu sync.Mutex
	var got []string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Method+" "+r.URL.Path)
		mu.Unlock()
	})
	ctx := context.Background()
	if err := c.DeleteItem(ctx, testInception); err != nil {
		t.Fatal(err)
	}
	if err := c.RefreshLibrary(ctx); err != nil {
		t.Fatal(err)
	}
	want := []string{"DELETE /Items/" + testInception, "POST /Library/Refresh"}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(got, want) {
		t.Errorf("requests = %q, want %q", got, want)
	}
}

func TestExternalIDs(t *testing.T) {
	cases := []struct {
		name       string
		ids        map[string]string
		tvdb, tmdb int64
		imdb       string
	}{
		{"mixed case", map[string]string{"Tvdb": "170", "Tmdb": "27205", "Imdb": "tt1375666"}, 170, 27205, "tt1375666"},
		{"lower case", map[string]string{"tvdb": "81189", "tmdb": "1396", "imdb": "tt0903747"}, 81189, 1396, "tt0903747"},
		{"invalid imdb", map[string]string{"Imdb": "nope"}, 0, 0, ""},
		{"non numeric tmdb", map[string]string{"Tmdb": "abc"}, 0, 0, ""},
		{"nil map", nil, 0, 0, ""},
	}
	for _, c := range cases {
		tvdb, tmdb, imdb := Item{ProviderIDs: c.ids}.ExternalIDs()
		if tvdb != c.tvdb || tmdb != c.tmdb || imdb != c.imdb {
			t.Errorf("%s: got %d %d %q, want %d %d %q", c.name, tvdb, tmdb, imdb, c.tvdb, c.tmdb, c.imdb)
		}
	}
}

func TestIDs(t *testing.T) {
	for _, s := range []string{testInception, testAlice} {
		if !IsID(s) {
			t.Errorf("IsID(%q) = false", s)
		}
	}
	for _, s := range []string{strings.ToUpper(testInception), testInception + "0", testInception[1:], "", "f0000000-0000-0000-0000-000000000101"} {
		if IsID(s) {
			t.Errorf("IsID(%q) = true", s)
		}
	}

	alice, bob := AccountID(testAlice), AccountID(testBob)
	if alice < 1<<62 || bob < 1<<62 {
		t.Errorf("account ids below 1<<62: %d %d", alice, bob)
	}
	if alice == bob {
		t.Error("alice and bob share an account id")
	}
	if alice != AccountID(strings.ToUpper(testAlice)) {
		t.Error("AccountID must ignore case")
	}

	event := EventID(testAlice, testInception)
	if event < 1<<62 {
		t.Errorf("event id below 1<<62: %d", event)
	}
	if event != EventID(strings.ToUpper(testAlice), strings.ToUpper(testInception)) {
		t.Error("EventID must ignore case")
	}
	if event == EventID(testAlice, testDune) {
		t.Error("EventID must change with the item")
	}
	if event == EventID(testBob, testInception) {
		t.Error("EventID must change with the user")
	}
}
