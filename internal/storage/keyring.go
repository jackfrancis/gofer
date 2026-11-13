package storage

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

// KeySize is the size of a vault encryption key: AES-256.
const KeySize = 32

// ErrUnknownKey is returned when a record was sealed with a key this keyring does
// not hold (the key was rotated out, or the wrong key is configured).
var ErrUnknownKey = errors.New("storage: record is sealed with an unknown encryption key")

// ErrDecrypt is returned when a sealed record fails authentication: the
// ciphertext or its associated data was altered, or the key is wrong.
var ErrDecrypt = errors.New("storage: sealed record failed authentication")

// Keyring seals with one primary key and opens with the primary or any retired
// key, selected by the key id stored beside each record. That is what makes
// rotation possible: promote a new primary, keep the old key as retired, and
// records re-seal under the new key as they are next written.
type Keyring struct {
	primary string
	keys    map[string]cipher.AEAD
}

// NewKeyring builds a keyring. primary seals new records; retired keys only open
// existing ones. Every key must be KeySize bytes.
func NewKeyring(primary []byte, retired ...[]byte) (*Keyring, error) {
	k := &Keyring{keys: make(map[string]cipher.AEAD, 1+len(retired))}
	for i, key := range append([][]byte{primary}, retired...) {
		if len(key) != KeySize {
			return nil, fmt.Errorf("storage: encryption key must be %d bytes, got %d", KeySize, len(key))
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, err
		}
		// GCM with a fresh random 96-bit nonce per Seal, prepended to the output.
		aead, err := cipher.NewGCMWithRandomNonce(block)
		if err != nil {
			return nil, err
		}
		id := KeyID(key)
		k.keys[id] = aead
		if i == 0 {
			k.primary = id
		}
	}
	return k, nil
}

// KeyID is the identifier stored with each record sealed by key: the first 8 bytes
// of a domain-separated SHA-256 of the key. It is derived, so it cannot be
// misconfigured, and reveals nothing useful about a uniformly random key.
func KeyID(key []byte) string {
	h := sha256.New()
	h.Write([]byte("gofer-vault-key-id\x00"))
	h.Write(key)
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// PrimaryKeyID is the id of the key new records are sealed with.
func (k *Keyring) PrimaryKeyID() string { return k.primary }

// Seal encrypts plaintext under the primary key, binding aad. It returns the key
// id to store with the sealed bytes (nonce || ciphertext || tag).
func (k *Keyring) Seal(plaintext, aad []byte) (keyID string, sealed []byte) {
	return k.primary, k.keys[k.primary].Seal(nil, nil, plaintext, aad)
}

// Open decrypts sealed with the key named by keyID, verifying aad.
func (k *Keyring) Open(keyID string, sealed, aad []byte) ([]byte, error) {
	aead, ok := k.keys[keyID]
	if !ok {
		return nil, ErrUnknownKey
	}
	plaintext, err := aead.Open(nil, nil, sealed, aad)
	if err != nil {
		// The underlying error carries nothing useful and must not carry data.
		return nil, ErrDecrypt
	}
	return plaintext, nil
}
