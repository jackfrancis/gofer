// Package storage is gofer's durable web-tier state: one SQLite database file
// holding the worklist, the vault of delegated provider credentials (encrypted at
// rest), and authenticated sessions. It implements worklist.Store/Lister,
// vault.Vault and session.Store, so nothing else in gofer knows the state is in
// SQLite. See docs/adr/0004-sqlite-persistent-storage.md.
//
// The driver is modernc.org/sqlite, a pure-Go SQLite, so the web binary still
// builds with CGO_ENABLED=0 for the distroless static image. This is the only
// package that imports it.
package storage

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

// FileName is the database file inside the data directory.
const FileName = "gofer.db"

// maxConns caps the pool. WAL allows many readers beside one writer; each
// connection holds its own page cache, so the cap bounds memory.
const maxConns = 8

//go:embed migrations/*.sql
var migrationFS embed.FS

// DB is the open database. Its stores share one connection pool.
type DB struct {
	sql *sql.DB
}

// Open opens (creating if needed) the database in dir and applies any pending
// schema migrations. The directory is created 0700 and the file 0600: it holds
// personal data and sealed credentials.
func Open(ctx context.Context, dir string) (*DB, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("storage: create data directory: %w", err)
	}
	path, err := filepath.Abs(filepath.Join(dir, FileName))
	if err != nil {
		return nil, fmt.Errorf("storage: resolve database path: %w", err)
	}
	// Create the file ourselves so its mode is 0600 (SQLite would use 0644); the
	// -wal and -shm files inherit the main file's mode.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("storage: create database file: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("storage: create database file: %w", err)
	}

	conn, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("storage: open database: %w", err)
	}
	conn.SetMaxOpenConns(maxConns)
	db := &DB{sql: conn}
	if err := db.migrate(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return db, nil
}

// dsn builds the connection string. The pragmas run on every pooled connection, in
// this order (busy_timeout first, so switching to WAL waits for a lock instead of
// failing). The rationale for each is in ADR 0004.
func dsn(path string) string {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(FULL)")
	q.Add("_pragma", "foreign_keys(ON)")
	q.Add("_pragma", "temp_store(MEMORY)")
	// Write transactions take the write lock at BEGIN, so busy_timeout applies to
	// them; a deferred transaction that upgrades to a writer fails immediately.
	q.Set("_txlock", "immediate")
	u := url.URL{Scheme: "file", OmitHost: true, Path: path, RawQuery: q.Encode()}
	return u.String()
}

// Close checkpoints the write-ahead log into the database file and closes the
// pool. Call it after the HTTP server has drained.
func (db *DB) Close() error {
	// Best effort: SQLite also checkpoints when the last connection closes, and an
	// un-checkpointed WAL is replayed on the next open either way.
	_, _ = db.sql.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	return db.sql.Close()
}

// SchemaVersion reports the applied schema version (PRAGMA user_version).
func (db *DB) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	err := db.sql.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v)
	return v, err
}

type migration struct {
	version int
	name    string
	sql     string
}

// loadMigrations reads the embedded NNNN_name.sql files in version order.
func loadMigrations() ([]migration, error) {
	names, err := fs.Glob(migrationFS, "migrations/*.sql")
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	out := make([]migration, 0, len(names))
	for i, name := range names {
		base := filepath.Base(name)
		num, _, ok := strings.Cut(base, "_")
		v, err := strconv.Atoi(num)
		if !ok || err != nil {
			return nil, fmt.Errorf("storage: migration %q is not named NNNN_name.sql", base)
		}
		if v != i+1 {
			return nil, fmt.Errorf("storage: migration %q is out of sequence (want version %d)", base, i+1)
		}
		body, err := migrationFS.ReadFile(name)
		if err != nil {
			return nil, err
		}
		out = append(out, migration{version: v, name: base, sql: string(body)})
	}
	return out, nil
}

// migrate brings the schema up to the newest embedded version. Each migration runs
// in one transaction with its version bump, so a crash leaves the previous version
// intact. A database newer than this binary is refused.
func (db *DB) migrate(ctx context.Context) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}
	current, err := db.SchemaVersion(ctx)
	if err != nil {
		return fmt.Errorf("storage: read schema version: %w", err)
	}
	if current > len(migrations) {
		return fmt.Errorf("storage: database schema version %d is newer than this binary supports (%d)", current, len(migrations))
	}
	for _, m := range migrations[current:] {
		tx, err := db.sql.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("storage: begin migration %s: %w", m.name, err)
		}
		if _, err := tx.ExecContext(ctx, m.sql); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("storage: apply migration %s: %w", m.name, err)
		}
		// PRAGMA takes no bind parameters; the version is an integer we parsed.
		if _, err := tx.ExecContext(ctx, "PRAGMA user_version = "+strconv.Itoa(m.version)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("storage: record migration %s: %w", m.name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("storage: commit migration %s: %w", m.name, err)
		}
	}
	return nil
}
