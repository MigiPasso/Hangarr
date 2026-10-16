package httpx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

// BUG-04: only GET and HEAD are retried by default; a DELETE or a POST is
// sent once, a PUT marked Retry is retried.
func TestRetryOnlyIdempotent(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer ts.Close()
	c := New("svc", ts.URL, nil)
	c.Backoff = time.Millisecond
	for _, tc := range []struct {
		req  Request
		want int32
	}{
		{Request{Method: http.MethodGet, Path: "/x"}, 3},
		{Request{Method: http.MethodDelete, Path: "/x"}, 1},
		{Request{Method: http.MethodPost, Path: "/api/v3/tag", Body: map[string]string{"label": "keep"}}, 1},
		{Request{Method: http.MethodPut, Path: "/x", Body: map[string]int{"id": 1}}, 1},
		{Request{Method: http.MethodPut, Path: "/x", Body: map[string]int{"id": 1}, Retry: true}, 3},
	} {
		calls.Store(0)
		if err := c.JSON(context.Background(), tc.req, nil); err == nil {
			t.Fatalf("%s: no error", tc.req.Method)
		}
		if got := calls.Load(); got != tc.want {
			t.Errorf("%s retry=%v: %d calls, want %d", tc.req.Method, tc.req.Retry, got, tc.want)
		}
	}
}

// BUG-04: a slow DELETE is sent once, with its own (longer) timeout, and a
// timeout is reported as ambiguous.
func TestDeleteTimeoutAndAmbiguous(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(150 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	c := New("radarr", ts.URL, nil)
	c.Timeout, c.DeleteTimeout, c.Backoff = 50*time.Millisecond, time.Second, time.Millisecond
	if err := c.JSON(context.Background(), Request{Method: http.MethodDelete, Path: "/api/v3/movie/1"}, nil); err != nil {
		t.Fatalf("DELETE within DeleteTimeout: %v", err)
	}
	c.DeleteTimeout = 60 * time.Millisecond
	calls.Store(0)
	err := c.JSON(context.Background(), Request{Method: http.MethodDelete, Path: "/api/v3/movie/1"}, nil)
	if err == nil || !Ambiguous(err) {
		t.Fatalf("timeout must be ambiguous: %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("DELETE sent %d times", calls.Load())
	}
	for _, e := range []error{
		&StatusError{Status: 404}, &RedirectError{Host: "x"}, &TooLargeError{Limit: 1}, context.Canceled, nil,
	} {
		if Ambiguous(e) {
			t.Errorf("%v must not be ambiguous", e)
		}
	}
}

// SEC-04: a redirect to another port of the same host, which may be another
// service, does not get the API key.
func TestRedirectToAnotherPortIsRefused(t *testing.T) {
	var leaked atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "" {
			leaked.Store(true)
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer other.Close()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusFound)
	}))
	defer ts.Close()
	c := New("sonarr", ts.URL, func(r *http.Request) { r.Header.Set("X-Api-Key", "secret-key") })
	err := c.JSON(context.Background(), Request{Path: "/api"}, nil)
	var re *RedirectError
	if !errors.As(err, &re) || leaked.Load() {
		t.Fatalf("err=%v leaked=%v", err, leaked.Load())
	}
	for _, tc := range []struct {
		from, to string
		ok       bool
	}{
		{"http://sonarr.lan:8989/a", "http://sonarr.lan:8989/b", true},
		{"http://sonarr.lan/a", "https://sonarr.lan/b", true},
		{"http://sonarr.lan:80/a", "https://sonarr.lan:443/b", true},
		{"http://SONARR.lan/a", "http://sonarr.lan/b", true},
		{"http://sonarr.lan:8989/a", "http://sonarr.lan:9999/b", false},
		{"http://sonarr.lan:8989/a", "https://sonarr.lan/b", false},
		{"http://sonarr.lan/a", "https://sonarr.lan:8443/b", false},
		{"https://sonarr.lan/a", "https://sonarr.lan:8443/b", false},
		{"https://sonarr.lan/a", "http://sonarr.lan/b", false},
		{"http://sonarr.lan/a", "http://evil.lan/b", false},
	} {
		from, _ := url.Parse(tc.from)
		to, _ := url.Parse(tc.to)
		err := SameHostOnly(&http.Request{URL: to}, []*http.Request{{URL: from}})
		if (err == nil) != tc.ok {
			t.Errorf("%s → %s: %v, want ok=%v", tc.from, tc.to, err, tc.ok)
		}
	}
}

// SEC-11: MaxBytes stops reading a response early; the JSON is decoded as it
// arrives.
func TestMaxBytesAndStreaming(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[`+strings.Repeat(`1,`, 1000)+`1]`)
	}))
	defer ts.Close()
	c := New("img", ts.URL, nil)
	_, err := c.Do(context.Background(), Request{Path: "/x", MaxBytes: 100})
	var tl *TooLargeError
	if !errors.As(err, &tl) || tl.Limit != 100 {
		t.Fatalf("err = %v", err)
	}
	var out []int
	if err := c.JSON(context.Background(), Request{Path: "/x"}, &out); err != nil || len(out) != 1001 {
		t.Fatalf("decode: %v %d", err, len(out))
	}
	if err := c.JSON(context.Background(), Request{Path: "/x", MaxBytes: 50}, &out); !errors.As(err, &tl) {
		t.Errorf("decode over the limit: %v", err)
	}
}

// BUG-12: error snippets are cut on a character boundary.
func TestTruncateRunes(t *testing.T) {
	for _, tc := range []struct {
		in   string
		n    int
		want string
	}{
		{"abc", 5, "abc"},
		{"abcdef", 3, "abc…"},
		{"ééééé", 2, "éé…"},
		{"日本語テキスト", 3, "日本語…"},
	} {
		if got := Truncate(tc.in, tc.n); got != tc.want || !utf8.ValidString(got) {
			t.Errorf("Truncate(%q, %d) = %q", tc.in, tc.n, got)
		}
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, strings.Repeat("é", 300))
	}))
	defer ts.Close()
	_, err := New("svc", ts.URL, nil).Do(context.Background(), Request{Path: "/x"})
	if err == nil || !utf8.ValidString(err.Error()) {
		t.Errorf("snippet: %v", err)
	}
}

// BUG-07: a server that ignores the offset sends the same page again: the
// loop stops with an error instead of running forever.
func TestPageGuard(t *testing.T) {
	var g PageGuard
	if err := g.Next("a"); err != nil {
		t.Fatal(err)
	}
	if err := g.Next("b"); err != nil {
		t.Fatal(err)
	}
	if err := g.Next("b"); !errors.Is(err, ErrPagingIgnored) {
		t.Errorf("same first key: %v", err)
	}
	var h PageGuard
	for i := range MaxPages {
		if err := h.Next(fmt.Sprint(i)); err != nil {
			t.Fatalf("page %d: %v", i, err)
		}
	}
	if err := h.Next("x"); err == nil {
		t.Error("no page limit")
	}
}
