# AGENTS.md — gofer project context

Canonical, tool-neutral context for humans and AI agents working in this repo.
Keep it current.

## What this is

gofer is a lean, secure Go backend for **GitHub worklist management**. It retrieves
a user's GitHub work (issues, PRs, comments, reviews), persists it, and decorates
each item with agent-derived metadata (relevance, impact, engagement, urgency →
rank). A landing page renders the user's work ordered by that metadata.

**A starting point for an agent-runtime backend (branch `gofer-clean`).** gofer is an
opinionated application stack for its mission with the agent-runtime layer factored out
behind a single seam. The web tier, worklist model, scoring, UI, OAuth, and all the
interface seams are fully implemented; the default backend is a no-op, so the worklist
stays empty until a backend that executes runs is selected. Adding one is net-new code
— a package under `internal/runtime/<name>` plus one wiring line in `cmd/server`, with
no change to gofer's interfaces (see `internal/runtime` for the contract).

A parallel branch `gofer-aei` keeps the previous, functional implementation whose
dispatch layer was the Agent Execution Interface (AEI), as a reference.

**This branch (`aei-agentsessions`) selects the agentsessions backend.**
`internal/runtime/agentsessions` runs each gofer run as a durable agentsessions session
(journaled run intent, an `api.Harness` executing `agent.Run`, `Status` read from the
journal), wired by one line in `cmd/server`. It runs the workload IN-PROCESS against
the vault and store, so it does not exercise the `/agent/*` plane or run-credential
minting; and gofer's workload makes its own model calls rather than host-mediated ones
(the converse tool loop needs a live ToolBox, and rank/enrich fan out concurrently
while an agentsessions journal is single-writer), so replay/fork determinism is not
claimed — the harness declares `ForkSafe: false`.

## Tech & layout

- Go 1.26.0. gofer's own code depends only on its packages plus a few libraries
  (goldmark, bluemonday, prometheus, oauth2); the SELECTED BACKEND brings its own
  dependency — on this branch that is agentsessions (a local sibling checkout via a
  `replace`), reached only from `internal/runtime/agentsessions`.
- Module: `github.com/jackfrancis/gofer`.

```
cmd/server/          web tier entrypoint (HTTP, UI)
internal/config/     env configuration
internal/principal/  Principal{Kind, Subject, ActingUserID, Scopes}
internal/identity/   run-credential authority (Ed25519); web verifies, a backend mints
internal/vault/      delegated provider tokens; vended via internal/api credential broker
internal/ingest/     Dispatcher seam + NoopDispatcher (THE runtime socket)
internal/runtime/    the backend contract (doc.go) + agentsessions/ (this branch's backend)
internal/agent/      agent-runtime WORKLOAD (a backend runs it per run) + Vendor/Sink adapters
internal/worklist/   WorkItem + Metadata, Store + Ingestor seams, sort, scoring
internal/auth/       OAuth provider wiring + login/callback
internal/authn/      RequireAuth / RequireScope (cookie OR bearer)
internal/session/    HMAC-signed cookie sessions
internal/api/        JSON handlers: worklist, agent sink, credential broker
internal/server/     web-tier router + middleware
internal/webui/      landing page
internal/github/     GitHub client (workload-only)
internal/llm/        LLM ranking / converse (workload-only)
internal/markdown/   render + sanitize assistant Markdown
internal/metrics/    Prometheus metrics
Dockerfile           web-tier image
deploy/k8s/base/     app manifests: namespace, SA, config, deployment, service
deploy/k8s/overlays/dev/  base (a backend adds its dispatch registration)
```

## The runtime socket (what a real agent runtime plugs into)

gofer builds the full *intent* of every agent action and hands it to a seam. The
default is a no-op backend:

| Seam | Default (no-op) | What a backend provides |
| --- | --- | --- |
| `ingest.Dispatcher` (`Submit`/`Status`) | `ingest.NoopDispatcher` (accept + drop) | dispatch each `RunSpec` to an execution substrate |
| `agent.Vendor` / `agent.Sink` | HTTP clients to gofer's `/agent/*` plane | the workload's view of gofer (vend a credential, read/write the worklist) |
| `agent.Run` (`internal/agent`) | present, uncalled | the workload a runtime executes per run |
| `identity.Authority` | verify-only (`internal/identity`) | the minting half (a control plane) |

`ingest.RunSpec` is the provider-neutral run intent (task ref, parameters, scoped
identity, deadline). gofer constructs one for `github-ingest` (backfill/refresh) and
`github-converse` (Discuss / review) and submits it to the `Dispatcher`. Implement a
real `Dispatcher`, run `agent.Run` behind it, and have it write back through the
`/agent/*` plane to bring gofer to life. Start from the contract in `internal/runtime`
and the selection point in `cmd/server`.

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
- gofer imports no client-go and no agent-runtime SDK. The runtime socket is the
  `ingest.Dispatcher` interface; the only implementation is `NoopDispatcher`.
- Interfaces are the extension seams: `identity.Authority`, `worklist.Store` /
  `worklist.Ingestor`, `ingest.Dispatcher`, and the workload's `agent.Vendor` /
  `agent.Sink`.
- The web tier boots and serves the UI standalone; the worklist stays "Discovering…"
  because the default no-op dispatcher fetches nothing — expected until a backend that
  executes runs is selected.
- `go test` binds httptest sockets; run it outside a restrictive sandbox.

## Build / test / run

```
make build   # go build ./...
make test    # go test ./...
make run     # run the web tier (local UI/OAuth)
make genkey  # run-credential keypair (verify key + unused private half)
```
