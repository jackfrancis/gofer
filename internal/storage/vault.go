package storage

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"time"

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

var _ vault.Vault = (*VaultStore)(nil)

// sealedCredential is the plaintext that gets sealed: the whole credential, so
// nothing about a token (not even its expiry) is stored in the clear.
type sealedCredential struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	TokenType    string    `json:"token_type,omitempty"`
	Expiry       time.Time `json:"expiry"`
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
	if userID == "" || cred.Provider == "" {
		return errors.New("vault: userID and provider are required")
	}
	plaintext, err := json.Marshal(sealedCredential{
		AccessToken:  cred.AccessToken,
		RefreshToken: cred.RefreshToken,
		TokenType:    cred.TokenType,
		Expiry:       cred.Expiry,
	})
	if err != nil {
		return fmt.Errorf("storage: encode credential: %w", err)
	}
	keyID, sealed := v.keys.Seal(plaintext, credentialAAD(userID, cred.Provider))
	_, err = v.db.sql.ExecContext(ctx, `
		INSERT INTO credentials (user_id, provider, key_id, sealed, updated_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (user_id, provider) DO UPDATE SET
			key_id = excluded.key_id, sealed = excluded.sealed, updated_at = excluded.updated_at`,
		userID, cred.Provider, keyID, sealed, time.Now().Unix())
	if err != nil {
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
		Provider:     provider,
		AccessToken:  sc.AccessToken,
		RefreshToken: sc.RefreshToken,
		TokenType:    sc.TokenType,
		Expiry:       sc.Expiry,
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
