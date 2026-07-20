package identity

import (
	"crypto/ed25519"
	"testing"
	"time"
)

func TestEd25519RoundTrip(t *testing.T) {
	_, priv, err := GenerateEd25519()
	if err != nil {
		t.Fatal(err)
	}
	a := NewEd25519Authority(priv, "gofer-agent", time.Minute)
	tok, err := a.Mint(Claims{RunID: "r1", Subject: "u1", Scopes: []string{"github:read"}})
	if err != nil {
		t.Fatal(err)
	}
	c, ok := a.Verify(tok)
	if !ok {
		t.Fatal("verify failed")
	}
	if c.RunID != "r1" || c.Subject != "u1" || c.Audience != "gofer-agent" {
		t.Fatalf("bad claims: %+v", c)
	}
}

func TestAudienceEnforced(t *testing.T) {
	pub, priv, _ := GenerateEd25519()
	a := NewEd25519Authority(priv, "gofer-agent", time.Minute)
	tok, _ := a.Mint(Claims{RunID: "r1", Subject: "u1"})

	if _, ok := NewEd25519Verifier(pub, "other-audience").Verify(tok); ok {
		t.Fatal("expected audience mismatch to fail")
	}
	if _, ok := NewEd25519Verifier(pub, "gofer-agent").Verify(tok); !ok {
		t.Fatal("expected matching audience to pass")
	}
}

// A verify-only tier holding the wrong (or any) public key must never accept a
// token it did not sign — the web tier cannot forge a job token.
func TestVerifierCannotForge(t *testing.T) {
	_, priv1, _ := GenerateEd25519()
	pub2, _, _ := GenerateEd25519()
	a := NewEd25519Authority(priv1, "gofer-agent", time.Minute)
	tok, _ := a.Mint(Claims{RunID: "r1", Subject: "u1"})
	if _, ok := NewEd25519Verifier(pub2, "gofer-agent").Verify(tok); ok {
		t.Fatal("verifier with the wrong key must reject")
	}
}

func TestExpiry(t *testing.T) {
	_, priv, _ := GenerateEd25519()
	pub := priv.Public().(ed25519.PublicKey)
	// Mint two minutes in the past with a one-minute TTL, so the token is expired.
	s := &ed25519Signer{
		priv: priv,
		ttl:  time.Minute,
		now:  func() time.Time { return time.Now().Add(-2 * time.Minute) },
		ver:  &ed25519Verifier{pub: pub, now: time.Now},
	}
	tok, _ := s.Mint(Claims{RunID: "r1", Subject: "u1"})
	if _, ok := s.Verify(tok); ok {
		t.Fatal("expected expired token to fail")
	}
}
