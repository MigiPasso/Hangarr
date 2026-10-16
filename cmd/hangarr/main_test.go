package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"hangarr/internal/auth"
	"hangarr/internal/config"
	"hangarr/internal/store"
)

// syncBuffer collects the output of run, written from several goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func testConfig(t *testing.T, env map[string]string) *config.Config {
	t.Helper()
	cfg, err := config.Load(func(k string) (string, bool) { v, ok := env[k]; return v, ok }, os.ReadFile)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// QUAL-05: serve starts on a blank data directory (admin banner, generated
// master key), answers its probes, marks the deletions a crash left pending,
// and stops cleanly, the database closed last.
func TestRunStartsAndStops(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.BeginAction(context.Background(), store.Action{At: 1, Action: "delete_movie", MediaKind: "movie",
		ArrID: 3, Title: "Tenet"}); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	addr := freeAddr(t)
	cfg := testConfig(t, map[string]string{"DATA_DIR": dir, "LISTEN_ADDR": addr, "COOKIE_SECURE": "false",
		"LOG_FORMAT": "text"})
	out := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, cfg, out) }()

	deadline := time.Now().Add(10 * time.Second)
	for {
		res, err := http.Get("http://" + addr + "/readyz")
		if err == nil {
			_ = res.Body.Close()
			if res.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("not ready: %v\n%s", err, out.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Setenv("LISTEN_ADDR", addr)
	if err := healthcheck(); err != nil {
		t.Errorf("healthcheck: %v", err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("run did not stop")
	}
	logs := out.String()
	for _, want := range []string{"INITIAL ADMIN ACCOUNT CREATED", "generated a master key", "deletions interrupted",
		"shutdown complete"} {
		if !strings.Contains(logs, want) {
			t.Errorf("missing %q in the output", want)
		}
	}
	if i, j := strings.LastIndex(logs, "shutdown complete"), strings.LastIndex(logs, "database closed"); j < i {
		t.Errorf("the database must close last:\n%s", logs)
	}
	if _, err := os.Stat(filepath.Join(dir, "master.key")); err != nil {
		t.Errorf("master key: %v", err)
	}
	st, err = store.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	actions, _, _ := st.Actions(context.Background(), 0, 1)
	if len(actions) != 1 || actions[0].State != store.ActionInterrupted {
		t.Errorf("pending action: %+v", actions)
	}
}

func TestHealthcheck(t *testing.T) {
	status := http.StatusOK
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
	}))
	defer ts.Close()
	t.Setenv("LISTEN_ADDR", strings.TrimPrefix(ts.URL, "http://"))
	if err := healthcheck(); err != nil {
		t.Errorf("healthy: %v", err)
	}
	status = http.StatusServiceUnavailable
	if err := healthcheck(); err == nil {
		t.Error("unhealthy server reported healthy")
	}
	t.Setenv("LISTEN_ADDR", "bad")
	if err := healthcheck(); err == nil {
		t.Error("invalid LISTEN_ADDR accepted")
	}
}

func TestBackupCommand(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DATA_DIR", dir)
	st, err := store.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	path := filepath.Join(t.TempDir(), "sub", "copy.db")
	if err := backup([]string{path}); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("backup: %v %v", fi, err)
	}
	if err := backup([]string{path}); err == nil {
		t.Error("an existing backup was overwritten")
	}
	if err := backup([]string{"a", "b"}); err == nil {
		t.Error("two paths accepted")
	}
}

// The janitor purges what is past its retention on its first pass.
func TestJanitor(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	old := time.Now().AddDate(-2, 0, 0).Unix()
	if err := st.AddAuthEvent(ctx, store.AuthEvent{At: old, Event: "login_success", Username: "admin"}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddAction(ctx, store.Action{At: old, Action: "delete_movie", MediaKind: "movie", Title: "Old", Success: true}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{AuthEventsRetentionDays: 365, ActionLogRetentionDays: 30}
	h, err := auth.NewHasher(auth.HashParams{Memory: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32})
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := &auth.Service{Store: st, Hasher: h, Log: log, IdleTimeout: time.Hour, MaxAge: time.Hour}
	swept := make(chan struct{}, 1)
	go janitor(ctx, log, cfg, svc, st, func() { swept <- struct{}{} })
	select {
	case <-swept:
	case <-time.After(10 * time.Second):
		t.Fatal("janitor did not run")
	}
	events, _ := st.AuthEvents(ctx, 10)
	actions, _, _ := st.Actions(ctx, 0, 10)
	if len(events) != 0 || len(actions) != 0 {
		t.Errorf("not purged: %d events, %d actions", len(events), len(actions))
	}
}
