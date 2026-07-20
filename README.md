# gofer

A lean, secure GitHub **worklist management** backend in Go: it authenticates a
user via a trusted OAuth2 identity provider, retrieves and persists their GitHub
work (issues, PRs, reviews), decorates each item with agent-derived metadata
(relevance, impact, engagement, urgency → rank), and renders the user's work
ordered by that metadata.

gofer is **functionally equivalent to [zumble-zay](https://github.com/jackfrancis/zumble-zay)**,
but its agent-dispatch layer is the **[Agent Execution Interface (AEI)](https://github.com/jackfrancis/agent-execution-interface)**
rather than a homegrown orchestrator. gofer is an AEI *app*: it dispatches runs
through AEI's app SDK to a pre-installed control plane and runs its agent on the
AEI runtime SDK; it imports no provider and no client-go.

> **Project context & design:** [AGENTS.md](AGENTS.md) and the decision records in
> [docs/adr/](docs/adr/).

## What AEI provides vs. what gofer owns

| Concern | Owner |
| --- | --- |
| Dispatch a run · observe completion · select isolation · push/pull credential | **AEI** (`aeiapp` app SDK → the pre-installed `aei-controller`) |
| Runtime ABI (learn the run, redeem, vend, complete) | **AEI** (`aeiruntime`) |
| Downstream credential vending | gofer serves its own `GET /agent/credential` from `internal/vault` (the controller can't reach gofer's vault) |
| Run-credential minting/verification (the "job token") | the `aei-controller` mints with gofer's Ed25519 authority; gofer's web tier verifies with only the public key — a pluggable **authority seam** (see [ADR 0002](docs/adr/0002-identity-authority-seam.md)) |
| Domain (worklist model, scoring, sort, GitHub client, LLM rank, UI, auth, sessions) | gofer — AEI does not touch it |

## Status

**Functional.** The web tier (OAuth, sessions, API, UI), the agent runtime (GitHub
fetch, enrich, LLM rank, converse, research), the aeiapp dispatch client, and the
split identity (the controller mints, the web tier verifies) are implemented and
tested. What remains is operational hardening and a live end-to-end run on an
AEI-installed cluster. See [AGENTS.md](AGENTS.md) for details.

## Project layout

```
cmd/server/          web tier entrypoint (HTTP, UI, aeiapp dispatch client)
cmd/runtime/         agent runtime entrypoint (built on aeiruntime)
internal/config/     env configuration
internal/principal/  Principal{Kind, Subject, ActingUserID, Scopes}
internal/identity/   run-credential authority (Ed25519); web verifies, controller mints — ADR 0002
internal/vault/      delegated provider tokens; vended via internal/api credential broker
internal/dispatch/   AEI app dispatch: aeiapp.Client wrapper (Submit / Status)
internal/ingest/     Ingestor: turns an empty worklist into dispatched runs
internal/worklist/   WorkItem + Metadata, Store + Ingestor seams, sort
internal/{auth,authn,session,api,server,webui,github,llm,markdown,metrics}/
```

## Build

```sh
make build     # go build ./...  (stdlib-only; no client-go)
make test      # go test ./...
make genkey    # generate the run-credential keypair (ADR 0002)
make run       # run the web tier for local UI/OAuth work
```

gofer imports no provider and no client-go: dispatch is an HTTP call to the
pre-installed `aei-controller` through the `aeiapp` SDK. Running against a cluster
needs AEI installed (controller + CRDs + a `gofer` AgentProviderClass) and the
controller configured with gofer's signing key; see [ADR 0001](docs/adr/0001-aei-for-dispatch.md)
and `make dev-up`.
