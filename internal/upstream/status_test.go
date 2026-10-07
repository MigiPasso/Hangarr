package upstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"hangarr/internal/clients/sonarr"
)

// A failed read of the health alerts is never cached; a successful one is
// shared for alertsTTL.
func TestHealthAlertsCacheOnlySuccess(t *testing.T) {
	var down atomic.Bool
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		if down.Load() {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"source":"IndexerStatusCheck","type":"warning","message":"Indexers unavailable due to failures: A"}]`))
	}))
	defer ts.Close()
	c := &Clients{Sonarr: sonarr.New(ts.URL, "k")}
	ctx := context.Background()

	down.Store(true) // every attempt fails, retries included
	if got := healthAlerts(ctx, c, Sonarr); len(got) != 0 {
		t.Fatalf("failed read: %v", got)
	}
	down.Store(false)
	before := calls.Load()
	if got := healthAlerts(ctx, c, Sonarr); len(got) != 1 || calls.Load() == before {
		t.Fatalf("after a failure the next call must ask again: %v", got)
	}
	before = calls.Load()
	if got := healthAlerts(ctx, c, Sonarr); len(got) != 1 || calls.Load() != before {
		t.Fatalf("a success is cached: %v (%d new calls)", got, calls.Load()-before)
	}
}
