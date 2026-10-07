package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetriesOnServerErrors(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer ts.Close()
	c := New("svc", ts.URL, nil)
	c.Backoff = time.Millisecond
	var out struct{ OK bool }
	if err := c.JSON(context.Background(), Request{Path: "/x"}, &out); err != nil || !out.OK {
		t.Fatalf("err=%v out=%v", err, out)
	}
	if calls.Load() != 3 {
		t.Fatalf("calls=%d, want 3 (1 + 2 retries)", calls.Load())
	}
}

func TestNoRetryOnClientError(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer ts.Close()
	c := New("svc", ts.URL, nil)
	c.Backoff = time.Millisecond
	err := c.JSON(context.Background(), Request{Path: "/x"}, nil)
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusUnauthorized || calls.Load() != 1 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}

func TestErrorsNeverLeakCredentials(t *testing.T) {
	c := New("tautulli", "http://127.0.0.1:1", func(r *http.Request) {
		q := r.URL.Query()
		q.Set("apikey", "SECRET")
		r.URL.RawQuery = q.Encode()
	})
	c.Backoff = time.Millisecond
	c.Retries = 0
	err := c.JSON(context.Background(), Request{Path: "/api/v2", Query: url.Values{"cmd": {"x"}}}, nil)
	if err == nil || strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "apikey") {
		t.Fatalf("error leaks credentials or is nil: %v", err)
	}
}

func TestFlexTypes(t *testing.T) {
	var v struct {
		A, B, C, D, E Int
		F, G          Float
		H, I, J       String
		K, L          Time
	}
	in := `{"A":12,"B":"34","C":"","D":null,"E":"5.0","F":"0.5","G":1,"H":"x","I":123,"J":null,
		"K":"2024-03-01T10:00:00Z","L":"0001-01-01T00:00:00Z"}`
	if err := json.Unmarshal([]byte(in), &v); err != nil {
		t.Fatal(err)
	}
	if v.A != 12 || v.B != 34 || v.C != 0 || v.D != 0 || v.E != 5 {
		t.Errorf("Int: %+v", v)
	}
	if v.F != 0.5 || v.G != 1 {
		t.Errorf("Float: %v %v", v.F, v.G)
	}
	if v.H != "x" || v.I != "123" || v.J != "" {
		t.Errorf("String: %q %q %q", v.H, v.I, v.J)
	}
	if v.K.IsZero() || !v.L.IsZero() || v.L.UnixPtr() != nil {
		t.Errorf("Time: %v %v", v.K, v.L)
	}
}

func TestRedirectToAnotherHostKeepsTheKey(t *testing.T) {
	var leaked atomic.Value
	leaked.Store("")
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked.Store(r.Header.Get("X-Api-Key"))
		_, _ = w.Write([]byte(`{}`))
	}))
	defer other.Close()
	// Same port, another host name: 127.0.0.1 -> localhost.
	otherURL := strings.Replace(other.URL, "127.0.0.1", "localhost", 1)
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, otherURL+"/login", http.StatusFound)
	}))
	defer src.Close()
	c := New("sonarr", src.URL, func(r *http.Request) { r.Header.Set("X-Api-Key", "secret-key") })
	c.Retries = 0
	err := c.JSON(context.Background(), Request{Path: "/api/v3/system/status"}, nil)
	var re *RedirectError
	if !errors.As(err, &re) || re.Host != "localhost" {
		t.Errorf("cross-host redirect must be refused: %v", err)
	}
	if v := leaked.Load().(string); v != "" {
		t.Fatalf("API key sent to another host: %q", v)
	}
}

func TestRedirectOnSameHostIsFollowed(t *testing.T) {
	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/old" {
			http.Redirect(w, r, ts.URL+"/new", http.StatusMovedPermanently)
			return
		}
		if r.Header.Get("X-Api-Key") != "k" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer ts.Close()
	c := New("sonarr", ts.URL, func(r *http.Request) { r.Header.Set("X-Api-Key", "k") })
	var out struct{ OK bool }
	if err := c.JSON(context.Background(), Request{Path: "/old"}, &out); err != nil || !out.OK {
		t.Fatalf("same-host redirect: %v %v", err, out)
	}
}

func TestRedirectToPlainHTTPIsRefused(t *testing.T) {
	via := []*http.Request{{URL: &url.URL{Scheme: "https", Host: "sonarr.example:443"}}}
	var re *RedirectError
	if err := SameHostOnly(&http.Request{URL: &url.URL{Scheme: "http", Host: "sonarr.example"}}, via); !errors.As(err, &re) || !re.Downgrade {
		t.Errorf("https → http on the same host must be refused: %v", err)
	}
	if err := SameHostOnly(&http.Request{URL: &url.URL{Scheme: "https", Host: "sonarr.example"}}, via); err != nil {
		t.Errorf("https → https on the same host: %v", err)
	}
	via[0].URL.Scheme = "http"
	if err := SameHostOnly(&http.Request{URL: &url.URL{Scheme: "https", Host: "sonarr.example"}}, via); err != nil {
		t.Errorf("http → https upgrade must be followed: %v", err)
	}
}
