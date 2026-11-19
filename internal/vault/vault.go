// Package vault stores delegated provider credentials (OAuth tokens) obtained
// with a user's consent. gofer's agent-plane credential broker
// (api.CredentialHandler, GET /agent/credential) reads it to vend a short-lived
// provider token to a runtime on demand, so the runtime never holds a standing
// secret: gofer is the durable holder; the runtime holds a vended credential for
// the run only.
package vault

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrNotFound is returned when no credential exists for a user/provider.
var ErrNotFound = errors.New("vault: credential not found")

// ErrSignInRequired reports that gofer holds no usable credential for the user: none
// is stored, the stored one was issued by a different OAuth client than the one
// configured now, or GitHub has said it is dead (revoked, or its refresh token spent
// or expired). Only the user signing in again fixes it. The credential broker
// answers it with a distinct status, and a run that gets it stops asking GitHub.
var ErrSignInRequired = errors.New("sign-in required")

// Credential is a delegated provider credential held on a user's behalf.
type Credential struct {
	Provider     string
	AccessToken  string
	RefreshToken string
	TokenType    string
	Expiry       time.Time
	// RefreshExpiry is when the refresh token stops working, when the provider says
	// (GitHub's refresh_token_expires_in); zero when unknown or there is none.
	RefreshExpiry time.Time
	// ClientID is the OAuth client that issued the credential. Empty on a row stored
	// before gofer recorded it. A credential from another client is never vended.
	ClientID string
	// Version is a random ID minted at every write of the credential (sign-in,
	// refresh, legacy tagging). A conditional write or delete applies only while the
	// row still holds the version it read, and a run names the version GitHub
	// rejected. Not a secret. Empty on a row stored before gofer recorded it.
	Version string
}

// Vault stores and retrieves delegated provider credentials, scoped by user.
type Vault interface {
	Put(ctx context.Context, userID string, cred Credential) error
	Get(ctx context.Context, userID, provider string) (Credential, error)
	Delete(ctx context.Context, userID, provider string) error
}

// MemoryVault is an in-memory Vault for development and tests. The KMS-backed
// backend will implement the same interface.
type MemoryVault struct {
	mu    sync.RWMutex
	creds map[string]map[string]Credential // userID -> provider -> credential
}

// NewMemoryVault returns an empty in-memory vault.
func NewMemoryVault() *MemoryVault {
	return &MemoryVault{creds: make(map[string]map[string]Credential)}
}

// Put stores the credential for a user and provider, replacing any existing one.
func (v *MemoryVault) Put(_ context.Context, userID string, cred Credential) error {
	if userID == "" || cred.Provider == "" {
		return errors.New("vault: userID and provider are required")
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	byProvider := v.creds[userID]
	if byProvider == nil {
		byProvider = make(map[string]Credential)
		v.creds[userID] = byProvider
	}
	byProvider[cred.Provider] = cred
	return nil
}

// Get returns the stored credential for a user and provider, or ErrNotFound.
func (v *MemoryVault) Get(_ context.Context, userID, provider string) (Credential, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	cred, ok := v.creds[userID][provider]
	if !ok {
		return Credential{}, ErrNotFound
	}
	return cred, nil
}

// Delete removes the stored credential for a user and provider. It is a no-op
// (not an error) when none exists, so logout can call it unconditionally.
func (v *MemoryVault) Delete(_ context.Context, userID, provider string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if byProvider := v.creds[userID]; byProvider != nil {
		delete(byProvider, provider)
		if len(byProvider) == 0 {
			delete(v.creds, userID)
		}
	}
	return nil
}
