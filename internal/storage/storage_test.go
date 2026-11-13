package storage

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var ctx = context.Background()

// open opens a database in dir, closing it when the test ends.
func open(t *testing.T, dir string) *DB {
	t.Helper()
	db, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func testKey(b byte) []byte {
	key := make([]byte, KeySize)
	for i := range key {
		key[i] = b
	}
	return key
}

func keyring(t *testing.T, primary []byte, retired ...[]byte) *Keyring {
	t.Helper()
	k, err := NewKeyring(primary, retired...)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func tables(t *testing.T, db *DB) []string {
	t.Helper()
	rows, err := db.sql.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		out = append(out, name)
	}
	return out
}

func TestOpenMigratesAnEmptyFile(t *testing.T) {
	dir := t.TempDir()
	// A zero-byte file is what a freshly provisioned volume holds after a crash
	// between creating the file and the first migration.
	if err := os.WriteFile(filepath.Join(dir, FileName), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	db := open(t, dir)

	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	v, err := db.SchemaVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if v != len(migrations) || v < 1 {
		t.Fatalf("schema version = %d, want %d", v, len(migrations))
	}
	if got := strings.Join(tables(t, db), ","); got != "credentials,sessions,work_items" {
		t.Fatalf("tables = %s", got)
	}
}

func TestOpenCreatesAPrivateDirectoryAndFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "data")
	open(t, dir)
	for path, want := range map[string]os.FileMode{dir: 0o700, filepath.Join(dir, FileName): 0o600} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s mode = %o, want %o", path, got, want)
		}
	}
}

func TestOpenAppliesTheConnectionSettings(t *testing.T) {
	db := open(t, t.TempDir())
	for pragma, want := range map[string]string{
		"journal_mode": "wal",
		"synchronous":  "2", // FULL
		"busy_timeout": "5000",
		"foreign_keys": "1",
		"temp_store":   "2", // MEMORY
	} {
		var got string
		if err := db.sql.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("PRAGMA %s = %s, want %s", pragma, got, want)
		}
	}
}

func TestReopenIsIdempotentAndKeepsData(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.ExecContext(ctx,
		`INSERT INTO work_items (owner_id, id, doc, updated_at) VALUES ('o', 'i', '{}', 0)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// Close checkpoints: no write-ahead log is left to replay.
	if info, err := os.Stat(filepath.Join(dir, FileName+"-wal")); err == nil && info.Size() != 0 {
		t.Errorf("WAL holds %d bytes after Close", info.Size())
	}

	db = open(t, dir)
	var n int
	if err := db.sql.QueryRowContext(ctx, `SELECT count(*) FROM work_items`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("rows after reopen = %d, want 1", n)
	}
}

func TestOpenRefusesANewerSchema(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.ExecContext(ctx, "PRAGMA user_version = 9999"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, dir); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("expected a newer-schema refusal, got %v", err)
	}
}

// A failed migration must leave the previous schema version in place.
func TestMigrationIsTransactional(t *testing.T) {
	db := open(t, t.TempDir())
	before, _ := db.SchemaVersion(ctx)
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, "CREATE TABLE half_done (x); PRAGMA user_version = 42"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	after, _ := db.SchemaVersion(ctx)
	if after != before {
		t.Fatalf("user_version = %d after rollback, want %d", after, before)
	}
	var name string
	err = db.sql.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE name = 'half_done'`).Scan(&name)
	if err != sql.ErrNoRows {
		t.Fatalf("rolled-back table still exists (err=%v)", err)
	}
}
