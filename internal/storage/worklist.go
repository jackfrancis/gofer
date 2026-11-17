package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackfrancis/gofer/internal/worklist"
)

// WorklistStore is the SQLite worklist.Store and worklist.Lister. Each WorkItem is
// a JSON document keyed by (owner, id); its semantics match worklist.MemoryStore.
type WorklistStore struct {
	db *DB
}

// Worklist returns the worklist store over this database.
func (db *DB) Worklist() *WorklistStore { return &WorklistStore{db: db} }

var (
	_ worklist.Store  = (*WorklistStore)(nil)
	_ worklist.Lister = (*WorklistStore)(nil)
)

// List returns the owner's work items in first-insertion order. An owner with no
// items yields an empty, non-nil slice.
func (s *WorklistStore) List(ctx context.Context, ownerID string) ([]worklist.WorkItem, error) {
	return listOwner(ctx, s.db.sql, ownerID)
}

// queryer is what listOwner reads through: the pool, or an open transaction.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// listOwner reads an owner's items in first-insertion order.
func listOwner(ctx context.Context, q queryer, ownerID string) ([]worklist.WorkItem, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT doc FROM work_items WHERE owner_id = ? ORDER BY seq`, ownerID)
	if err != nil {
		return nil, fmt.Errorf("storage: list work items: %w", err)
	}
	defer rows.Close()
	out := []worklist.WorkItem{}
	for rows.Next() {
		var doc []byte
		if err := rows.Scan(&doc); err != nil {
			return nil, fmt.Errorf("storage: list work items: %w", err)
		}
		it, err := decodeItem(doc, ownerID)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: list work items: %w", err)
	}
	return out, nil
}

// All returns every owner's items, for the staleness reconciler. It is the
// cross-owner read behind worklist.Lister.
func (s *WorklistStore) All(ctx context.Context) (map[string][]worklist.WorkItem, error) {
	rows, err := s.db.sql.QueryContext(ctx,
		`SELECT owner_id, doc FROM work_items ORDER BY seq`)
	if err != nil {
		return nil, fmt.Errorf("storage: list all work items: %w", err)
	}
	defer rows.Close()
	out := map[string][]worklist.WorkItem{}
	for rows.Next() {
		var (
			owner string
			doc   []byte
		)
		if err := rows.Scan(&owner, &doc); err != nil {
			return nil, fmt.Errorf("storage: list all work items: %w", err)
		}
		it, err := decodeItem(doc, owner)
		if err != nil {
			return nil, err
		}
		out[owner] = append(out[owner], it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: list all work items: %w", err)
	}
	return out, nil
}

// Upsert adds or replaces an owner's items keyed by WorkItem.ID, in one
// transaction: every item lands or none does. Every item is re-scoped to ownerID
// so an agent ingestion call cannot write across users. It is a blind write; see
// Update for read-modify-write.
func (s *WorklistStore) Upsert(ctx context.Context, ownerID string, items ...worklist.WorkItem) error {
	if len(items) == 0 {
		return nil
	}
	// Encode before taking the write lock, so the lock is held only for the writes.
	docs, err := encodeItems(ownerID, items)
	if err != nil {
		return err
	}
	tx, err := s.db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("storage: upsert work items: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit
	if err := writeItems(ctx, tx, ownerID, items, docs); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: upsert work items: %w", err)
	}
	return nil
}

// Update reads, changes and writes an owner's items in one transaction. The
// connection is opened with _txlock=immediate, so the transaction takes SQLite's
// write lock when it begins: no other write can land between the read and the
// write, from this process or any other.
func (s *WorklistStore) Update(ctx context.Context, ownerID string, fn func([]worklist.WorkItem) ([]worklist.WorkItem, error)) error {
	tx, err := s.db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("storage: update work items: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit
	current, err := listOwner(ctx, tx, ownerID)
	if err != nil {
		return err
	}
	items, err := fn(current)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return nil
	}
	docs, err := encodeItems(ownerID, items)
	if err != nil {
		return err
	}
	if err := writeItems(ctx, tx, ownerID, items, docs); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: update work items: %w", err)
	}
	return nil
}

// encodeItems re-scopes each item to ownerID and encodes it as a JSON document.
func encodeItems(ownerID string, items []worklist.WorkItem) ([][]byte, error) {
	docs := make([][]byte, len(items))
	for i, it := range items {
		it.OwnerID = ownerID
		doc, err := json.Marshal(it)
		if err != nil {
			return nil, fmt.Errorf("storage: encode work item %q: %w", it.ID, err)
		}
		docs[i] = doc
	}
	return docs, nil
}

// writeItems adds or replaces items by (owner, id) inside tx. A replaced item keeps
// its first-insertion position.
func writeItems(ctx context.Context, tx *sql.Tx, ownerID string, items []worklist.WorkItem, docs [][]byte) error {
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO work_items (owner_id, id, doc, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (owner_id, id) DO UPDATE SET doc = excluded.doc, updated_at = excluded.updated_at`)
	if err != nil {
		return fmt.Errorf("storage: write work items: %w", err)
	}
	defer stmt.Close()
	now := time.Now().Unix()
	for i, it := range items {
		// string(doc) so the column holds TEXT (readable with the sqlite3 CLI and
		// usable with SQLite's JSON functions), not a BLOB.
		if _, err := stmt.ExecContext(ctx, ownerID, it.ID, string(docs[i]), now); err != nil {
			return fmt.Errorf("storage: write work item %q: %w", it.ID, err)
		}
	}
	return nil
}

// decodeItem parses a stored document and re-asserts its owner from the row, so a
// document can never claim an owner other than the one it is filed under.
func decodeItem(doc []byte, ownerID string) (worklist.WorkItem, error) {
	var it worklist.WorkItem
	if err := json.Unmarshal(doc, &it); err != nil {
		return worklist.WorkItem{}, fmt.Errorf("storage: decode work item: %w", err)
	}
	it.OwnerID = ownerID
	return it, nil
}
