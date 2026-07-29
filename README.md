# gofer

A lean, secure GitHub **worklist management** backend in Go: it authenticates a
user via a trusted OAuth2 identity provider, retrieves and persists their GitHub
work (issues, PRs, reviews), decorates each item with agent-derived metadata
(relevance, impact, engagement, urgency → rank), and renders the user's work
ordered by that metadata.

> **A starting point for an agent-runtime backend.** gofer is a complete, opinionated
> application stack for its mission (manage GitHub work) with the agent-runtime layer
> factored out behind a single seam (`ingest.Dispatcher`). The default backend is a
> no-op, so the web tier (OAuth, sessions, worklist model, scoring, UI, API) runs fully
> but the worklist stays empty until a backend that executes runs is selected. Adding
> one — AEI, agent-sandbox, a local launcher — is net-new code: a package under
> `internal/runtime/<name>` plus one wiring line in `cmd/server`, with no change to
> gofer's interfaces. The contract is documented in `internal/runtime`.

> **Project context & design:** [AGENTS.md](AGENTS.md).

## The socket: where an agent runtime slots in

gofer builds the full *intent* of every agent action (a `RunSpec`) and hands it to a
`Dispatcher`. The default implementation is a no-op; a backend replaces it:

| Concern | Default (no-op backend) | With a backend |
| --- | --- | --- |
| Dispatch a run (`ingest.Dispatcher`) | `ingest.NoopDispatcher` — accepts every run, does nothing | a real dispatcher to an agent-execution substrate |
| The agent WORKLOAD (`internal/agent`) | present; the no-op backend never calls it | executed per run (fetch GitHub, enrich, rank, converse) |
| Credential vend + write-back (`/agent/*`) | served; the no-op backend never calls them | a backend authenticates and drives them |
| Run-credential mint/verify (`internal/identity`) | verify-only wired; nothing mints | a runtime's control plane mints |
| Domain (worklist model, scoring, sort, GitHub client, LLM, UI, auth, sessions) | **fully implemented** | unchanged |

To bring gofer to life, implement `ingest.Dispatcher` (see the contract in
`internal/runtime`) so it executes the `RunSpec`s gofer already constructs, running
the workload in `internal/agent` and writing results back through the `/agent/*` plane.

## Project layout

```
cmd/server/          web tier entrypoint (HTTP, UI)
internal/config/     env configuration
internal/principal/  Principal{Kind, Subject, ActingUserID, Scopes}
internal/identity/   run-credential authority (Ed25519); web verifies, a backend mints
internal/vault/      delegated provider tokens; vended via internal/api credential broker
internal/ingest/     Dispatcher seam + NoopDispatcher: turns actions into run requests
internal/agent/      agent-runtime WORKLOAD (a backend runs it per run)
internal/worklist/   WorkItem + Metadata, Store + Ingestor seams, sort, scoring
internal/{auth,authn,session,api,server,webui,github,llm,markdown,metrics}/
```

## Build & run

```sh
make build     # go build ./...
make test      # go test ./...
make genkey    # generate the run-credential keypair (verify key + a runtime minter's private half)
make run       # run the web tier for local UI/OAuth work
```

The web tier runs standalone: sign in with GitHub and the UI renders, but the
worklist stays on "Discovering your work…" because the default no-op dispatcher never
fetches anything. Select a backend that executes runs to populate it.
