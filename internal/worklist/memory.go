package worklist

import (
	"context"
	"sync"
)

// MemoryStore is an in-memory Store for development and tests. The cloud
// persistence backend will implement the same Store interface.
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
	src := s.items[ownerID]
	out := make([]WorkItem, len(src))
	copy(out, src)
	return out, nil
}

// Seed replaces the items for an owner. Development and test helper.
func (s *MemoryStore) Seed(ownerID string, items ...WorkItem) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[ownerID] = items
}

// All returns a copy of every owner's items, for the staleness reconciler. It is
// the cross-owner read behind worklist.Lister.
func (s *MemoryStore) All(_ context.Context) (map[string][]WorkItem, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string][]WorkItem, len(s.items))
	for owner, src := range s.items {
		items := make([]WorkItem, len(src))
		copy(items, src)
		out[owner] = items
	}
	return out, nil
}

// Upsert adds or replaces an owner's items keyed by WorkItem.ID. Every item is
// re-scoped to ownerID so an agent ingestion call cannot write across users.
func (s *MemoryStore) Upsert(_ context.Context, ownerID string, items ...WorkItem) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing := s.items[ownerID]
	index := make(map[string]int, len(existing))
	for i, it := range existing {
		index[it.ID] = i
	}
	for _, it := range items {
		it.OwnerID = ownerID
		if i, ok := index[it.ID]; ok {
			existing[i] = it
			continue
		}
		index[it.ID] = len(existing)
		existing = append(existing, it)
	}
	s.items[ownerID] = existing
	return nil
}
