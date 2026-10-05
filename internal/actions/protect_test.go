package actions

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"
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
	// Plex-only items cannot be protected; no tag configured is an error.
	if _, err := e.svc.SetProtected(ctx, ProtectRequest{MediaID: e.mediaID("movie", "Vacances 2019"), Protect: true}); !errors.Is(err, ErrNotManaged) {
		t.Errorf("plex only: %v", err)
	}
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
