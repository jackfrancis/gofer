package storage

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/jackfrancis/gofer/internal/vault"
)

func vaultImpls(t *testing.T) map[string]vault.Vault {
	return map[string]vault.Vault{
		"memory": vault.NewMemoryVault(),
		"sqlite": open(t, t.TempDir()).Vault(keyring(t, testKey(1))),
	}
}

func cred(access string) vault.Credential {
	return vault.Credential{
		Provider:     "github",
		AccessToken:  access,
		RefreshToken: "refresh-" + access,
		TokenType:    "bearer",
		Expiry:       time.Date(2027, 1, 2, 3, 4, 5, 0, time.UTC),
		// ADR 0007's fields, sealed with the rest.
		RefreshExpiry: time.Date(2027, 6, 2, 3, 4, 5, 0, time.UTC),
		ClientID:      "client-1",
		Version:       "v-" + access,
	}
}

func mustGet(t *testing.T, v vault.Vault, user, provider string) vault.Credential {
	t.Helper()
	got, err := v.Get(ctx, user, provider)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestVaultParity(t *testing.T) {
	cases := []struct {
		name string
		run  func(t *testing.T, v vault.Vault)
	}{
		{"get of a missing credential is ErrNotFound", func(t *testing.T, v vault.Vault) {
			if _, err := v.Get(ctx, "alice", "github"); !errors.Is(err, vault.ErrNotFound) {
				t.Fatalf("err = %v, want ErrNotFound", err)
			}
		}},
		{"put then get round-trips every field", func(t *testing.T, v vault.Vault) {
			want := cred("tok")
			if err := v.Put(ctx, "alice", want); err != nil {
				t.Fatal(err)
			}
			got := mustGet(t, v, "alice", "github")
			if !got.Expiry.Equal(want.Expiry) {
				t.Fatalf("expiry = %v, want %v", got.Expiry, want.Expiry)
			}
			if !got.RefreshExpiry.Equal(want.RefreshExpiry) {
				t.Fatalf("refresh expiry = %v, want %v", got.RefreshExpiry, want.RefreshExpiry)
			}
			got.Expiry, got.RefreshExpiry = want.Expiry, want.RefreshExpiry
			if got != want {
				t.Fatalf("got %+v, want %+v", got, want)
			}
		}},
		{"a zero expiry stays zero", func(t *testing.T, v vault.Vault) {
			_ = v.Put(ctx, "alice", vault.Credential{Provider: "github", AccessToken: "tok"})
			if got := mustGet(t, v, "alice", "github"); !got.Expiry.IsZero() || got.RefreshToken != "" {
				t.Fatalf("got %+v", got)
			}
		}},
		{"put replaces the existing credential", func(t *testing.T, v vault.Vault) {
			_ = v.Put(ctx, "alice", cred("old"))
			_ = v.Put(ctx, "alice", cred("new"))
			if got := mustGet(t, v, "alice", "github"); got.AccessToken != "new" || got.RefreshToken != "refresh-new" {
				t.Fatalf("got %+v", got)
			}
		}},
		{"credentials are scoped by user and by provider", func(t *testing.T, v vault.Vault) {
			_ = v.Put(ctx, "alice", cred("alice-gh"))
			_ = v.Put(ctx, "bob", cred("bob-gh"))
			google := cred("alice-google")
			google.Provider = "google"
			_ = v.Put(ctx, "alice", google)
			if got := mustGet(t, v, "alice", "github"); got.AccessToken != "alice-gh" {
				t.Fatalf("alice/github = %+v", got)
			}
			if got := mustGet(t, v, "bob", "github"); got.AccessToken != "bob-gh" {
				t.Fatalf("bob/github = %+v", got)
			}
			if got := mustGet(t, v, "alice", "google"); got.AccessToken != "alice-google" || got.Provider != "google" {
				t.Fatalf("alice/google = %+v", got)
			}
			if _, err := v.Get(ctx, "bob", "google"); !errors.Is(err, vault.ErrNotFound) {
				t.Fatalf("bob/google err = %v", err)
			}
		}},
		{"delete removes only that credential, and is a no-op when absent", func(t *testing.T, v vault.Vault) {
			_ = v.Put(ctx, "alice", cred("a"))
			_ = v.Put(ctx, "bob", cred("b"))
			if err := v.Delete(ctx, "alice", "github"); err != nil {
				t.Fatal(err)
			}
			if err := v.Delete(ctx, "alice", "github"); err != nil {
				t.Fatalf("second delete: %v", err)
			}
			if err := v.Delete(ctx, "nobody", "github"); err != nil {
				t.Fatalf("delete of an unknown user: %v", err)
			}
			if _, err := v.Get(ctx, "alice", "github"); !errors.Is(err, vault.ErrNotFound) {
				t.Fatalf("err = %v, want ErrNotFound", err)
			}
			if got := mustGet(t, v, "bob", "github"); got.AccessToken != "b" {
				t.Fatalf("bob = %+v", got)
			}
		}},
		{"put requires a user and a provider", func(t *testing.T, v vault.Vault) {
			if err := v.Put(ctx, "", cred("x")); err == nil {
				t.Fatal("expected an error for an empty user")
			}
			if err := v.Put(ctx, "alice", vault.Credential{AccessToken: "x"}); err == nil {
				t.Fatal("expected an error for an empty provider")
			}
		}},
	}
	for _, tc := range cases {
		for name, v := range vaultImpls(t) {
			t.Run(tc.name+"/"+name, func(t *testing.T) { tc.run(t, v) })
		}
	}
}

func TestVaultPersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Vault(keyring(t, testKey(1))).Put(ctx, "alice", cred("tok")); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// A new process: a new keyring built from the same key bytes.
	got := mustGet(t, open(t, dir).Vault(keyring(t, testKey(1))), "alice", "github")
	if got.AccessToken != "tok" || got.RefreshToken != "refresh-tok" {
		t.Fatalf("got %+v", got)
	}
}

// Nothing a token is made of may appear in the database file: not in the table,
// not in the write-ahead log.
func TestVaultTokensAreNotOnDiskInTheClear(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	c := vault.Credential{Provider: "github", AccessToken: "gho_ACCESS_SECRET_VALUE", RefreshToken: "ghr_REFRESH_SECRET_VALUE", TokenType: "bearer"}
	if err := db.Vault(keyring(t, testKey(1))).Put(ctx, "alice", c); err != nil {
		t.Fatal(err)
	}

	var keyID string
	var sealed []byte
	if err := db.sql.QueryRowContext(ctx, `SELECT key_id, sealed FROM credentials`).Scan(&keyID, &sealed); err != nil {
		t.Fatal(err)
	}
	if keyID != KeyID(testKey(1)) {
		t.Fatalf("stored key id = %q, want %q", keyID, KeyID(testKey(1)))
	}

	for _, name := range []string{FileName, FileName + "-wal"} {
		raw, err := readIfExists(dir, name)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{c.AccessToken, c.RefreshToken} {
			if bytes.Contains(raw, []byte(secret)) {
				t.Fatalf("%s contains a token in the clear", name)
			}
		}
	}
	_ = db.Close()
}

func TestVaultSealsWithAFreshNonce(t *testing.T) {
	db := open(t, t.TempDir())
	v := db.Vault(keyring(t, testKey(1)))
	read := func() []byte {
		var sealed []byte
		if err := db.sql.QueryRowContext(ctx, `SELECT sealed FROM credentials`).Scan(&sealed); err != nil {
			t.Fatal(err)
		}
		return sealed
	}
	_ = v.Put(ctx, "alice", cred("same"))
	first := read()
	_ = v.Put(ctx, "alice", cred("same"))
	if bytes.Equal(first, read()) {
		t.Fatal("sealing the same credential twice produced identical bytes")
	}
}

func TestVaultWrongKey(t *testing.T) {
	db := open(t, t.TempDir())
	if err := db.Vault(keyring(t, testKey(1))).Put(ctx, "alice", cred("tok")); err != nil {
		t.Fatal(err)
	}

	// A different key: the row names a key this keyring does not hold.
	_, err := db.Vault(keyring(t, testKey(2))).Get(ctx, "alice", "github")
	if !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("err = %v, want ErrUnknownKey", err)
	}
	if errors.Is(err, vault.ErrNotFound) {
		t.Fatal("an unreadable credential must not look like a missing one")
	}

	// A row relabelled with the wrong key's id is tried and fails authentication.
	if _, err := db.sql.ExecContext(ctx, `UPDATE credentials SET key_id = ?`, KeyID(testKey(2))); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Vault(keyring(t, testKey(2))).Get(ctx, "alice", "github"); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("err = %v, want ErrDecrypt", err)
	}

	// Signing in again (a Put) replaces the unreadable row under the new key.
	v2 := db.Vault(keyring(t, testKey(2)))
	if err := v2.Put(ctx, "alice", cred("fresh")); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, v2, "alice", "github"); got.AccessToken != "fresh" {
		t.Fatalf("got %+v", got)
	}
}

func TestVaultTamperedCiphertext(t *testing.T) {
	db := open(t, t.TempDir())
	v := db.Vault(keyring(t, testKey(1)))
	if err := v.Put(ctx, "alice", cred("tok")); err != nil {
		t.Fatal(err)
	}
	var sealed []byte
	if err := db.sql.QueryRowContext(ctx, `SELECT sealed FROM credentials`).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	// Flip one bit in the nonce, the ciphertext body, and the tag in turn; also
	// truncate. Every variant must fail authentication.
	variants := map[string][]byte{
		"nonce":     flip(sealed, 0),
		"body":      flip(sealed, len(sealed)/2),
		"tag":       flip(sealed, len(sealed)-1),
		"truncated": sealed[:len(sealed)-1],
		"empty":     {},
	}
	for name, bad := range variants {
		if _, err := db.sql.ExecContext(ctx, `UPDATE credentials SET sealed = ?`, bad); err != nil {
			t.Fatal(err)
		}
		if _, err := v.Get(ctx, "alice", "github"); !errors.Is(err, ErrDecrypt) {
			t.Errorf("%s: err = %v, want ErrDecrypt", name, err)
		}
	}
}

// The (user, provider) pair is bound as associated data: a sealed blob moved to
// another row does not open there.
func TestVaultSealedRecordIsBoundToItsUserAndProvider(t *testing.T) {
	db := open(t, t.TempDir())
	v := db.Vault(keyring(t, testKey(1)))
	_ = v.Put(ctx, "mallory", cred("mallory-token"))
	_ = v.Put(ctx, "alice", cred("alice-token"))

	// Mallory's row is overwritten with alice's sealed bytes.
	if _, err := db.sql.ExecContext(ctx, `
		UPDATE credentials SET sealed = (SELECT sealed FROM credentials WHERE user_id = 'alice')
		WHERE user_id = 'mallory'`); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Get(ctx, "mallory", "github"); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("user swap: err = %v, want ErrDecrypt", err)
	}

	// The same for the provider half of the pair.
	if _, err := db.sql.ExecContext(ctx, `UPDATE credentials SET provider = 'google' WHERE user_id = 'alice'`); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Get(ctx, "alice", "google"); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("provider swap: err = %v, want ErrDecrypt", err)
	}

	// The associated data is unambiguous about where the user ends.
	if bytes.Equal(credentialAAD("ab", "c"), credentialAAD("a", "bc")) {
		t.Fatal("associated data does not separate user from provider")
	}
}

// Rotation: a new primary key with the old one retired still opens old rows, and
// the next write re-seals the row under the new primary.
func TestVaultKeyRotation(t *testing.T) {
	db := open(t, t.TempDir())
	if err := db.Vault(keyring(t, testKey(1))).Put(ctx, "alice", cred("tok")); err != nil {
		t.Fatal(err)
	}

	rotated := db.Vault(keyring(t, testKey(2), testKey(1)))
	if got := mustGet(t, rotated, "alice", "github"); got.AccessToken != "tok" {
		t.Fatalf("got %+v", got)
	}
	if err := rotated.Put(ctx, "alice", cred("tok")); err != nil {
		t.Fatal(err)
	}
	var keyID string
	if err := db.sql.QueryRowContext(ctx, `SELECT key_id FROM credentials`).Scan(&keyID); err != nil {
		t.Fatal(err)
	}
	if keyID != KeyID(testKey(2)) {
		t.Fatalf("row not re-sealed under the new primary: key id %q", keyID)
	}
	// The retired key can now be dropped.
	if got := mustGet(t, db.Vault(keyring(t, testKey(2))), "alice", "github"); got.AccessToken != "tok" {
		t.Fatalf("got %+v", got)
	}
}

func TestKeyringRejectsBadKeys(t *testing.T) {
	for _, n := range []int{0, 16, 31, 33} {
		if _, err := NewKeyring(make([]byte, n)); err == nil {
			t.Errorf("a %d-byte key was accepted", n)
		}
	}
	if _, err := NewKeyring(testKey(1), make([]byte, 16)); err == nil {
		t.Error("a short retired key was accepted")
	}
	if KeyID(testKey(1)) == KeyID(testKey(2)) {
		t.Error("distinct keys share a key id")
	}
}

func flip(b []byte, i int) []byte {
	out := bytes.Clone(b)
	out[i] ^= 0x01
	return out
}

// A row sealed before ADR 0007 has no client_id, version or refresh expiry. It still
// opens, with those fields empty, so no migration is needed and the broker can tell
// it is a legacy row.
func TestVaultOpensALegacyRow(t *testing.T) {
	db := open(t, t.TempDir())
	keys := keyring(t, testKey(1))
	v := db.Vault(keys)
	legacy := []byte(`{"access_token":"gho_legacy","token_type":"bearer","expiry":"0001-01-01T00:00:00Z"}`)
	keyID, sealed := keys.Seal(legacy, credentialAAD("alice", "github"))
	if _, err := db.sql.ExecContext(ctx,
		`INSERT INTO credentials (user_id, provider, key_id, sealed, updated_at) VALUES (?, ?, ?, ?, ?)`,
		"alice", "github", keyID, sealed, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	got := mustGet(t, v, "alice", "github")
	if got.AccessToken != "gho_legacy" || got.ClientID != "" || got.Version != "" || !got.RefreshExpiry.IsZero() || !got.Expiry.IsZero() {
		t.Fatalf("legacy row = %+v", got)
	}
}

// The pending-logout ledger: marked, listed and cleared independently of the
// credential row, in both vaults, and durable across a reopen in SQLite. It lives
// under a reserved provider name that no credential can take.
func TestLogoutLedger(t *testing.T) {
	dir := t.TempDir()
	impls := map[string]vault.Vault{
		"memory": vault.NewMemoryVault(),
		"sqlite": open(t, dir).Vault(keyring(t, testKey(1))),
	}
	for name, v := range impls {
		t.Run(name, func(t *testing.T) {
			l := v.(vault.LogoutLedger)
			if err := v.Put(ctx, "alice", cred("tok")); err != nil {
				t.Fatal(err)
			}
			if p, err := l.LogoutPending(ctx, "alice", "github"); err != nil || p {
				t.Fatalf("pending before marking = %v, %v", p, err)
			}
			if err := l.MarkLogout(ctx, "alice", "github"); err != nil {
				t.Fatal(err)
			}
			if p, err := l.LogoutPending(ctx, "alice", "github"); err != nil || !p {
				t.Fatalf("pending after marking = %v, %v", p, err)
			}
			keys, err := l.PendingLogouts(ctx)
			if err != nil || len(keys) != 1 || keys[0] != (vault.LogoutKey{UserID: "alice", Provider: "github"}) {
				t.Fatalf("pending logouts = %v, %v", keys, err)
			}
			if mustGet(t, v, "alice", "github").AccessToken != "tok" {
				t.Fatal("marking touched the credential")
			}
			// Deleting the credential leaves the marker; clearing the marker leaves
			// the credential.
			if err := l.ClearLogout(ctx, "alice", "github"); err != nil {
				t.Fatal(err)
			}
			if p, _ := l.LogoutPending(ctx, "alice", "github"); p {
				t.Fatal("still pending after clearing")
			}
			mustGet(t, v, "alice", "github")
		})
	}

	// SQLite: a marker survives reopening the database, and the reserved name is not
	// a credential anyone can write.
	dir2 := t.TempDir()
	first := open(t, dir2)
	if err := first.Vault(keyring(t, testKey(1))).MarkLogout(ctx, "bob", "github"); err != nil {
		t.Fatal(err)
	}
	_ = first.Close()
	reopened := open(t, dir2).Vault(keyring(t, testKey(1)))
	if p, err := reopened.LogoutPending(ctx, "bob", "github"); err != nil || !p {
		t.Fatalf("marker lost on reopen: %v, %v", p, err)
	}
	if err := reopened.Put(ctx, "bob", vault.Credential{Provider: logoutMarkerPrefix + "github", AccessToken: "x"}); err == nil {
		t.Error("a credential was written under the reserved marker name")
	}
}
