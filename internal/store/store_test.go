package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestMigrationsIdempotentAndReady(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	for range 2 {
		s, err := Open(ctx, dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Ready(ctx); err != nil {
			t.Fatal(err)
		}
		_ = s.Close()
	}
}

func TestReadyDetectsMissingMigration(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM schema_migrations`); err != nil {
		t.Fatal(err)
	}
	if err := s.Ready(ctx); err == nil {
		t.Fatal("Ready must fail when migrations are missing")
	}
}

func TestConcurrentOpenAndCreateFirstUser(t *testing.T) {
	path := filepath.Join(t.TempDir(), DBFileName)
	ctx := context.Background()
	const n = 4
	var wg sync.WaitGroup
	created := make([]bool, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := OpenPath(ctx, path)
			if err != nil {
				errs[i] = err
				return
			}
			defer func() { _ = s.Close() }()
			created[i], errs[i] = s.CreateFirstUser(ctx, "admin", "hash", true, time.Now())
		}()
	}
	wg.Wait()
	count := 0
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("worker %d: %v", i, errs[i])
		}
		if created[i] {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("admin created %d times, want 1", count)
	}
}

func TestUserLookupIsCaseInsensitive(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	if _, err := s.CreateFirstUser(ctx, "Admin", "h", true, time.Now()); err != nil {
		t.Fatal(err)
	}
	u, err := s.UserByUsername(ctx, "ADMIN")
	if err != nil {
		t.Fatal(err)
	}
	if !u.MustChangePassword {
		t.Error("first user must have must_change_password set")
	}
	if _, err := s.UserByUsername(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestSecretIsStable(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	calls := 0
	gen := func() ([]byte, error) { calls++; return []byte{byte(calls)}, nil }
	a, err := s.Secret(ctx, "k", gen)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Secret(ctx, "k", gen)
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) || calls != 1 {
		t.Fatalf("secret not stable: %v %v calls=%d", a, b, calls)
	}
}

func TestBackupTo(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if err := st.SetSetting(ctx, "sync_time", "05:30", time.Now()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "backup.db")
	if err := st.BackupTo(ctx, path); err != nil {
		t.Fatal(err)
	}
	// SEC-14: hashes and encrypted secrets, readable by the owner only.
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("backup mode: %v %v", fi.Mode(), err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".backup-*")); len(left) != 0 {
		t.Errorf("temporary directory left: %v", left)
	}
	if err := st.BackupTo(ctx, path); err == nil {
		t.Error("an existing backup must not be overwritten")
	}
	copyStore, err := OpenPath(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = copyStore.Close() }()
	if v, _ := copyStore.Setting(ctx, "sync_time", ""); v != "05:30" {
		t.Errorf("backup content: %q", v)
	}
}

func TestLegacyDatabaseRenamed(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	old, err := OpenPath(ctx, filepath.Join(dir, legacyDBFileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := old.SetSetting(ctx, "probe", "kept", time.Now()); err != nil {
		t.Fatal(err)
	}
	_ = old.Close()
	st, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if v, _ := st.Setting(ctx, "probe", ""); v != "kept" {
		t.Errorf("data lost in the rename: %q", v)
	}
	if _, err := os.Stat(filepath.Join(dir, legacyDBFileName)); !os.IsNotExist(err) {
		t.Errorf("legacy file still there: %v", err)
	}
}

func TestPruneSyncRunsKeepsTheLatestOfEachSource(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	now := time.Unix(1_800_000_000, 0)
	old := now.Add(-SyncRunRetention - time.Hour)
	add := func(source string, at time.Time, finish bool) int64 {
		id, err := s.StartSyncRun(ctx, source, at)
		if err != nil {
			t.Fatal(err)
		}
		if finish {
			if err := s.FinishSyncRun(ctx, id, at.Add(time.Minute), 1, nil); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	add("sonarr", old, true)            // old: purged
	add("sonarr", now, true)            // recent: kept
	keptOld := add("radarr", old, true) // old but the latest radarr run: kept
	running := add("plex", old, false)  // never finished: kept (closed at startup)
	add("tautulli", old, true)          // old: purged
	add("tautulli", now.Add(-time.Hour), true)
	n, err := s.PruneSyncRuns(ctx, now.Add(-SyncRunRetention))
	if err != nil || n != 2 {
		t.Fatalf("pruned %d runs: %v", n, err)
	}
	var left int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sync_runs WHERE id IN (?, ?)`, keptOld, running).Scan(&left); err != nil || left != 2 {
		t.Errorf("latest run per source and running run must stay: %d %v", left, err)
	}
}

// The protection rule is evaluated in Go (badges, deletion) and in SQL
// (cleanup candidates): both must give the same answer.
func TestProtectedTagRuleGoAndSQLAgree(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	protected := []string{"keep", "Archive"}
	for _, c := range []struct {
		tags []string
		want bool
	}{
		{nil, false},
		{[]string{"keep"}, true},
		{[]string{"KEEP"}, true},
		{[]string{" keep "}, true},
		{[]string{"4k", "archive"}, true},
		{[]string{"keeper"}, false},
		{[]string{"kee p"}, false},
	} {
		b, _ := json.Marshal(c.tags)
		if c.tags == nil {
			b = []byte("[]")
		}
		clause, args := protectedClause("m", protected)
		var excluded bool
		err := s.db.QueryRowContext(ctx, `SELECT NOT EXISTS (SELECT 1 FROM (SELECT ? AS tags, 0 AS protected_local) m
			WHERE 1 = 1`+clause+`)`, append([]any{string(b)}, args...)...).Scan(&excluded)
		if err != nil {
			t.Fatal(err)
		}
		if got := HasProtectedTag(c.tags, protected); got != c.want || excluded != c.want {
			t.Errorf("%q: Go %v, SQL %v, want %v", c.tags, got, excluded, c.want)
		}
	}
}

// BUG-15: a database migrated by a more recent release is refused, at open
// and by the readiness probe.
func TestNewerDatabaseRefused(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO schema_migrations(version, name, applied_at) VALUES (9999, '9999_future.sql', 0)`); err != nil {
		t.Fatal(err)
	}
	if err := s.Ready(context.Background()); !errors.Is(err, ErrNewerDatabase) {
		t.Errorf("Ready: %v", err)
	}
	_ = s.Close()
	if _, err := Open(context.Background(), dir); !errors.Is(err, ErrNewerDatabase) || !strings.Contains(err.Error(), "9999_future.sql") {
		t.Fatalf("Open: %v", err)
	}
}

// SEC-12: repeated failures of a client share a row; old events are purged.
func TestAuthEventsAggregatedAndPruned(t *testing.T) {
	s, ctx := openTemp(t), context.Background()
	add := func(at int64, ip, event string) {
		t.Helper()
		if err := s.AddAuthEvent(ctx, AuthEvent{At: at, Username: "admin", Event: event, IP: ip}); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 50 {
		add(1000+int64(i), "203.0.113.7", EventLoginFailure)
	}
	add(1100, "198.51.100.2", EventLoginFailure)
	add(1101, "203.0.113.7", "login_success")
	add(1102, "203.0.113.7", EventLoginFailure)
	add(1102+int64(failureMergeWindow.Seconds())+1, "203.0.113.7", EventLoginFailure)
	events, err := s.AuthEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	var counts []int
	for _, e := range events {
		counts = append(counts, e.Count)
	}
	// Newest first: after the window, after the success, the other client, the 50.
	if len(events) != 5 || counts[0] != 1 || counts[1] != 1 || counts[2] != 1 || counts[3] != 1 || counts[4] != 50 {
		t.Fatalf("counts: %v", counts)
	}
	if n, err := s.PruneAuthEvents(ctx, time.Unix(1101, 0)); err != nil || n != 2 {
		t.Errorf("pruned %d: %v", n, err)
	}
}
