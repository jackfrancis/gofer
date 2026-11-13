package session

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrNotFound is returned by Store.Load when no session exists for a key.
var ErrNotFound = errors.New("session: not found")

// Store holds authenticated sessions. It is the seam that makes logins durable:
// the default is MemoryStore, and internal/storage supplies a SQLite one.
//
// key is always the hex SHA-256 of a session ID, never the ID itself (Manager
// hashes before every call), so an implementation never holds a value that works
// as a cookie. Pre-auth OAuth flow state never reaches a Store.
type Store interface {
	// Save stores the user under key until expiresAt, replacing any existing entry.
	Save(ctx context.Context, key string, u User, expiresAt time.Time) error
	// Load returns the user and expiry stored under key, or ErrNotFound. It may
	// return an expired entry; Manager checks the expiry.
	Load(ctx context.Context, key string) (User, time.Time, error)
	// Delete removes the entry under key; a missing entry is not an error.
	Delete(ctx context.Context, key string) error
	// DeleteExpired removes every entry that expired at or before now.
	DeleteExpired(ctx context.Context, now time.Time) error
}

// MemoryStore is the in-memory Store: sessions are lost on restart.
type MemoryStore struct {
	mu      sync.Mutex
	entries map[string]memoryEntry
}

type memoryEntry struct {
	user      User
	expiresAt time.Time
}

// NewMemoryStore returns an empty in-memory session store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{entries: make(map[string]memoryEntry)}
}

var _ Store = (*MemoryStore)(nil)

func (s *MemoryStore) Save(_ context.Context, key string, u User, expiresAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[key] = memoryEntry{user: u, expiresAt: expiresAt}
	return nil
}

func (s *MemoryStore) Load(_ context.Context, key string) (User, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[key]
	if !ok {
		return User{}, time.Time{}, ErrNotFound
	}
	return e.user, e.expiresAt, nil
}

func (s *MemoryStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, key)
	return nil
}

func (s *MemoryStore) DeleteExpired(_ context.Context, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, e := range s.entries {
		if !now.Before(e.expiresAt) {
			delete(s.entries, key)
		}
	}
	return nil
}
