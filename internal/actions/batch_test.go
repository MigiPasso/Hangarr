package actions

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestDeleteBatch(t *testing.T) {
	e := newEnv(t, false)
	ids := []int64{e.mediaID("movie", "Tenet"), e.mediaID("series", "Severance"), e.mediaID("movie", "Interstellar")}
	before := e.mock.Hits("plex:refresh")
	report := e.svc.DeleteBatch(context.Background(), ids, BatchOptions{DeleteFiles: true, Username: "admin"})
	if len(report) != 3 {
		t.Fatalf("report: %+v", report)
	}
	// Sequential, one result per item; the protected series fails alone.
	if !report[0].OK || report[0].Title != "Tenet" || report[0].SizeFreed != 15_000_000_000 {
		t.Errorf("Tenet: %+v", report[0])
	}
	if report[1].OK || !errors.Is(report[1].Err, ErrProtected) || report[1].Title != "Severance" {
		t.Errorf("Severance: %+v", report[1])
	}
	if !report[2].OK {
		t.Errorf("Interstellar: %+v", report[2])
	}
	var deletes []string
	for _, c := range e.destructive() {
		if c.Method == http.MethodDelete {
			deletes = append(deletes, c.Path)
		}
	}
	if strings.Join(deletes, ",") != "/api/v3/movie/3,/api/v3/movie/4" {
		t.Errorf("deletions: %v", deletes)
	}
	// Whole series do not need their title typed in a batch (confirmed once).
	if n := e.mock.Hits("plex:refresh") - before; n != 1 {
		t.Errorf("Plex must be refreshed once per batch and kind, got %d", n)
	}
	actions, _, _ := e.st.Actions(context.Background(), 0, 10)
	if len(actions) != 3 {
		t.Errorf("every attempt is journaled: %d", len(actions))
	}
}

func TestDeleteBatchSeriesWithoutTitle(t *testing.T) {
	e := newEnv(t, true)
	report := e.svc.DeleteBatch(context.Background(), []int64{e.mediaID("series", "Breaking Bad")}, BatchOptions{DeleteFiles: true})
	if len(report) != 1 || !report[0].OK {
		t.Errorf("batch confirmation must replace the typed title: %+v", report)
	}
}

func TestProtectBatch(t *testing.T) {
	e := newEnv(t, true)
	report := e.svc.ProtectBatch(context.Background(),
		[]int64{e.mediaID("movie", "Dune"), e.mediaID("series", "Severance"), e.mediaID("movie", "Vacances 2019")}, "admin")
	// Plex-only: protected in Cleanarr only.
	if len(report) != 3 || !report[0].OK || report[0].Unchanged || !report[1].OK || !report[1].Unchanged || !report[2].OK {
		t.Errorf("report: %+v", report)
	}
}
