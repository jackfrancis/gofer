# ADR 0002 — Identity is a pluggable authority seam (Ed25519 by default)

Status: accepted
Date: 2026-07-15

## Context

A run credential (the "job token") is the per-run, capability-scoped, short-TTL,
audience-bound token an agent runtime carries. zumble-zay mints it with an
**asymmetric** issuer (Ed25519): the orchestrator holds the private key and is the
sole minter; the web tier holds only the public key and can only verify. A
web-tier compromise therefore cannot forge a job token. Tokens are also
**audience-bound**, so a runtime credential can never be replayed on the
interactive user API, and the two authentication planes are disjoint.

AEI's reference `control.Plane` instead mints an **HMAC** token (symmetric,
single shared secret) and its roadmap defers a hardened, attestation-backed
profile (SPIFFE/SPIRE) to a later phase. AEI's design explicitly treats identity
as a *seam* (design tenet #4) and already extracted `CredentialSource` as a
pluggable interface for `/vend` — but it has **not** extracted the mint/verify
step, which is hardcoded to HMAC.

Separately, Kubernetes-native orchestrators like **orka** already own a richer
identity/governance plane than an Ed25519 mint: **kontxt TxTokens** (audience-bound
transaction tokens with signed scope constraints, immutable transaction metadata,
and a token-transformation service that narrows child/outbound tokens for
delegated agents and downstream tools), plus ServiceAccount TokenReview and OIDC.
If AEI is ever adopted as orka's launcher abstraction, orka — not AEI — should
supply the identity authority.

## Decision

Model run-credential identity in gofer as a **pluggable authority seam**, not a
hardcoded scheme:

```go
type Claims struct { RunID, Subject string; Scopes []string; Audience string; Exp int64 }
type Minter    interface { Mint(Claims) (string, error) }
type Verifier  interface { Verify(token string) (Claims, bool) }
type Authority interface { Minter; Verifier }
```

- gofer's **default authority is Ed25519**, split into a signer (private key; the
  dispatch/control tier) and a verifier (public key; the web tier) so the
  asymmetric, sole-minter property is enforced by construction. Verification
  enforces expiry **and audience**.
- The seam accepts any authority: an orka **kontxt TxToken** authority or a
  **SPIFFE/SPIRE** authority plugs in behind the same interface with no gofer
  change.

## Where this belongs long-term: AEI stays lean

The correct home for asymmetric crypto + audience enforcement is **the platform,
not AEI core** — the CRI/CSI precedent AEI models itself on (CRI does not sign
images; CSI does not manage KMS). Baking Ed25519 + audience into AEI would
duplicate or conflict with orka's kontxt the day orka adopts AEI.

The lean upstream change is therefore **not** "add asymmetric mint to AEI." It is
**"add a `control.TokenAuthority` (Minter/Verifier) seam to AEI's control plane,"**
mirroring the `CredentialSource` seam it already exposes for `/vend`, defaulting to
the existing HMAC implementation. Behind that seam:

- gofer plugs in this Ed25519 authority → 100% zumble-zay posture parity today;
- orka plugs in kontxt TxToken TTS → AEI slots under orka with zero identity
  opinionation;
- a production profile plugs in SPIFFE/SPIRE → AEI's own roadmap Phase 6.

gofer will offer that seam as an upstream AEI contribution.

**Status: implemented.** The seam now exists in AEI's control plane as
`control.TokenAuthority` (a `Mint`/`Verify` interface set via `Config.Authority`,
defaulting to the reference HMAC). gofer adapts its `identity.Authority` to it
(`internal/dispatch`.`controlAuthority`), so the control plane mints and verifies
gofer's Ed25519 run credential directly. The AEI runtime SDK also gained
`Runtime.Credential()`, so a runtime authenticates its own backend calls with the
same audience-bound credential it uses for the ABI.

## Consequences

- **One credential, both planes.** The run credential is a single gofer
  audience-bound Ed25519 token: the AEI ABI (`/complete`, `/vend`) verifies it via
  the control plane (through the adapter), and gofer's domain plane
  (`/agent/worklist`) verifies it via `authn` — with only the public key. Proven
  end-to-end by `internal/dispatch.TestOneCredentialBothPlanes`.
- The web tier links only a verifier; it cannot mint. Enforced by the type: the
  web build is given an Ed25519 public key, not a private key.
- Audience binding is enforced at verification, in gofer, not in AEI.
- Swapping the authority (Ed25519 → kontxt → SPIFFE) is a wiring change at one
  construction site, not a change to dispatch, the runtime, or the domain.

## Update (2026-07-16): the minter moves into the control plane; gofer holds only the public key

With dispatch now going through the pre-installed aei-controller (ADR 0001 update),
the control plane — not gofer's web tier — is the run-credential minter. To keep the
asymmetric posture, the **controller is configured with gofer's Ed25519 authority**:
AEI's `control` package gained `NewEd25519Authority(priv)` (the same
`base64url(json).base64url(sig)` wire format and `run/sub/scp/aud/exp` claims as
gofer's `identity`), and the controller builds it from `AEI_ED25519_PRIVATE_KEY`,
falling back to the reference HMAC when unset. Then:

- gofer's web tier holds **only** `MINT_PUBLIC_KEY` and verifies; it has no private
  key and cannot mint — the sole-minter property is now enforced across a process
  boundary, not just by type. `internal/dispatch.controlAuthority` is gone (gofer no
  longer adapts an authority into a control plane it embeds).
- The run credential is still one audience-bound Ed25519 token for both planes, but
  those planes are now gofer's own: `GET /agent/credential` (vend the user's
  provider token) and `/agent/worklist` (results), both verified by `authn` with the
  public key. Proven by `internal/dispatch.TestOneCredentialBothPlanes`; the
  controller's matching mint is proven by `control.TestEd25519AuthorityPublicVerify`.
- gofer's `identity` package keeps the full mint+verify authority for tests (it
  stands in for the controller); production gofer wires only the verifier.
- Provisioning: `make genkey` emits the pair — `MINT_PUBLIC_KEY` into gofer's
  Secret, `AEI_ED25519_PRIVATE_KEY` into the controller.
- The seam is unchanged and still the right long-term shape; this update only moves
  *where* gofer's authority is plugged in (the controller) and removes gofer's
  embedded control plane.

