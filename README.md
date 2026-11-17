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
preferred when both are installed; kind supports no other node provider, so the kind
loop uses `KIND_ENGINE` and is unaffected by Apple's `container` CLI); a GitHub OAuth
app whose callback is `http://localhost:8080/auth/github/callback`.

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

### Deploy to another cluster

`make dev-up` is the kind loop. To deploy to any other cluster, the way a CI job
would, push both images for the nodes' platform (log in to the registry first), then
pass a kubeconfig file. Neither step needs podman: `make deploy` uses only `kubectl`,
`jq`, `sed` and `openssl`, and `make push` builds with `CONTAINER_ENGINE`, which on
macOS defaults to Apple's [`container`](https://github.com/apple/container) CLI when it
is installed (`brew install container`, then `container system start
--enable-kernel-install` once, which also downloads its Linux kernel; Apple silicon and
macOS 26 or later). It keeps registry credentials in the macOS Keychain, so the first
push may show a Keychain access prompt: allow it, or the push fails with Keychain
error -128. Override the engine with `CONTAINER_ENGINE=podman` or `docker`.

```sh
make push IMAGE=ghcr.io/you/gofer:v1 RUNTIME_IMAGE=ghcr.io/you/gofer-runtime:v1 \
    PLATFORM=linux/amd64
make deploy KUBECONFIG=/path/to/kubeconfig \
    IMAGE=ghcr.io/you/gofer:v1 RUNTIME_IMAGE=ghcr.io/you/gofer-runtime:v1
```

Install AEI on that cluster first, with the same pattern from the AEI checkout
(`make push IMG=...`, then `make deploy KUBECONFIG=... IMG=... PROVIDERS=k8s-job`);
its `make public-key KUBECONFIG=...` prints the value for `MINT_PUBLIC_KEY`. Off
kind, pass `AGENT_PROVIDER_CLASS=k8s-job` here unless AEI's `isolated` class is
installed with a RuntimeClass the cluster has (`ISOLATION_CLASS` on the AEI side,
e.g. `kata-vm-isolation` on AKS; see the AKS example below).

`deploy` only applies: it does not create or manage the cluster, build or push
images (that is `make push`), or create secrets. It expects AEI installed, the `gofer-secrets` Secret in
place (template: `deploy/k8s/base/secret.example.yaml`), and both images pullable
by the cluster. It uses the kubeconfig's current context unless `KUBE_CONTEXT`
names another, and refuses to run without an explicit target. `dev-up` runs the
same apply step after its kind-specific setup. The other kubectl targets
(`dev-forward`, `dev-logs`, `dev-down`) accept `KUBECONFIG=` the same way.

### Example: AKS, end to end

From nothing to gofer at a stable public address with its database on an Azure
Disk. The cluster comes from AEI's `make create-aks` (in the AEI checkout; see its
README); everything after that is the ordinary any-cluster flow above. Each step is
a make command, so a CI job can run the same sequence. The cluster bills while it
exists (about $0.40/hour with the defaults).

```sh
# --- in the AEI checkout -----------------------------------------------------------
# A cluster with a Kata pool for runs, a registry, and a static public IP for gofer.
# The DNS label (unique in the region) names the address before it exists, so the
# GitHub OAuth app's callback can be registered up front:
#   http://<label>.westus2.cloudapp.azure.com/auth/github/callback
make create-aks AKS_PUBLIC_IP=gofer AKS_PUBLIC_IP_DNS_LABEL=<label>
# then the install commands create-aks prints (az acr login, make push,
# make install-agent-sandbox, make deploy ... ISOLATION_CLASS=kata-vm-isolation ...)

# --- in this checkout ----------------------------------------------------------------
KC=~/.kube/aei-aks-aei.kubeconfig  REG=<registry>.azurecr.io  TAG=$(git rev-parse --short HEAD)
# Log the image engine in to the registry. With Apple's container CLI (az acr login
# assumes docker):
#   az acr login -n <registry> --expose-token --query accessToken -o tsv | \
#     container registry login -u 00000000-0000-0000-0000-000000000000 --password-stdin $REG
make push IMAGE=$REG/gofer:$TAG RUNTIME_IMAGE=$REG/gofer-runtime:$TAG PLATFORM=linux/amd64
# First time only: gofer-secrets, from GITHUB_CLIENT_ID / GITHUB_CLIENT_SECRET /
# AI_TOKEN in the environment, AEI's verify key read from the cluster, and generated
# SESSION_SECRET / VAULT_ENCRYPTION_KEY (back the latter up; docs/backup-restore.md).
make dev-secrets KUBECONFIG=$KC
A=service.beta.kubernetes.io
make deploy KUBECONFIG=$KC IMAGE=$REG/gofer:$TAG RUNTIME_IMAGE=$REG/gofer-runtime:$TAG \
    PUBLIC_SERVICE=true \
    PUBLIC_SERVICE_ANNOTATIONS=$A/azure-load-balancer-resource-group=aei,$A/azure-pip-name=gofer,$A/azure-allowed-ip-ranges=<your-ip>/32 \
    BASE_URL=http://<label>.westus2.cloudapp.azure.com
make public-address KUBECONFIG=$KC      # waits for the load balancer; prints http://<ip>
curl http://<label>.westus2.cloudapp.azure.com/healthz

# --- when done, in the AEI checkout ----------------------------------------------------
make delete-aks AKS_PUBLIC_IP=gofer AKS_CONFIRM_DELETE=aei
```

What each piece is, and why:

- **Address.** `PUBLIC_SERVICE=true` adds `gofer-public`
  ([deploy/k8s/public/service.yaml](deploy/k8s/public/service.yaml)), a Service of
  type `LoadBalancer` on port 80, next to the in-cluster `gofer` Service that
  runtimes call. The two annotations bind it to the static IP `create-aks` made in
  its resource group, so the address (and its DNS name) stays the same across
  redeploys and even if `gofer-public` is deleted and re-created; without them, AKS
  picks an address that goes away with the Service. The DNS name lets you register
  the OAuth callback before the address exists.
- **Who can connect.** On AKS, limit access with the annotations
  `azure-allowed-ip-ranges` (CIDRs) and `azure-allowed-service-tags` (Azure service
  tags, e.g. a corporate VPN's tag, comma-separated), which the cloud provider writes
  into the cluster's network security group. Do not combine a service tag with
  `PUBLIC_SOURCE_RANGES` (`spec.loadBalancerSourceRanges`): kube-proxy also enforces
  that list on every node, so traffic the NSG admits through the tag is dropped
  there. `PUBLIC_SOURCE_RANGES` is for clouds without such annotations.
- **No TLS.** The address serves plain HTTP and `COOKIE_SECURE` stays `false`, so
  the session cookie crosses the network in the clear, and browsers that upgrade
  links to `https://` cannot reach it. Use it only for a first test; serve gofer over
  HTTPS (next section) before anything else, and before registering a GitHub
  webhook to it.
- **Disk.** The StatefulSet's `data` claim uses the cluster's default StorageClass
  (on AKS, `default`: Azure Disk CSI, Standard SSD LRS, `WaitForFirstConsumer`), so
  a 1Gi Azure Disk is provisioned on first deploy in the cluster's node resource
  group and attached to whichever node runs `gofer-0`. A disk attaches to one node
  at a time; the single replica and the StatefulSet's stop-before-start update
  respect that, and a move to another node waits for the detach (about 30 seconds).
  `create-aks` taints the Kata pool for agent runs, so `gofer-0` and its disk are on
  the system pool; its nodes are not zonal, so the disk is not either and can attach
  to any of them. The claim outlives the pod and the StatefulSet
  (`persistentVolumeClaimRetentionPolicy: Retain`); the disk is deleted with the
  cluster, so back it up first (docs/backup-restore.md) if the data matters.
- **Isolation.** `AGENT_PROVIDER_CLASS` defaults to `isolated`, which on this cluster
  is AEI's agent-sandbox class with `kata-vm-isolation`: each run in its own Kata VM
  on the Kata pool.

### HTTPS

HTTPS replaces `gofer-public` with an ingress: [Traefik](https://traefik.io) holds the
public address and terminates TLS, [cert-manager](https://cert-manager.io) obtains and
renews a Let's Encrypt certificate for the DNS name, and gofer runs with
`BASE_URL=https://<name>` and `COOKIE_SECURE=true`. (Traefik rather than ingress-nginx,
which was retired in March 2026.) The manifests are in
[deploy/k8s/tls](deploy/k8s/tls).

```sh
# Once per cluster: cert-manager and Traefik at pinned chart versions, with Traefik's
# Service on the static IP (it replaces gofer-public, which must be gone first).
A=service.beta.kubernetes.io
make public-down KUBECONFIG=$KC
make install-ingress KUBECONFIG=$KC \
    INGRESS_SERVICE_ANNOTATIONS=$A/azure-load-balancer-resource-group=<group>,$A/azure-pip-name=<ip-name>

# Every deploy: TLS_HOST switches HTTPS on (and is the certificate's name).
make deploy KUBECONFIG=$KC IMAGE=$REG/gofer:$TAG RUNTIME_IMAGE=$REG/gofer-runtime:$TAG \
    TLS_HOST=<label>.westus2.cloudapp.azure.com ALLOWED_GITHUB_LOGINS=<you>,...
make tls-status KUBECONFIG=$KC          # the certificate is Ready in about 30 seconds
```

Before switching, add `https://<name>/auth/github/callback` to the GitHub OAuth app's
redirect URIs (an app may have several, so keep the `http://` one until HTTPS works).

- **Certificate.** HTTP-01 validation: Let's Encrypt fetches a token from
  `http://<name>/.well-known/acme-challenge/` over the internet, so Traefik's Service
  accepts ports 80 and 443 from everywhere. Plain HTTP is otherwise redirected to
  HTTPS; the redirect runs at the lowest priority so the challenge route still answers.
  To prove the setup without spending production rate limits, deploy once with
  `ACME_ENV=staging` (an untrusted test certificate), then deploy with the default
  `ACME_ENV=production` and delete the `gofer-tls` Secret so cert-manager re-issues
  from production. cert-manager renews before expiry on its own; no contact email is
  registered.
- **Who can sign in.** `ALLOWED_GITHUB_LOGINS=you,colleague` on `make deploy`
  restricts sign-in to those GitHub accounts (case-insensitive). Anyone else who
  completes GitHub's consent screen gets a `403`, no session, and nothing stored, and
  the token GitHub issued for them is revoked. Removing a login also ends that
  account's existing session on its next request. Other sign-in providers are refused
  while the list is set. The value is kept in `gofer-config` across deploys that omit
  it. This is the access control to rely on: it works from any network.
- **Who can connect.** Optionally, `ALLOWED_SOURCE_RANGES` (comma-separated CIDRs;
  empty allows everyone) adds an address allow-list as Traefik `ipAllowList` on gofer's
  route only; other addresses get `403`. Traefik sees real client addresses because its
  Service uses `externalTrafficPolicy: Local`, but the address it sees is the one your
  HTTPS traffic leaves from, which a corporate network client may change: Microsoft's
  Global Secure Access, for example, tunnels port 443 through Microsoft's edge, so HTTPS
  arrives from a shared edge address, not your own.
- **Cookies.** With `COOKIE_SECURE=true` the session cookie is only sent over HTTPS.
  Existing sessions keep working, as the cookie value does not change.

`make run` starts the web tier alone for UI/OAuth work; without a reachable
aei-controller, sign-in works but dispatch fails and the worklist stays empty. It
leaves `DATA_DIR` unset, so state is in memory; to persist locally, run it with
`DATA_DIR=./data VAULT_ENCRYPTION_KEY="$(openssl rand -base64 32)"` (reuse the same
key on later runs).
