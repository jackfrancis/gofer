# ADR 0001 — AEI is gofer's agent-dispatch layer

Status: accepted
Date: 2026-07-15

## Context

gofer is functionally equivalent to zumble-zay, which built its own agent-dispatch
tier: an orchestrator, a launcher registry with per-substrate providers
(`k8s-job`, `k8s-pod`, `agent-sandbox`, `opensandbox`, `agent-substrate`,
`kagent`), an Ed25519 token mint, a control plane (token exchange, redemption,
caller auth), and a credential vault. The Agent Execution Interface (AEI) is that
dispatch tier extracted and generalized into a vendor-neutral standard — "the
CRI/CSI for agent dispatch." AEI's own reference app is zumble-zay, so the seams
line up almost 1:1.

## Decision

gofer consumes AEI instead of a homegrown orchestrator:

- **Dispatch** is `aei.Launcher` + the provider registry (`aei.Register` /
  `aei.Build`). gofer selects a substrate by name (`LAUNCHER`) and never imports a
  substrate SDK.
- **The control plane** is AEI's reference `control.Plane`: it dispatches through
  any launcher, races the runtime's `/complete` callback against the provider's
  `Await` and the deadline, and serves the runtime ABI (`/complete`, `/redeem`,
  `/vend`).
- **The runtime** is built on the `aeiruntime` SDK: it learns its run (env or A2A
  metadata) through one decoder, redeems a pull-ticket if present, vends
  downstream credentials on demand, and reports completion idempotently.

gofer owns everything AEI explicitly leaves out of scope: the worklist domain
model and scoring, the GitHub client, LLM ranking, Markdown rendering, the web UI,
OAuth, sessions, and the principal/authorization model.

## Readiness assessment (why this is viable today)

Ready in AEI, consumed as-is:

- `aei` core (RunSpec, Launcher/AsyncLauncher/PullCredentialLauncher, registry,
  injection), tested.
- `aeiruntime` SDK (Load, Vend, Complete, redeem, Main), tested.
- `control` plane (mint, ticket redeem, `/complete` `/redeem` `/vend`,
  Run/Submit/Status, `CredentialSource` seam), functional but **reference-grade**.
- Providers `k8s-job`, `agent-sandbox`, `agent-substrate`.
- App SDKs (Go `aeiapp`, Python) and the data-plane dispatch API + CRDs.

Gaps gofer accounts for:

1. **Provider parity.** AEI ships 3 providers; zumble-zay demonstrated ~6–8
   (adds bare-pod, `opensandbox`, `kagent`). gofer v1 targets the 3 shipped AEI
   providers. Others are authored against `aei.Launcher` later if needed.
2. **Toolchain floor — resolved.** AEI's `providers` module is go 1.26.0, so
   gofer's go directive is now 1.26.0 (auto-fetched). Because the providers pull in
   client-go, they are compiled only under the `providers` build tag: the default
   build stays lean (the in-process `dispatch.LocalLauncher`, no client-go), and a
   `-tags providers` build links the three substrates. `internal/launcher.Build`
   selects `local` → in-process or any other name → the AEI provider registry
   (`aei.Build`); `LAUNCHER` picks one at runtime.
3. **Control-plane security posture.** AEI's reference control plane mints an
   **HMAC** token (symmetric). zumble-zay's posture is **Ed25519** (asymmetric),
   audience-bound, with disjoint auth planes. gofer restores that posture via a
   pluggable identity authority seam — see ADR 0002.

## Consequences

- gofer's dependency graph contains no substrate client library unless a provider
  is blank-imported at the top level.
- Swapping substrates is a config change (`LAUNCHER`), not a code change.
- gofer tracks AEI's evolution; contributions that harden AEI (notably the
  identity seam in ADR 0002) flow upstream rather than forking.

## Update (2026-07-16): dispatch is the aeiapp data-plane client, not an embedded control plane

gofer no longer embeds `control.Plane` or selects a launcher. It is an AEI *app*
that dispatches through the **app SDK** (`sdks/go/aeiapp`) to the pre-installed
aei-controller's data plane (`POST /aei/v1alpha1/dispatch`, see AEI's
docs/dispatch-plane.md), authenticating with its projected ServiceAccount token.
Consequences that supersede the Decision above:

- gofer imports **no provider and no client-go**, and there is no `providers` build
  tag: the default (and only) build is stdlib-only. `internal/launcher` and
  `dispatch.LocalLauncher` are removed; `internal/dispatch.Engine` is a thin
  `aeiapp.Client` wrapper (`Submit` → `/dispatch`, `Status` → `/runs/{id}`). There is
  no `LAUNCHER` knob — the provider is fixed by gofer's `AgentProviderClass` on the
  controller.
- The control plane (mint, `/complete`, `/redeem`, `/vend`) runs in the controller,
  not gofer. gofer no longer serves the runtime ABI.
- Because the controller's `/vend` cannot reach gofer's vault, gofer vends the
  user's provider token from its **own** domain plane instead
  (`GET /agent/credential`); the runtime calls that with its run credential.
- gofer now depends on a real AEI install (controller + CRDs + a `gofer`
  AgentProviderClass). The previous in-process "works without AEI" path is gone by
  design — this ADR's premise (embed the reference control plane, select a
  substrate) is retained only as history.
