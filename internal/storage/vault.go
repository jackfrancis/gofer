package storage

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackfrancis/gofer/internal/session"
	"github.com/jackfrancis/gofer/internal/vault"
)

// VaultStore is the SQLite vault.Vault. Every credential is sealed with the
// keyring before it is written; the database never holds a token in the clear.
type VaultStore struct {
	db   *DB
	keys *Keyring
}

// Vault returns the credential vault over this database, sealing with keys.
func (db *DB) Vault(keys *Keyring) *VaultStore { return &VaultStore{db: db, keys: keys} }

var (
	_ vault.Vault        = (*VaultStore)(nil)
	_ vault.LogoutLedger = (*VaultStore)(nil)
)

// logoutMarkerPrefix is the reserved provider prefix of a pending-logout marker
// (vault.LogoutLedger), kept in the credentials table so it needs no schema change:
// a migration would stop an older binary from starting against this database, and
// older code never reads a provider it does not know. The marker is sealed like any
// row, though it holds nothing secret.
const logoutMarkerPrefix = "logout-pending:"

// sealedCredential is the plaintext that gets sealed: the whole credential, so
// nothing about a token (not even its expiry) is stored in the clear.
// Fields added after the first release are omitempty/omitzero, so a row written
// before them decodes with them empty (ADR 0007: no schema migration).
type sealedCredential struct {
	AccessToken   string    `json:"access_token"`
	RefreshToken  string    `json:"refresh_token,omitempty"`
	TokenType     string    `json:"token_type,omitempty"`
	Expiry        time.Time `json:"expiry"`
	RefreshExpiry time.Time `json:"refresh_expiry,omitzero"`
	ClientID      string    `json:"client_id,omitempty"`
	Version       string    `json:"version,omitempty"`
	Login         string    `json:"login,omitempty"`
}

// credentialAAD is the associated data bound to a sealed credential: a domain tag
// and the length-prefixed user and provider. A sealed blob moved to another row
// fails authentication. Length prefixes keep ("ab","c") distinct from ("a","bc").
func credentialAAD(userID, provider string) []byte {
	aad := []byte("gofer/vault/credential/v1")
	for _, s := range []string{userID, provider} {
		aad = binary.BigEndian.AppendUint32(aad, uint32(len(s)))
		aad = append(aad, s...)
	}
	return aad
}

// Put seals and stores the credential for a user and provider, replacing any
// existing one. It always seals under the primary key, so a Put re-keys the row.
func (v *VaultStore) Put(ctx context.Context, userID string, cred vault.Credential) error {
	return v.putWith(ctx, v.db.sql, userID, cred)
}

// execer is a *sql.DB or a *sql.Tx.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// upsertRow writes one credentials row.
const upsertRow = `
		INSERT INTO credentials (user_id, provider, key_id, sealed, updated_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (user_id, provider) DO UPDATE SET
			key_id = excluded.key_id, sealed = excluded.sealed, updated_at = excluded.updated_at`

func (v *VaultStore) putWith(ctx context.Context, ex execer, userID string, cred vault.Credential) error {
	if userID == "" || cred.Provider == "" {
		return errors.New("vault: userID and provider are required")
	}
	if strings.HasPrefix(cred.Provider, logoutMarkerPrefix) {
		return errors.New("vault: reserved provider name")
	}
	plaintext, err := json.Marshal(sealedCredential{
		AccessToken:   cred.AccessToken,
		RefreshToken:  cred.RefreshToken,
		TokenType:     cred.TokenType,
		Expiry:        cred.Expiry,
		RefreshExpiry: cred.RefreshExpiry,
		ClientID:      cred.ClientID,
		Version:       cred.Version,
		Login:         cred.Login,
	})
	if err != nil {
		return fmt.Errorf("storage: encode credential: %w", err)
	}
	keyID, sealed := v.keys.Seal(plaintext, credentialAAD(userID, cred.Provider))
	if _, err := ex.ExecContext(ctx, upsertRow, userID, cred.Provider, keyID, sealed, time.Now().Unix()); err != nil {
		return fmt.Errorf("storage: store credential: %w", err)
	}
	return nil
}

// Get returns the stored credential for a user and provider, or vault.ErrNotFound.
// A row that cannot be opened (unknown key, failed authentication) is an error,
// not ErrNotFound: the credential exists but this process cannot read it.
func (v *VaultStore) Get(ctx context.Context, userID, provider string) (vault.Credential, error) {
	var (
		keyID  string
		sealed []byte
	)
	err := v.db.sql.QueryRowContext(ctx,
		`SELECT key_id, sealed FROM credentials WHERE user_id = ? AND provider = ?`,
		userID, provider).Scan(&keyID, &sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return vault.Credential{}, vault.ErrNotFound
	}
	if err != nil {
		return vault.Credential{}, fmt.Errorf("storage: load credential: %w", err)
	}
	plaintext, err := v.keys.Open(keyID, sealed, credentialAAD(userID, provider))
	if err != nil {
		return vault.Credential{}, fmt.Errorf("storage: open credential (key id %s): %w", keyID, err)
	}
	var sc sealedCredential
	if err := json.Unmarshal(plaintext, &sc); err != nil {
		// Do not wrap err: a JSON error can quote the input, which is a token.
		return vault.Credential{}, errors.New("storage: decode credential: malformed record")
	}
	return vault.Credential{
		Provider:      provider,
		AccessToken:   sc.AccessToken,
		RefreshToken:  sc.RefreshToken,
		TokenType:     sc.TokenType,
		Expiry:        sc.Expiry,
		RefreshExpiry: sc.RefreshExpiry,
		ClientID:      sc.ClientID,
		Version:       sc.Version,
		Login:         sc.Login,
	}, nil
}

// Delete removes the stored credential for a user and provider. It is a no-op
// (not an error) when none exists, so logout can call it unconditionally.
func (v *VaultStore) Delete(ctx context.Context, userID, provider string) error {
	if _, err := v.db.sql.ExecContext(ctx,
		`DELETE FROM credentials WHERE user_id = ? AND provider = ?`, userID, provider); err != nil {
		return fmt.Errorf("storage: delete credential: %w", err)
	}
	return nil
}

// MarkLogout records a pending logout for the user and provider.
func (v *VaultStore) MarkLogout(ctx context.Context, userID, provider string) error {
	return v.markWith(ctx, v.db.sql, userID, provider)
}

func (v *VaultStore) markWith(ctx context.Context, ex execer, userID, provider string) error {
	if userID == "" || provider == "" {
		return errors.New("vault: userID and provider are required")
	}
	marker := logoutMarkerPrefix + provider
	keyID, sealed := v.keys.Seal([]byte(`{}`), credentialAAD(userID, marker))
	if _, err := ex.ExecContext(ctx, upsertRow, userID, marker, keyID, sealed, time.Now().Unix()); err != nil {
		return fmt.Errorf("storage: mark logout: %w", err)
	}
	return nil
}

// MarkLogoutEndingSession consumes the session whose hashed id is sessionKey (the
// sessions table of this database) and records a pending logout, in one
// transaction: either this call removed the session and the logout is recorded, or
// neither happened. A session already gone (another logout of it got there first)
// is session.ErrNotFound, and nothing is recorded, so each browser session starts
// at most one logout. An empty sessionKey records the logout alone.
func (v *VaultStore) MarkLogoutEndingSession(ctx context.Context, sessionKey, userID, provider string) error {
	return v.inTx(ctx, func(tx *sql.Tx) error {
		if sessionKey != "" {
			res, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE id_hash = ?`, sessionKey)
			if err != nil {
				return fmt.Errorf("storage: delete session: %w", err)
			}
			if n, err := res.RowsAffected(); err != nil {
				return fmt.Errorf("storage: delete session: %w", err)
			} else if n == 0 {
				return session.ErrNotFound
			}
		}
		return v.markWith(ctx, tx, userID, provider)
	})
}

// PutClearingLogout stores a new credential and clears any pending logout for its
// user and provider, in one transaction, so a restart sees either the pending
// logout or the sign-in that replaced it, never the new credential paired with the
// old logout.
func (v *VaultStore) PutClearingLogout(ctx context.Context, userID string, cred vault.Credential) error {
	return v.inTx(ctx, func(tx *sql.Tx) error {
		if err := v.putWith(ctx, tx, userID, cred); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM credentials WHERE user_id = ? AND provider = ?`, userID, logoutMarkerPrefix+cred.Provider); err != nil {
			return fmt.Errorf("storage: clear logout: %w", err)
		}
		return nil
	})
}

// inTx runs fn in one transaction, committing only if it succeeds.
func (v *VaultStore) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := v.db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("storage: begin: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: commit: %w", err)
	}
	return nil
}

// ClearLogout removes a pending logout; a missing one is not an error.
func (v *VaultStore) ClearLogout(ctx context.Context, userID, provider string) error {
	if _, err := v.db.sql.ExecContext(ctx,
		`DELETE FROM credentials WHERE user_id = ? AND provider = ?`, userID, logoutMarkerPrefix+provider); err != nil {
		return fmt.Errorf("storage: clear logout: %w", err)
	}
	return nil
}

// LogoutPending reports whether a logout is pending for the user and provider.
func (v *VaultStore) LogoutPending(ctx context.Context, userID, provider string) (bool, error) {
	var one int
	err := v.db.sql.QueryRowContext(ctx,
		`SELECT 1 FROM credentials WHERE user_id = ? AND provider = ?`, userID, logoutMarkerPrefix+provider).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("storage: read logout marker: %w", err)
	}
	return true, nil
}

// PendingLogouts lists every pending logout.
func (v *VaultStore) PendingLogouts(ctx context.Context) ([]vault.LogoutKey, error) {
	rows, err := v.db.sql.QueryContext(ctx,
		`SELECT user_id, provider FROM credentials WHERE substr(provider, 1, ?) = ?`,
		len(logoutMarkerPrefix), logoutMarkerPrefix)
	if err != nil {
		return nil, fmt.Errorf("storage: list logout markers: %w", err)
	}
	defer rows.Close()
	var out []vault.LogoutKey
	for rows.Next() {
		var k vault.LogoutKey
		if err := rows.Scan(&k.UserID, &k.Provider); err != nil {
			return nil, fmt.Errorf("storage: list logout markers: %w", err)
		}
		k.Provider = strings.TrimPrefix(k.Provider, logoutMarkerPrefix)
		out = append(out, k)
	}
	return out, rows.Err()
}
