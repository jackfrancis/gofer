package worklist

import (
	"context"
	"encoding/json"
	"sync"
)

// MemoryStore is an in-memory Store for development and tests, with the same
// semantics as the SQLite store (internal/storage). Like a database, it never shares
// memory with its callers: every item goes in and comes out as a full copy, so a
// caller mutating what it read — a slice, a pointer — cannot change what is stored.
type MemoryStore struct {
	mu    sync.RWMutex
	items map[string][]WorkItem // ownerID -> items
}

// NewMemoryStore returns an empty in-memory store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{items: make(map[string][]WorkItem)}
}

var (
	_ Store  = (*MemoryStore)(nil)
	_ Lister = (*MemoryStore)(nil)
)

// List returns a copy of the owner's work items.
func (s *MemoryStore) List(_ context.Context, ownerID string) ([]WorkItem, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneItems(s.items[ownerID]), nil
}

// Seed replaces the items for an owner. Development and test helper.
func (s *MemoryStore) Seed(ownerID string, items ...WorkItem) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[ownerID] = cloneItems(items)
}

// All returns a copy of every owner's items, for the staleness reconciler. It is
// the cross-owner read behind worklist.Lister.
func (s *MemoryStore) All(_ context.Context) (map[string][]WorkItem, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string][]WorkItem, len(s.items))
	for owner, src := range s.items {
		out[owner] = cloneItems(src)
	}
	return out, nil
}

// Upsert adds or replaces an owner's items keyed by WorkItem.ID. Every item is
// re-scoped to ownerID so an agent ingestion call cannot write across users.
func (s *MemoryStore) Upsert(_ context.Context, ownerID string, items ...WorkItem) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.upsertLocked(ownerID, items)
	return nil
}

// Update reads, changes and writes an owner's items under the store's lock, so no
// other write interleaves. fn gets full copies, so a change it does not return — or
// returns alongside an error — leaves the store untouched.
func (s *MemoryStore) Update(_ context.Context, ownerID string, fn func([]WorkItem) ([]WorkItem, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	items, err := fn(cloneItems(s.items[ownerID]))
	if err != nil {
		return err
	}
	s.upsertLocked(ownerID, items)
	return nil
}

// upsertLocked adds or replaces items by ID; the caller holds s.mu.
func (s *MemoryStore) upsertLocked(ownerID string, items []WorkItem) {
	existing := s.items[ownerID]
	index := make(map[string]int, len(existing))
	for i, it := range existing {
		index[it.ID] = i
	}
	for _, it := range cloneItems(items) {
		it.OwnerID = ownerID
		if i, ok := index[it.ID]; ok {
			existing[i] = it
			continue
		}
		index[it.ID] = len(existing)
		existing = append(existing, it)
	}
	s.items[ownerID] = existing
}

// cloneItems returns full copies of items, sharing no slices, maps or pointers with
// them. It goes through JSON, the same encoding the SQLite store keeps, so the two
// stores round-trip an item identically.
func cloneItems(items []WorkItem) []WorkItem {
	out := make([]WorkItem, len(items))
	for i, it := range items {
		doc, err := json.Marshal(it)
		if err != nil {
			panic("worklist: encode work item: " + err.Error())
		}
		if err := json.Unmarshal(doc, &out[i]); err != nil {
			panic("worklist: decode work item: " + err.Error())
		}
	}
	return out
}
