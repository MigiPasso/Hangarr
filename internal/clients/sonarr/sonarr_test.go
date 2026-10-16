package sonarr

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestUnmonitorSeason(t *testing.T) {
	raw := json.RawMessage(`{"id":2,"title":"The Expanse","network":"Prime Video","seasons":[
		{"seasonNumber":1,"monitored":true,"statistics":{"x":1}},{"seasonNumber":2,"monitored":true}]}`)
	out, ok, err := UnmonitorSeason(raw, 2)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	var obj struct {
		Network string
		Seasons []struct {
			SeasonNumber int
			Monitored    bool
			Statistics   map[string]int
		}
	}
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if obj.Network != "Prime Video" || !obj.Seasons[0].Monitored || obj.Seasons[1].Monitored || obj.Seasons[0].Statistics["x"] != 1 {
		t.Errorf("result: %s", out)
	}
	if _, ok, err := UnmonitorSeason(raw, 9); ok || err != nil {
		t.Errorf("missing season: ok=%v err=%v", ok, err)
	}
	if _, _, err := UnmonitorSeason(json.RawMessage(`[`), 1); err == nil {
		t.Error("invalid JSON accepted")
	}
}

func TestWithTags(t *testing.T) {
	out, err := WithTags(json.RawMessage(`{"id":1,"tags":[1,2],"path":"/tv/x"}`), nil)
	if err != nil || !strings.Contains(string(out), `"tags":[]`) || !strings.Contains(string(out), `"path":"/tv/x"`) {
		t.Fatalf("%s %v", out, err)
	}
}

// The deletion carries its options in the query, is sent once, and a POST of
// a tag is never repeated.
func TestDeleteSeriesAndCreateTag(t *testing.T) {
	var deletes, posts atomic.Int32
	var query, body string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "k" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case http.MethodDelete:
			deletes.Add(1)
			query = r.URL.RawQuery
			w.WriteHeader(http.StatusBadGateway)
		case http.MethodPost:
			posts.Add(1)
			b, _ := io.ReadAll(r.Body)
			body = string(b)
			w.WriteHeader(http.StatusBadGateway)
		}
	}))
	defer ts.Close()
	c := New(ts.URL, "k")
	if err := c.DeleteSeries(context.Background(), 7, true, false); err == nil {
		t.Error("502 must fail")
	}
	if deletes.Load() != 1 || query != "addImportListExclusion=false&deleteFiles=true" {
		t.Errorf("deletes=%d query=%q", deletes.Load(), query)
	}
	if _, err := c.CreateTag(context.Background(), "keep"); err == nil {
		t.Error("502 must fail")
	}
	if posts.Load() != 1 || body != `{"label":"keep"}` {
		t.Errorf("posts=%d body=%q", posts.Load(), body)
	}
}

func FuzzUnmonitorSeason(f *testing.F) {
	f.Add([]byte(`{"seasons":[{"seasonNumber":1,"monitored":true}]}`), 1)
	f.Add([]byte(`{"seasons":null}`), 0)
	f.Fuzz(func(t *testing.T, raw []byte, n int) {
		out, ok, err := UnmonitorSeason(raw, n)
		if err == nil && ok && !json.Valid(out) {
			t.Fatalf("invalid JSON out: %s", out)
		}
	})
}

func FuzzWithTags(f *testing.F) {
	f.Add([]byte(`{"id":1,"tags":[1]}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		out, err := WithTags(raw, []int64{3})
		if err == nil && (!json.Valid(out) || !strings.Contains(string(out), `"tags":[3]`)) {
			t.Fatalf("bad output: %s", out)
		}
	})
}
