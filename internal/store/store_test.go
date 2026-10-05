package store

import (
	"context"
	"errors"
	"path/filepath"
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
			created[i], errs[i] = s.CreateFirstUser(ctx, "admin", "hash", time.Now())
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
	if _, err := s.CreateFirstUser(ctx, "Admin", "h", time.Now()); err != nil {
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
