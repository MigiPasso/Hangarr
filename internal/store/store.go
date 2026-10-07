// Package store owns the SQLite database: connection settings, embedded
// migrations and all SQL queries. Queries are always parameterized.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure Go driver, registered as "sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// DBFileName is the SQLite file inside DATA_DIR.
const DBFileName = "hangarr.db"

// legacyDBFileName is the file of Cleanarr, the former name; Open renames it.
const legacyDBFileName = "cleanarr.db"

// Store wraps the database handle.
type Store struct {
	db *sql.DB
}

// DSN builds the modernc.org/sqlite connection string. WAL + busy_timeout let
// the CLI (admin reset-password) and the server share the file; every
// transaction starts IMMEDIATE so writers serialize instead of failing on lock
// upgrades.
func DSN(path string) string {
	q := url.Values{}
	// busy_timeout first: the following pragmas (WAL switch) may need a lock.
	q.Add("_pragma", "busy_timeout(10000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "temp_store(MEMORY)")
	q.Set("_txlock", "immediate")
	return "file:" + path + "?" + q.Encode()
}

// Open creates DATA_DIR if needed, opens the database and applies migrations.
func Open(ctx context.Context, dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	if err := renameLegacyDB(dataDir); err != nil {
		return nil, err
	}
	return OpenPath(ctx, filepath.Join(dataDir, DBFileName))
}

// renameLegacyDB renames cleanarr.db (and its WAL files) to hangarr.db when
// only the old one exists. The WAL and shared-memory files move first: if the
// process stops halfway, the next start finishes the job without losing
// the journal.
func renameLegacyDB(dataDir string) error {
	newPath, oldPath := filepath.Join(dataDir, DBFileName), filepath.Join(dataDir, legacyDBFileName)
	// Rename only when hangarr.db is certainly absent: never overwrite it.
	if _, err := os.Stat(newPath); !errors.Is(err, os.ErrNotExist) {
		return nil //nolint:nilerr // present (or unreadable): keep it, Open reports the error
	}
	if _, err := os.Stat(oldPath); err != nil {
		return nil //nolint:nilerr // no legacy database: nothing to do
	}
	for _, suffix := range []string{"-wal", "-shm", ""} {
		err := os.Rename(oldPath+suffix, newPath+suffix)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("rename %s to %s: %w", legacyDBFileName+suffix, DBFileName+suffix, err)
		}
	}
	slog.Info("database renamed after the rename of Cleanarr to Hangarr", "from", legacyDBFileName, "to", DBFileName)
	return nil
}

// OpenPath opens the database at path and applies migrations.
func OpenPath(ctx context.Context, path string) (*Store, error) {
	db, err := sql.Open("sqlite", DSN(path))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	db.SetConnMaxIdleTime(5 * time.Minute)
	s := &Store{db: db}
	if err := pingWithRetry(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for packages that own their queries' tests.
func (s *Store) DB() *sql.DB { return s.db }

type migration struct {
	version int
	name    string
	sql     string
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return nil, err
	}
	var out []migration
	for _, e := range entries {
		name := e.Name()
		prefix, _, ok := strings.Cut(name, "_")
		if !ok || !strings.HasSuffix(name, ".sql") {
			return nil, fmt.Errorf("bad migration name %q", name)
		}
		v, err := strconv.Atoi(prefix)
		if err != nil {
			return nil, fmt.Errorf("bad migration version in %q", name)
		}
		b, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return nil, err
		}
		out = append(out, migration{version: v, name: name, sql: string(b)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	migs, err := loadMigrations()
	if err != nil {
		return err
	}
	for _, m := range migs {
		if err := s.applyMigration(ctx, m); err != nil {
			return fmt.Errorf("migration %s: %w", m.name, err)
		}
	}
	return nil
}

// applyMigration runs one migration in its own IMMEDIATE transaction; the
// version check happens inside it so concurrent starts apply it once.
func (s *Store) applyMigration(ctx context.Context, m migration) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, m.version).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, name, applied_at) VALUES (?, ?, ?)`,
		m.version, m.name, time.Now().Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

// Ready reports whether the database answers and every embedded migration
// has been applied.
func (s *Store) Ready(ctx context.Context) error {
	migs, err := loadMigrations()
	if err != nil {
		return err
	}
	var applied int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&applied); err != nil {
		return fmt.Errorf("database not ready: %w", err)
	}
	if applied < len(migs) {
		return fmt.Errorf("database not ready: %d/%d migrations applied", applied, len(migs))
	}
	return nil
}

func nullInt(v *int64) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *v, Valid: true}
}

func ptrInt(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	x := v.Int64
	return &x
}

// isBusy reports SQLITE_BUSY / SQLITE_LOCKED errors.
func isBusy(err error) bool {
	var coded interface{ Code() int }
	if errors.As(err, &coded) {
		code := coded.Code() & 0xff // primary result code
		return code == 5 || code == 6
	}
	return false
}

// pingWithRetry opens the first connection. Switching a fresh file to WAL
// can return SQLITE_BUSY without consulting the busy handler when several
// processes start at once (pod + `admin reset-password`), so retry briefly.
func pingWithRetry(ctx context.Context, db *sql.DB) error {
	deadline := time.Now().Add(15 * time.Second)
	wait := 10 * time.Millisecond
	for {
		err := db.PingContext(ctx)
		if err == nil || !isBusy(err) || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		wait = min(wait*2, 500*time.Millisecond)
	}
}

// BackupTo writes a consistent copy of the database to path (VACUUM INTO):
// safe while the application runs, the target must not exist.
func (s *Store) BackupTo(ctx context.Context, path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists", path)
	}
	_, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, path)
	return err
}
