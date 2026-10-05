package images

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"cleanarr/internal/clients/httpx"
)

func TestRemoteRedirectToPlainHTTPRefused(t *testing.T) {
	var hit atomic.Bool
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit.Store(true)
	}))
	defer internal.Close()
	cdn := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL+"/admin", http.StatusFound)
	}))
	defer cdn.Close()
	p := New(t.TempDir(), func(string) *httpx.Client { return nil }, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	tlsClient := cdn.Client()
	p.Remote.Transport = tlsClient.Transport
	if _, err := p.fetchRemote(context.Background(), cdn.URL+"/poster.jpg"); err == nil {
		t.Error("a redirect to plain HTTP must fail")
	}
	if hit.Load() {
		t.Fatal("the plain-HTTP target must never be requested")
	}
}
