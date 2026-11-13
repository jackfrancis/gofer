# gofer

A lean, secure GitHub **worklist management** backend in Go: it authenticates a
user via a trusted OAuth2 identity provider, retrieves and persists their GitHub
work (issues, PRs, reviews), decorates each item with agent-derived metadata
(relevance, impact, engagement, urgency → rank), and renders the user's work
ordered by that metadata.

> **Agent runs execute on AEI.** gofer is a complete, opinionated application stack
> for its mission (manage GitHub work) with the agent-runtime layer factored out behind
> a single seam (`ingest.Dispatcher`). The selected backend is the
> [Agent Execution Interface](https://github.com/jackfrancis/agent-execution-interface)
> (`internal/runtime/aei`): the web tier dispatches each run to a pre-installed AEI
> control plane, which launches gofer's runtime image on whatever substrate the app's
> provider class names. gofer therefore needs AEI on the cluster; it does not run
> standalone. The backend contract is documented in `internal/runtime`.

> **Project context & design:** [AGENTS.md](AGENTS.md).

## The socket: where the agent runtime slots in

gofer builds the full *intent* of every agent action (a `RunSpec`) and hands it to a
`Dispatcher`. Swapping backends changes one wiring line in `cmd/server` and nothing in
gofer's interfaces:

| Concern | gofer (unchanged by the backend) | AEI backend |
| --- | --- | --- |
| Dispatch a run (`ingest.Dispatcher`) | constructs the `RunSpec` | `internal/runtime/aei` POSTs it to the aei-controller, authenticating with the web pod's ServiceAccount token |
| The agent WORKLOAD (`internal/agent`) | fetch GitHub, enrich, rank, converse | `cmd/runtime` runs it per run, out-of-process, in the runtime image |
| Credential vend + write-back (`/agent/*`) | served by the web tier | the runtime calls them with its run credential |
| Run-credential mint/verify (`internal/identity`) | verify-only (public key) | AEI's control plane is the sole minter |
| Domain (worklist model, scoring, sort, GitHub client, LLM, UI, auth, sessions) | **fully implemented** | — |

`ingest.NoopDispatcher` (accept and drop every run) remains in `internal/ingest` as the
reference no-op implementation; the `gofer-clean` branch keeps the prototype in which
it was the wired default.

## Project layout

```
cmd/server/            web tier entrypoint (HTTP, UI); selects the backend
cmd/runtime/           agent runtime entrypoint (the image AEI launches per run)
internal/runtime/      the backend contract (doc.go)
internal/runtime/aei/  the AEI backend: dispatcher + workload entrypoint
internal/config/       env configuration
internal/principal/    Principal{Kind, Subject, ActingUserID, Scopes}
internal/identity/     run-credential authority (Ed25519); web verifies, AEI mints
internal/vault/        delegated provider tokens; vended via internal/api credential broker
internal/ingest/       Dispatcher seam: turns actions into run requests
internal/agent/        agent-runtime WORKLOAD (the runtime runs it per run)
internal/worklist/     WorkItem + Metadata, Store + Ingestor seams, sort, scoring
internal/storage/      SQLite persistence for the web tier: worklist, encrypted vault, sessions
internal/{auth,authn,session,api,server,webui,github,llm,markdown,metrics}/
```

## Build & test

```sh
make build     # go build ./...
make test      # go test ./...
```

gofer consumes AEI's two stdlib-only SDK modules through `replace` directives in
`go.mod`, so it builds only with the AEI repository checked out **next to** this one:

```
<parent>/gofer
<parent>/agent-execution-interface
```

## Run on kind

Prerequisites: `kind`, `kubectl`, `jq`, `openssl`, and podman or docker (podman is
preferred when both are installed); a GitHub OAuth app whose callback is
`http://localhost:8080/auth/github/callback`.

1. Install AEI and a provider class on a kind cluster named `gofer`, from the AEI
   checkout. For the default class (`isolated`: agent-sandbox pods under gVisor):

   ```sh
   make dev-up KIND_CLUSTER=gofer PROVIDERS=agent-sandbox
   make install-agent-sandbox KIND_CLUSTER=gofer
   make install-gvisor KIND_CLUSTER=gofer
   ```

   For a lighter setup with no extra substrate, use `PROVIDERS=k8s-job` and skip the
   two installs, then pass `AGENT_PROVIDER_CLASS=k8s-job` below.

2. Deploy gofer, from this checkout:

   ```sh
   export GITHUB_CLIENT_ID=... GITHUB_CLIENT_SECRET=...
   export AI_TOKEN=...          # optional: the chat-model token; unset = stub ranker, no Discuss
   make dev-up                  # AGENT_PROVIDER_CLASS defaults to "isolated"
   make dev-forward             # http://localhost:8080
   ```

   `dev-up` reads AEI's run-credential public key from the cluster, creates the
   `gofer-secrets` Secret, and applies the web app and its `AgentApp`. Override the
   chat models with `AI_CONNECTIONS` (see `deploy/k8s/base/configmap.yaml`).

After a code change to the workload only (`internal/agent`, `internal/llm`,
`internal/github`), `make kind-load-runtime` reloads the runtime image without
restarting the web pod.

Web-tier state (sessions, delegated tokens, the worklist and its threads) is durable:
the web tier is a single-replica StatefulSet with one SQLite database on a
PersistentVolume (`DATA_DIR`), and delegated tokens are encrypted at rest with
`VAULT_ENCRYPTION_KEY`, which `dev-up` generates into `gofer-secrets`. A pod restart
keeps logins and the worklist. See
[ADR 0004](docs/adr/0004-sqlite-persistent-storage.md) for the design and
[docs/backup-restore.md](docs/backup-restore.md) for volume snapshots and restore.

`make run` starts the web tier alone for UI/OAuth work; without a reachable
aei-controller, sign-in works but dispatch fails and the worklist stays empty. It
leaves `DATA_DIR` unset, so state is in memory; to persist locally, run it with
`DATA_DIR=./data VAULT_ENCRYPTION_KEY="$(openssl rand -base64 32)"` (reuse the same
key on later runs).
