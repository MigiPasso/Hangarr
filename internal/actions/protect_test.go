package actions

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"hangarr/internal/syncer"
)

func putTags(t *testing.T, body string) ([]int64, map[string]any) {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, v := range obj["tags"].([]any) {
		ids = append(ids, int64(v.(float64)))
	}
	return ids, obj
}

func TestProtectSeries(t *testing.T) {
	e := newEnv(t, false)
	ctx := context.Background()
	id := e.mediaID("series", "Breaking Bad")
	res, err := e.svc.SetProtected(ctx, ProtectRequest{MediaID: id, Protect: true, Username: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Protect || res.Tag != "keep" || res.Unchanged || res.DryRun {
		t.Errorf("result: %+v", res)
	}
	calls := e.destructive()
	if len(calls) != 1 || calls[0].Method != http.MethodPut || calls[0].Path != "/api/v3/series/1" {
		t.Fatalf("calls: %+v", calls)
	}
	ids, obj := putTags(t, calls[0].Body)
	if len(ids) != 1 || ids[0] != 1 {
		t.Errorf("tags sent: %v", ids)
	}
	if obj["title"] != "Breaking Bad" || obj["seasons"] == nil {
		t.Error("the whole series object must be sent back")
	}
	if a := e.lastAction(); a.Action != ProtectAction || !a.Success || a.DryRun || a.Username != "admin" {
		t.Errorf("journal: %+v", a)
	}
	m, _ := e.st.MediaByID(ctx, id)
	if len(m.Tags) != 1 || m.Tags[0] != "keep" {
		t.Errorf("local tags: %v", m.Tags)
	}
}

func TestUnprotectKeepsOtherTags(t *testing.T) {
	e := newEnv(t, false)
	// Severance is tagged keep (1); add anime (2) to check it stays.
	e.fsys["sonarr/series.json"] = &fstest.MapFile{Data: []byte(replaceOnce(t, string(e.fsys["sonarr/series.json"].Data),
		`"tags": [1]`, `"tags": [2, 1]`))}
	id := e.mediaID("series", "Severance")
	res, err := e.svc.SetProtected(context.Background(), ProtectRequest{MediaID: id, Protect: false})
	if err != nil {
		t.Fatal(err)
	}
	if res.Protect || res.Tag != "keep" {
		t.Errorf("result: %+v", res)
	}
	calls := e.destructive()
	if len(calls) != 1 {
		t.Fatalf("calls: %+v", calls)
	}
	if ids, _ := putTags(t, calls[0].Body); len(ids) != 1 || ids[0] != 2 {
		t.Errorf("tags sent: %v (anime must stay)", ids)
	}
	if a := e.lastAction(); a.Action != UnprotectAction || !a.Success {
		t.Errorf("journal: %+v", a)
	}
}

func TestProtectCreatesMissingTagForMovie(t *testing.T) {
	e := newEnv(t, false)
	e.fsys["radarr/tag.json"] = &fstest.MapFile{Data: []byte(`[{"id": 2, "label": "anime"}]`)}
	id := e.mediaID("movie", "Tenet")
	res, err := e.svc.SetProtected(context.Background(), ProtectRequest{MediaID: id, Protect: true})
	if err != nil {
		t.Fatal(err)
	}
	calls := e.destructive()
	if len(calls) != 2 || calls[0].Method != http.MethodPost || calls[0].Path != "/api/v3/tag" ||
		calls[1].Method != http.MethodPut || calls[1].Path != "/api/v3/movie/3" {
		t.Fatalf("calls: %+v", calls)
	}
	if calls[0].Body != `{"label":"keep"}` {
		t.Errorf("tag creation body: %s", calls[0].Body)
	}
	if ids, _ := putTags(t, calls[1].Body); len(ids) != 1 || ids[0] != 100 {
		t.Errorf("tags sent: %v", ids)
	}
	if res.Tag != "keep" {
		t.Errorf("result: %+v", res)
	}
}

func TestProtectDryRunAndNoop(t *testing.T) {
	e := newEnv(t, true)
	ctx := context.Background()
	bb := e.mediaID("series", "Breaking Bad")
	res, err := e.svc.SetProtected(ctx, ProtectRequest{MediaID: bb, Protect: true})
	if err != nil || !res.DryRun || res.Tag != "keep" {
		t.Fatalf("dry-run: %+v %v", res, err)
	}
	if calls := e.destructive(); len(calls) != 0 {
		t.Errorf("dry-run must not write: %+v", calls)
	}
	if a := e.lastAction(); a.Action != ProtectAction || !a.DryRun || !a.Success {
		t.Errorf("dry-run journal: %+v", a)
	}
	if m, _ := e.st.MediaByID(ctx, bb); len(m.Tags) != 0 {
		t.Errorf("dry-run changed local tags: %v", m.Tags)
	}
	// Already protected / not protected: nothing sent, nothing journaled.
	before, _, _ := e.st.Actions(ctx, 0, 100)
	if res, err := e.svc.SetProtected(ctx, ProtectRequest{MediaID: e.mediaID("series", "Severance"), Protect: true}); err != nil || !res.Unchanged {
		t.Errorf("already protected: %+v %v", res, err)
	}
	if res, err := e.svc.SetProtected(ctx, ProtectRequest{MediaID: bb, Protect: false}); err != nil || !res.Unchanged {
		t.Errorf("not protected: %+v %v", res, err)
	}
	if after, _, _ := e.st.Actions(ctx, 0, 100); len(after) != len(before) {
		t.Error("no-op requests must not be journaled")
	}
	// A stale local copy (tag removed in Sonarr since the sync) is realigned.
	if err := e.st.SetMediaTags(ctx, bb, []string{"keep"}); err != nil {
		t.Fatal(err)
	}
	if res, err := e.svc.SetProtected(ctx, ProtectRequest{MediaID: bb, Protect: false}); err != nil || !res.Unchanged {
		t.Errorf("stale protection: %+v %v", res, err)
	}
	if m, _ := e.st.MediaByID(ctx, bb); len(m.Tags) != 0 {
		t.Errorf("local tags not realigned: %v", m.Tags)
	}
	// No tag configured is an error (Plex-only items: TestLocalProtection).
	e.svc.ProtectedTags = nil
	if _, err := e.svc.SetProtected(ctx, ProtectRequest{MediaID: bb, Protect: true}); !errors.Is(err, ErrNoProtectedTag) {
		t.Errorf("no tag: %v", err)
	}
}

func replaceOnce(t *testing.T, s, old, repl string) string {
	t.Helper()
	if !strings.Contains(s, old) {
		t.Fatalf("%q not found", old)
	}
	return strings.Replace(s, old, repl, 1)
}

func TestLocalProtection(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		e := newEnv(t, dryRun)
		ctx := context.Background()
		id := e.mediaID("movie", "Vacances 2019")
		// In Hangarr only, even in dry-run: nothing is sent anywhere.
		res, err := e.svc.SetProtected(ctx, ProtectRequest{MediaID: id, Protect: true, Username: "admin"})
		if err != nil || !res.Local || res.Unchanged || res.DryRun {
			t.Fatalf("dry-run=%v protect: %+v %v", dryRun, res, err)
		}
		if calls := e.mock.Calls(); len(calls) != 0 {
			t.Errorf("nothing must be sent: %+v", calls)
		}
		if a := e.lastAction(); a.Action != ProtectAction || !a.Success || a.DryRun || a.Username != "admin" {
			t.Errorf("journal: %+v", a)
		}
		if res, _ := e.svc.SetProtected(ctx, ProtectRequest{MediaID: id, Protect: true}); !res.Unchanged {
			t.Error("already protected")
		}
		// The sync keeps it, and a protected item cannot be deleted.
		if _, err := e.svc.Refresher.(*syncer.Syncer).Run(ctx); err != nil {
			t.Fatal(err)
		}
		if m, _ := e.st.MediaByID(ctx, id); !m.ProtectedLocal {
			t.Fatal("protection lost by the sync")
		}
		if _, err := e.svc.Delete(ctx, Request{MediaID: id}); !errors.Is(err, ErrProtectedLocal) {
			t.Errorf("delete protected: %v", err)
		}
		if a := e.lastAction(); a.Action != DeletePlex || a.Success {
			t.Errorf("refusal journal: %+v", a)
		}
		if res, err := e.svc.SetProtected(ctx, ProtectRequest{MediaID: id, Protect: false}); err != nil || res.Unchanged {
			t.Fatalf("unprotect: %+v %v", res, err)
		}
		if _, err := e.svc.Delete(ctx, Request{MediaID: id}); err != nil {
			t.Errorf("delete after unprotect: %v", err)
		}
		e.mock.Reset()
	}
}

// BUG-01: an item protected in Hangarr that Sonarr/Radarr took over keeps
// that protection: deletion refused (single and batch), and "Unprotect"
// removes it.
func TestInheritedLocalProtection(t *testing.T) {
	e := newEnv(t, false)
	ctx := context.Background()
	id := e.mediaID("movie", "Tenet")
	if err := e.st.SetLocalProtection(ctx, id, true, "admin", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Delete(ctx, Request{MediaID: id, DeleteFiles: true}); !errors.Is(err, ErrProtectedLocal) {
		t.Fatalf("delete: %v", err)
	}
	if report := e.svc.DeleteBatch(ctx, []int64{id}, BatchOptions{DeleteFiles: true}); report[0].OK ||
		!errors.Is(report[0].Err, ErrProtectedLocal) {
		t.Fatalf("batch: %+v", report)
	}
	if calls := e.destructive(); len(calls) != 0 {
		t.Fatalf("destructive calls: %+v", calls)
	}
	res, err := e.svc.SetProtected(ctx, ProtectRequest{MediaID: id, Protect: false, Username: "admin"})
	if err != nil || res.Unchanged || !res.Local {
		t.Fatalf("unprotect: %+v %v", res, err)
	}
	if a := e.lastAction(); a.Action != UnprotectAction || !a.Success || a.ArrID == 0 {
		t.Errorf("journal: %+v", a)
	}
	if _, err := e.svc.Delete(ctx, Request{MediaID: id, DeleteFiles: true}); err != nil {
		t.Fatalf("delete after unprotect: %v", err)
	}
}
