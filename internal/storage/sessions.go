package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackfrancis/gofer/internal/session"
)

// SessionStore is the SQLite session.Store. The key it is given is already the
// hash of the session id (session.Manager hashes before it calls a Store), so the
// database never sees a value that works as a cookie.
type SessionStore struct {
	db *DB
}

// Sessions returns the session store over this database.
func (db *DB) Sessions() *SessionStore { return &SessionStore{db: db} }

var _ session.Store = (*SessionStore)(nil)

// Save stores an authenticated session under key until expiresAt.
func (s *SessionStore) Save(ctx context.Context, key string, u session.User, expiresAt time.Time) error {
	doc, err := json.Marshal(u)
	if err != nil {
		return fmt.Errorf("storage: encode session: %w", err)
	}
	_, err = s.db.sql.ExecContext(ctx, `
		INSERT INTO sessions (id_hash, user, expires_at) VALUES (?, ?, ?)
		ON CONFLICT (id_hash) DO UPDATE SET user = excluded.user, expires_at = excluded.expires_at`,
		key, string(doc), expiresAt.Unix())
	if err != nil {
		return fmt.Errorf("storage: save session: %w", err)
	}
	return nil
}

// Load returns the session's user and expiry, or session.ErrNotFound. It does not
// judge expiry; the manager does, and DeleteExpired removes stale rows.
func (s *SessionStore) Load(ctx context.Context, key string) (session.User, time.Time, error) {
	var (
		doc     []byte
		expires int64
	)
	err := s.db.sql.QueryRowContext(ctx,
		`SELECT user, expires_at FROM sessions WHERE id_hash = ?`, key).Scan(&doc, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return session.User{}, time.Time{}, session.ErrNotFound
	}
	if err != nil {
		return session.User{}, time.Time{}, fmt.Errorf("storage: load session: %w", err)
	}
	var u session.User
	if err := json.Unmarshal(doc, &u); err != nil {
		return session.User{}, time.Time{}, fmt.Errorf("storage: decode session: %w", err)
	}
	return u, time.Unix(expires, 0), nil
}

// Delete removes a session; a missing one is not an error.
func (s *SessionStore) Delete(ctx context.Context, key string) error {
	if _, err := s.db.sql.ExecContext(ctx, `DELETE FROM sessions WHERE id_hash = ?`, key); err != nil {
		return fmt.Errorf("storage: delete session: %w", err)
	}
	return nil
}

// DeleteExpired removes every session that expired at or before now.
func (s *SessionStore) DeleteExpired(ctx context.Context, now time.Time) error {
	if _, err := s.db.sql.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, now.Unix()); err != nil {
		return fmt.Errorf("storage: expire sessions: %w", err)
	}
	return nil
}
