// Package identity is gofer's pluggable run-credential authority: the seam that
// mints and verifies the per-run, capability-scoped, short-TTL, audience-bound
// token an agent runtime carries (the "job token").
//
// It is an ASYMMETRIC issuer by design: a runtime's control plane holds the private
// key and is the sole minter, while gofer's web tier holds only the public key and can
// only VERIFY — so a web-tier compromise can never forge a job token — plus AUDIENCE
// binding so a runtime credential can never be replayed on the interactive user API.
// gofer wires only the verifier; a backend supplies the minting half (this Ed25519
// authority, or another — a transaction-token service, SPIFFE/SPIRE), keeping crypto
// and policy in the platform behind this seam.
package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// DefaultTTL bounds a run credential's lifetime when a caller does not set one.
const DefaultTTL = 15 * time.Minute

// Claims is the run credential's payload: the run it is bound to, the acting
// principal, the scopes it may exercise, the audience it is bound to, and expiry.
type Claims struct {
	RunID    string   `json:"run"`
	Subject  string   `json:"sub"`
	Scopes   []string `json:"scp,omitempty"`
	Audience string   `json:"aud,omitempty"`
	Exp      int64    `json:"exp"` // unix seconds
}

// Minter mints a signed run credential from claims. It is the privileged half of
// the seam: only a tier holding a signing key can implement it.
type Minter interface {
	Mint(Claims) (string, error)
}

// Verifier verifies a run credential and returns its claims. The web tier links
// only this half, so it can authenticate a workload token without ever being able
// to mint one.
type Verifier interface {
	Verify(token string) (Claims, bool)
}

// Authority is a full mint+verify identity, held by the sole-minter tier.
type Authority interface {
	Minter
	Verifier
}

// GenerateEd25519 returns a fresh Ed25519 keypair. For development a single
// process holds both; in a real deployment the private key lives only in the
// dispatch/control tier and the web tier receives only the public key.
func GenerateEd25519() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	return ed25519.GenerateKey(rand.Reader)
}

// ed25519Signer mints and verifies with a private key — the sole-minter tier.
type ed25519Signer struct {
	priv ed25519.PrivateKey
	ver  *ed25519Verifier
	ttl  time.Duration
	now  func() time.Time
}

// NewEd25519Authority builds a full (mint+verify) authority from a private key.
// The audience is stamped onto minted tokens and enforced on verification; ttl
// bounds token lifetime (DefaultTTL when non-positive).
func NewEd25519Authority(priv ed25519.PrivateKey, audience string, ttl time.Duration) Authority {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	pub := priv.Public().(ed25519.PublicKey)
	return &ed25519Signer{
		priv: priv,
		ver:  &ed25519Verifier{pub: pub, audience: audience, now: time.Now},
		ttl:  ttl,
		now:  time.Now,
	}
}

// Mint stamps expiry (and the authority's audience, if the caller left it unset)
// and returns base64url(json(claims)).base64url(ed25519-signature).
func (s *ed25519Signer) Mint(c Claims) (string, error) {
	if c.RunID == "" {
		return "", errors.New("identity: Mint requires a RunID")
	}
	if c.Audience == "" {
		c.Audience = s.ver.audience
	}
	c.Exp = s.now().Add(s.ttl).Unix()
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	b := base64.RawURLEncoding.EncodeToString(payload)
	sig := ed25519.Sign(s.priv, []byte(b))
	return b + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// Verify delegates to the public-key verifier.
func (s *ed25519Signer) Verify(token string) (Claims, bool) { return s.ver.Verify(token) }

// ed25519Verifier verifies with a public key only. It cannot mint. This is the
// type the web tier links, enforcing the asymmetric, sole-minter invariant by
// construction.
type ed25519Verifier struct {
	pub      ed25519.PublicKey
	audience string
	now      func() time.Time
}

// NewEd25519Verifier builds a verify-only identity for the web tier. It holds no
// private key, so it can never mint a job token. When audience is non-empty, a
// token whose aud does not match is rejected — the token-level half of keeping the
// user API and the agent sink disjoint.
func NewEd25519Verifier(pub ed25519.PublicKey, audience string) Verifier {
	return &ed25519Verifier{pub: pub, audience: audience, now: time.Now}
}

// Verify checks the signature, expiry, and (if configured) audience.
func (v *ed25519Verifier) Verify(token string) (Claims, bool) {
	b, sigPart, ok := strings.Cut(token, ".")
	if !ok {
		return Claims{}, false
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigPart)
	if err != nil || !ed25519.Verify(v.pub, []byte(b), sig) {
		return Claims{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(b)
	if err != nil {
		return Claims{}, false
	}
	var c Claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return Claims{}, false
	}
	if v.now().Unix() > c.Exp {
		return Claims{}, false
	}
	if v.audience != "" && c.Audience != v.audience {
		return Claims{}, false
	}
	return c, true
}
