# AGENTS.md — gofer project context

Canonical, tool-neutral context for humans and AI agents working in this repo.
Keep it current.

## What this is

gofer is a lean, secure Go backend for **GitHub worklist management**. It retrieves
a user's GitHub work (issues, PRs, comments, reviews), persists it, and decorates
each item with agent-derived metadata (relevance, impact, engagement, urgency →
rank). A landing page renders the user's work ordered by that metadata.

**Agent runs execute on AEI.** gofer is an opinionated application stack for its
mission with the agent-runtime layer factored out behind a single seam
(`ingest.Dispatcher`). The selected backend is the Agent Execution Interface (AEI,
`github.com/jackfrancis/agent-execution-interface`), in `internal/runtime/aei`: the web
tier dispatches each run to a pre-installed AEI control plane, which launches gofer's
runtime image (`cmd/runtime`) per run on the substrate the app's provider class names.
gofer needs AEI on the cluster and does not run standalone. A backend is still purely
additive — a package under `internal/runtime/<name>` plus one wiring line in
`cmd/server`, with no change to gofer's interfaces (see `internal/runtime` for the
contract).

The `gofer-clean` branch keeps the backend-less prototype (no-op dispatcher wired), as
a reference for the seam with nothing plugged in.

## Tech & layout

- Go 1.26.0. gofer imports its own packages, a few libraries (goldmark, bluemonday,
  prometheus, oauth2, modernc.org/sqlite — pure Go, so `CGO_ENABLED=0` holds), and AEI's two stdlib-only SDK modules (the app SDK
  `sdks/go/aeiapp` to dispatch, the runtime SDK `aeiruntime` for the agent binary).
  Those are consumed through `replace` directives in `go.mod` pointing at a SIBLING
  checkout (`../agent-execution-interface`), so the build needs that checkout present
  until AEI is published. gofer imports no provider and no client-go.
- Module: `github.com/jackfrancis/gofer`.

```
cmd/server/          web tier entrypoint (HTTP, UI); selects the backend
cmd/runtime/         agent runtime entrypoint (one-line shim over internal/runtime/aei)
internal/runtime/    the backend contract (doc.go)
internal/runtime/aei/  the AEI backend: dispatcher (aei.go) + workload entrypoint (workload.go)
internal/config/     env configuration
internal/principal/  Principal{Kind, Subject, ActingUserID, Scopes}
internal/identity/   run-credential authority (Ed25519); web verifies, AEI mints
internal/vault/      delegated provider tokens; vended via internal/api credential broker
internal/ingest/     Dispatcher seam (THE runtime socket) + the reference NoopDispatcher
internal/agent/      agent-runtime WORKLOAD (the runtime runs it per run)
internal/worklist/   WorkItem + Metadata, Store + Ingestor seams, sort, scoring
internal/auth/       OAuth provider wiring + login/callback
internal/authn/      RequireAuth / RequireScope (cookie OR bearer)
internal/session/    HMAC-signed cookie sessions; Store seam for authenticated sessions
internal/storage/    SQLite persistence: worklist, encrypted vault, sessions (web tier only)
internal/api/        JSON handlers: worklist, agent sink, credential broker
internal/server/     web-tier router + middleware
internal/webui/      landing page
internal/github/     GitHub client (workload-only)
internal/llm/        LLM ranking / converse (workload-only)
internal/markdown/   render + sanitize assistant Markdown
internal/metrics/    Prometheus metrics
Dockerfile           web-tier image
Dockerfile.runtime   agent-runtime image (AEI launches it per run)
deploy/k8s/base/     app manifests: namespace, SA, config, statefulset (+ data PVC), service
deploy/k8s/overlays/dev/  base + agentapp.yaml (gofer's AEI dispatch registration)
```

## The runtime socket (what the agent runtime plugs into)

gofer builds the full *intent* of every agent action and hands it to a seam. The AEI
backend fills each one:

| Seam | gofer's side | The AEI backend |
| --- | --- | --- |
| `ingest.Dispatcher` (`Submit`/`Status`) | constructs each `RunSpec` | `aei.Dispatcher` POSTs it to the aei-controller (`AEI_DISPATCH_ENDPOINT`) as the `AgentApp` named by `AEI_APP`, authenticating with the web pod's projected ServiceAccount token |
| `agent.Vendor` / `agent.Sink` | `agent.NewPlaneClient`: one HTTP client to the `/agent/*` plane | the runtime uses it with its run credential (vend a credential, read/write the worklist) |
| `agent.Run` (`internal/agent`) | the workload | `aei.RunWorkload` (`cmd/runtime`) executes it per run, out-of-process |
| `identity.Authority` | verify-only (`internal/identity`, `MINT_PUBLIC_KEY`) | AEI's control plane owns the keypair and is the sole minter |

`ingest.RunSpec` is the provider-neutral run intent (task ref, parameters, scoped
identity, deadline). gofer constructs one for `github-ingest` (backfill/refresh) and
`github-converse` (Discuss / review) and submits it to the `Dispatcher`. The backend
contract is in `internal/runtime`; the selection point is in `cmd/server`.

AEI itself (the aei-controller in `aei-system`, the `agents.x-k8s.io` CRDs, the signing
authority, and an `AgentProviderClass`) is installed from the AEI repository, not by
gofer. gofer's `AgentApp` (`deploy/k8s/overlays/dev/agentapp.yaml`) names a provider
class by capability; the default is `isolated` (agent-sandbox pods under gVisor, run in
the `gofer-runtime` namespace), and `k8s-job` is the lighter alternative.

## The domain (100% gofer's own, unaffected by the runtime)

`WorkItem{GitHubRef + Metadata}`. Axes relevance/impact/engagement/urgency → rank →
priority band. Server-side sort. Scoring is a pure function of an item's Signals, so
the worklist is re-scored at read time. Job types the workload implements:
github-ingest, github-enrich, llm-rank, github-converse, github-research.

## Security invariants (the contract a runtime must honor)

Wired on the web-tier side; they define what a runtime must respect:

- **Two disjoint auth planes.** A browser session authenticates `/api/*`; a run
  credential authenticates `/agent/*`. They never cross (audience-bound tokens).
- **Verify-only web tier.** It holds only the run-credential public key and can
  never mint. A runtime's control plane would be the sole minter — so a web-tier
  compromise cannot forge a job token.
- **Credentials vended on demand.** Downstream provider tokens come from
  `GET /agent/credential` (behind RequireScope) and are held by the workload for the
  run only — never a standing secret, never logged, never persisted. gofer core
  packages import no provider client; only the workload (`internal/agent`) does.
- **The chat-model token is the same class.** The web tier holds only the non-secret
  coordinates (`AI_CONNECTIONS`) and brokers the token; the workload runs the model.
- **Least privilege.** Source scopes are READ; only gofer metadata is WRITE.

## Conventions & gotchas

- Each `.go` file has exactly one `package` clause.
- gofer imports no client-go and no provider. The runtime socket is the
  `ingest.Dispatcher` interface; `cmd/server` wires `aei.Dispatcher`, and
  `NoopDispatcher` remains as the reference no-op (used by tests).
- Interfaces are the extension seams: `identity.Authority`, `worklist.Store` /
  `worklist.Ingestor`, `vault.Vault`, `session.Store`, `ingest.Dispatcher`, and the workload's `agent.Vendor` /
  `agent.Sink`.
- The web tier fails fast at startup without `AEI_DISPATCH_ENDPOINT` and `AEI_APP`.
  With them set but no reachable aei-controller (e.g. `make run`), sign-in works but
  dispatch fails, so the worklist stays empty.
- Web-tier state (sessions, delegated tokens in the vault, the worklist and its
  threads) is durable when `DATA_DIR` is set: one SQLite database
  (`internal/storage`, pure-Go `modernc.org/sqlite`) on the StatefulSet's
  PersistentVolume, with tokens sealed by `VAULT_ENCRYPTION_KEY` (required with
  `DATA_DIR`; startup fails without it). With `DATA_DIR` unset (`make run`, tests) the
  in-memory stores are used and a restart loses the state. The web tier is a single
  replica by construction. Pre-auth OAuth flow state and the ingest backfill tracker
  are always in memory. See `docs/adr/0004-sqlite-persistent-storage.md` and
  `docs/backup-restore.md`.
- Schema changes are a new numbered file in `internal/storage/migrations/`; never
  edit an applied one.
- To ship a workload-only change (`internal/agent`, `internal/llm`,
  `internal/github`) to the kind cluster without restarting the web pod, use
  `make kind-load-runtime`.
- The conversation's tool loop is capped at `maxToolIterations` rounds
  (`internal/llm/converse.go`); the model's GitHub tools are in `internal/agent/tools.go`.
- `go test` binds httptest sockets; run it outside a restrictive sandbox.

## Build / test / run

```
make build   # go build ./...
make test    # go test ./...
make run     # run the web tier alone (local UI/OAuth; dispatch needs an aei-controller)
make genkey  # an Ed25519 keypair for local runs (in-cluster, AEI's public key is used)
make dev-up  # deploy web + runtime images and the AgentApp to kind (AEI must be installed first)
make kind-load-runtime  # rebuild + load only the runtime image
make deploy KUBECONFIG=<file> IMAGE=... RUNTIME_IMAGE=...  # apply to any cluster (no kind, no secret creation); dev-up shares its apply step
```

See README.md for the kind walkthrough, including the AEI-side install.
