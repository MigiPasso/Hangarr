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
