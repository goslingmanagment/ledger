// Package db opens the SQLite file and applies the embedded forward-only migrations.
//
// One writer pool (a single connection, BEGIN IMMEDIATE) and one read pool on the same
// WAL file. Litestream replicates the file from its own container.
package db

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure Go driver, no cgo
)

//go:embed migrations/*.sql
var migrations embed.FS

// Store holds the write pool and the read pool of one database file.
type Store struct {
	Path string
	RW   *sql.DB // single connection: every write is serialised
	RO   *sql.DB // read-only connections for reports
}

func dsn(path string, readOnly bool) string {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(15000)")
	q.Add("_pragma", "foreign_keys(1)")
	if readOnly {
		q.Set("mode", "ro")
		q.Add("_pragma", "query_only(1)")
	} else {
		q.Add("_pragma", "journal_mode(WAL)")
		q.Add("_pragma", "synchronous(NORMAL)")
		q.Set("_txlock", "immediate")
	}
	return "file:" + path + "?" + q.Encode()
}

// Open opens (creating if needed) the database at path.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	rw, err := sql.Open("sqlite", dsn(path, false))
	if err != nil {
		return nil, err
	}
	rw.SetMaxOpenConns(1)
	rw.SetConnMaxIdleTime(0)
	if err := rw.Ping(); err != nil {
		rw.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	ro, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		rw.Close()
		return nil, err
	}
	ro.SetMaxOpenConns(4)
	return &Store{Path: path, RW: rw, RO: ro}, nil
}

// OpenReadOnly opens an existing file without the write pool (verify, drills).
func OpenReadOnly(path string) (*Store, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	ro, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		return nil, err
	}
	if err := ro.Ping(); err != nil {
		ro.Close()
		return nil, err
	}
	return &Store{Path: path, RO: ro}, nil
}

// Close closes both pools.
func (s *Store) Close() error {
	var errs []error
	if s.RW != nil {
		errs = append(errs, s.RW.Close())
	}
	if s.RO != nil {
		errs = append(errs, s.RO.Close())
	}
	return errors.Join(errs...)
}

// Migrate applies every embedded migration not yet recorded, in name order, each in its
// own transaction. Names are recorded, not a counter, so migrations merged in any order
// from parallel branches still apply exactly once.
func (s *Store) Migrate(ctx context.Context) ([]string, error) {
	if _, err := s.RW.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		name TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return nil, err
	}
	applied := map[string]bool{}
	rows, err := s.RW.QueryContext(ctx, `SELECT name FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		applied[n] = true
	}
	rows.Close()
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	var done []string
	for _, path := range names {
		name := strings.TrimSuffix(filepath.Base(path), ".sql")
		if applied[name] {
			continue
		}
		body, err := migrations.ReadFile(path)
		if err != nil {
			return done, err
		}
		tx, err := s.RW.BeginTx(ctx, nil)
		if err != nil {
			return done, err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			tx.Rollback()
			return done, fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(name, applied_at) VALUES(?, ?)`,
			name, time.Now().UTC().Format(time.RFC3339)); err != nil {
			tx.Rollback()
			return done, err
		}
		if err := tx.Commit(); err != nil {
			return done, err
		}
		done = append(done, name)
	}
	return done, nil
}

// SchemaVersion is the name of the last applied migration ("" on an empty file).
func SchemaVersion(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (string, error) {
	var v sql.NullString
	err := q.QueryRowContext(ctx, `SELECT max(name) FROM schema_migrations`).Scan(&v)
	if err != nil && strings.Contains(err.Error(), "no such table") {
		return "", nil
	}
	return v.String, err
}

// Backup writes a consistent copy with VACUUM INTO and checks its integrity.
func (s *Store) Backup(ctx context.Context, dest string) error {
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("%s exists", dest)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	pool := s.RW
	if pool == nil {
		pool = s.RO
	}
	if _, err := pool.ExecContext(ctx, `VACUUM INTO ?`, dest); err != nil {
		return fmt.Errorf("vacuum into %s: %w", dest, err)
	}
	if err := os.Chmod(dest, 0o600); err != nil {
		return err
	}
	c, err := OpenReadOnly(dest)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.Integrity(ctx)
}

// Integrity runs PRAGMA integrity_check.
func (s *Store) Integrity(ctx context.Context) error {
	pool := s.RO
	if pool == nil {
		pool = s.RW
	}
	var res string
	if err := pool.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&res); err != nil {
		return err
	}
	if res != "ok" {
		return fmt.Errorf("integrity_check: %s", res)
	}
	return nil
}
