# AGENTS.md — gofer project context

Canonical, tool-neutral context for humans and AI agents working in this repo.
Keep it current.

## What this is

gofer is a lean, secure Go backend for **GitHub worklist management**. It
retrieves a user's GitHub work (issues, PRs, comments, reviews), persists it, and
decorates each item with agent-derived metadata (relevance, impact, engagement,
urgency → rank) produced by ephemeral agent runtimes. A landing page renders the
user's work ordered by that metadata.

gofer is **functionally equivalent to zumble-zay** — same domain, same data
model, same user-facing behavior — with one deliberate architectural difference:
**agent dispatch is the Agent Execution Interface (AEI), not a homegrown
orchestrator.** AEI was itself extracted from zumble-zay, so the seams line up
almost 1:1.

Status: **functional**. gofer is an AEI app: it dispatches runs through the AEI
app SDK to a pre-installed control plane. The web tier (OAuth, sessions, API, UI),
the agent runtime (GitHub fetch, enrich, LLM rank, converse, research), and the
split identity (the controller mints the run credential with gofer's Ed25519 key,
the web tier verifies with only the public key) are all in place and tested. What
remains is operational hardening and a live end-to-end run on an AEI-installed
cluster.

## Tech & layout

- Go 1.26.0 (module directive). gofer imports only stdlib-only AEI packages and no
  client-go: dispatch is an HTTP call to the pre-installed control plane.
- Module: `github.com/jackfrancis/gofer`.
- AEI dependency (local sibling checkout via `replace`):
  - `.../aei` — run-spec types (Contract 1)
  - `.../aeiruntime` — runtime SDK (Contract 2): learn the run, redeem, vend,
    complete (used by `cmd/runtime`)
  - `.../sdks/go/aeiapp` — app SDK: dispatch runs to the control plane's data plane
    and read lifecycle back (HTTP/JSON, no client-go)

```
cmd/server/          web tier entrypoint
cmd/runtime/         agent runtime entrypoint (aeiruntime.Main)
internal/config/     env configuration
internal/principal/  Principal{Kind, Subject, ActingUserID, Scopes}
internal/identity/   run-credential authority (Ed25519); web verifies, controller mints — ADR 0002
internal/vault/      delegated provider tokens; vended to runtimes via internal/api
internal/dispatch/   AEI app dispatch: aeiapp.Client wrapper (Submit / Status)
internal/ingest/     Ingestor seam → dispatched AEI runs
internal/worklist/   WorkItem + Metadata, Store + Ingestor seams, sort
internal/auth/       OAuth provider wiring + login/callback        (ported incrementally)
internal/authn/      RequireAuth / RequireScope (cookie OR bearer) (ported incrementally)
internal/session/    HMAC-signed cookie sessions                   (ported incrementally)
internal/api/        JSON handlers: worklist, agent sink, credential broker
internal/server/     web-tier router + middleware                  (ported incrementally)
internal/webui/      landing page                                  (ported incrementally)
internal/github/     GitHub client (runtime-only)                  (ported incrementally)
internal/llm/        LLM ranking (runtime-only)                    (ported incrementally)
internal/markdown/   render + sanitize assistant Markdown          (ported incrementally)
internal/metrics/    Prometheus metrics                            (ported incrementally)
Dockerfile           web-tier image (packages the prebuilt host binary)
Dockerfile.runtime   agent-runtime image (cmd/runtime)
deploy/k8s/base/     app manifests: namespace, SA, config, deployment, service
deploy/k8s/overlays/dev/  base + AgentApp (gofer's AEI dispatch registration)
```

## Cluster dev loop (kind)

gofer ships **only its own application components** and assumes AEI is already
installed on the cluster — the `aei-controller` (namespace `aei-system`,
configured with gofer's Ed25519 signing key so its mints verify in gofer), the
`agents.x-k8s.io` CRDs, and an `AgentProviderClass` named `gofer` bound to the
`k8s-job` provider with `parameters.image` = the gofer runtime image. Installing
AEI is the AEI project's job, never gofer's.

- `make dev-up` cross-compiles the web + runtime binaries **on the host** (the AEI
  `replace` directives point at a sibling checkout outside any Docker build
  context, so a host build resolves them), packages each into a distroless image,
  `kind load`s both (creating the `gofer` kind cluster if absent), generates the
  run-credential keypair (`make genkey` → `.gofer-agent-key.env`, emitting the
  controller's private half), ensures the namespace + `gofer-secrets` (with the
  public half), and applies the web app + `AgentApp`. It **fails fast if AEI's CRDs
  are absent** — gofer is an AEI app and no longer runs standalone. `make
  dev-forward` port-forwards the web tier; `dev-down` / `dev-logs` / `cluster-up` /
  `cluster-down` round it out.
- The web tier dispatches every run to the pre-installed controller through the
  `aeiapp` SDK and holds **no minting key** — only the verify-only public key. The
  `AgentApp` CR is gofer's app-side AEI contract (which SA may dispatch, the scope
  ceiling, and the credential audience).

## The AEI seam (what replaces zumble-zay's dispatch tier)

| zumble-zay | gofer via AEI |
| --- | --- |
| `internal/orchestrator` + `internal/launcher` | `internal/dispatch` over the `aeiapp` SDK → pre-installed controller |
| `internal/mint` (Ed25519) | the aei-controller, configured with gofer's `identity` Ed25519 authority |
| `internal/controlplane` (redeem, token exchange, caller auth) | the aei-controller data plane (`/dispatch`, runtime ABI) |
| `internal/vault` (`Vend`) | `internal/vault` served by gofer's own `GET /agent/credential` |
| `internal/runtimespec` + runtime bootstrap | `aeiruntime` SDK |
| `internal/{k8slauncher,agentsandbox,substrate,...}` | AEI providers, running in the controller (not gofer) |

## Core design decisions (see docs/adr)

- **AEI owns dispatch; gofer owns the app** (ADR 0001). gofer is an AEI *app*: it
  imports the app SDK (`aeiapp`) and the runtime SDK, and dispatches runs to the
  pre-installed aei-controller over HTTP. It embeds no control plane, selects no
  substrate, and imports no provider or client-go. The provider that runs gofer's
  runtimes is fixed by the `gofer` `AgentProviderClass` on the controller.
- **Identity is a pluggable authority seam, Ed25519 by default** (ADR 0002). The
  run credential is a per-run, capability-scoped, short-TTL, **audience-bound** job
  token minted with an **asymmetric** issuer. The seam (`control.TokenAuthority`)
  is contributed upstream to AEI; the aei-controller is configured with gofer's
  Ed25519 authority (`AEI_ED25519_PRIVATE_KEY`) and is the **sole minter**, while
  gofer's web tier holds only the public key (`MINT_PUBLIC_KEY`) and can never mint.
  The same seam accepts an orka **kontxt TxToken** or a **SPIFFE/SPIRE** authority
  without a gofer change, keeping AEI lean and identity-agnostic.

## Security invariants (do not regress)

- Run-credential minting is **asymmetric**: the web tier holds no private key and
  can never gain minting ability (the aei-controller is the sole minter). A web-tier
  compromise cannot forge a job token.
- Job tokens are **audience-bound**; a runtime credential must never authenticate
  on the interactive user API, and a browser session must never authenticate on
  the agent sink. The two planes are disjoint.
- Downstream provider credentials are **vended on demand** (gofer's
  `GET /agent/credential`, authenticated by the run credential) and held in memory
  for the run only; never injected as standing secrets, never logged, never
  persisted. gofer core packages must not import a provider client — only the agent
  runtime does.
- The **chat-model token** (`AI_TOKEN`) is the same class of credential. It is
  **never** a web-pod secret: the web tier reads only the non-secret coordinates
  (`AI_ENDPOINT`, `AI_MODEL`) to decide whether to offer the Discuss UI. The runtime
  runs the model and receives `AI_TOKEN` through its AEI provisioning (the
  `AgentProviderClass` / a runtime secret), never the web tier.
- Least privilege: source scopes are READ; only gofer metadata is WRITE.

## Conventions & gotchas

- Each `.go` file has exactly one `package` clause.
- gofer imports **no client-go and no AEI provider**: dispatch is an HTTP call to
  the pre-installed aei-controller through the `aeiapp` SDK (stdlib-only), so there
  is no `providers` build tag and no `LAUNCHER` knob. The provider that runs gofer's
  runtimes is fixed by the `gofer` `AgentProviderClass` on the controller.
- Interfaces are the extension seams: `identity.Authority` (mint/verify; gofer wires
  only the verifier, the controller mints), `worklist.Store` / `worklist.Ingestor`,
  `ingest.Dispatcher` (the `aeiapp` dispatch surface), and the runtime's
  `agent.Vendor` / `agent.Sink`.
