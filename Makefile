# gofer — dev loop
#
# The selected agent-runtime backend is AEI (internal/runtime/aei), so gofer is an AEI
# app: it consumes AEI's app SDK (sdks/go/aeiapp) and runtime SDK (aeiruntime) as a
# local sibling checkout via replace directives in go.mod — both stdlib-only. It
# imports NO provider and NO client-go: dispatch is an HTTP call to the pre-installed
# aei-controller, and the runtime is gofer's own image, launched per run.

.PHONY: build test vet tidy run runtime genkey clean \
        engine kind-engine build-linux image image-runtime push-check push kind-load kind-load-runtime push-runtime \
        cluster-up cluster-down dev-up dev-down dev-forward dev-logs \
        deploy deploy-target deploy-apply deploy-public public-address public-down \
        install-ingress deploy-tls tls-status \
        aei-check secrets-check dev-secrets \
        backup-apply backup-down snapshot snapshots restore \
        monitoring-load dev-monitoring dev-monitoring-down dev-forward-grafana dev-forward-prometheus

build:
	go build ./...

test:
	go test ./...

vet:
	go vet ./...

tidy:
	go mod tidy

# Run the web tier for local UI/OAuth work. It is verify-only, so it needs a public
# key (an ephemeral pair is generated here) and a dispatch endpoint; without a real
# aei-controller at AEI_DISPATCH_ENDPOINT, dispatch fails but the UI/OAuth work.
run:
	@keys="$$(go run ./hack/genkey)"; export $$keys; \
		SESSION_SECRET="$${SESSION_SECRET:-gofer-dev-session-secret-change-me-please-0123456789}" \
		AEI_DISPATCH_ENDPOINT="$${AEI_DISPATCH_ENDPOINT:-http://localhost:9999}" \
		AEI_APP="$${AEI_APP:-gofer}" \
		go run ./cmd/server

# Run the agent runtime (expects AEI_* injection in the environment).
runtime:
	go run ./cmd/runtime

# Generate an Ed25519 run-credential keypair: MINT_PUBLIC_KEY for gofer's verify-only
# web tier, plus a MINT_PRIVATE_KEY a runtime's minter holds (the web tier never does).
genkey:
	@go run ./hack/genkey

clean:
	rm -rf bin build gofer-image.tar gofer-runtime-image.tar

# ---- container images + kind dev cluster -------------------------------------
#
# gofer ships only its OWN application components (the web + runtime images and its
# app manifests) and ASSUMES AEI is already installed on the cluster — the
# aei-controller with its signing authority provisioned (AEI owns the keypair as sole
# minter; gofer verifies with AEI's public key), the agents.x-k8s.io CRDs, and an
# AgentProviderClass (gofer supplies its own runtime image, so the class is generic).
# Installing AEI is the AEI project's job.
#
# The image binaries are cross-compiled on the HOST (not inside Docker) because the
# backend's AEI dependencies are local `replace` directives to a sibling checkout that
# is outside any Docker build context (see Dockerfile).

# Two container engines, because two jobs need different things:
#
#   CONTAINER_ENGINE  builds and pushes the images a REAL cluster pulls (`make push`).
#                     On macOS the default is Apple's `container` CLI
#                     (https://github.com/apple/container; `brew install container`,
#                     then `container system start --enable-kernel-install` once),
#                     which needs no podman or Docker VM. Elsewhere, or without it:
#                     podman, then docker.
#   KIND_ENGINE       runs the kind dev cluster's nodes and loads images into them. kind
#                     supports only podman and docker as node providers, so the whole
#                     kind loop (dev-up, kind-load*, push-runtime, monitoring-load) uses
#                     this one and is unaffected by CONTAINER_ENGINE.
#
# Either can be overridden, e.g. CONTAINER_ENGINE=podman to push with podman.
UNAME_S := $(shell uname -s)
ifeq ($(UNAME_S),Darwin)
CONTAINER_ENGINE ?= $(shell command -v container 2>/dev/null || command -v podman 2>/dev/null || command -v docker 2>/dev/null)
else
CONTAINER_ENGINE ?= $(shell command -v podman 2>/dev/null || command -v docker 2>/dev/null)
endif
KIND_ENGINE ?= $(shell command -v podman 2>/dev/null || command -v docker 2>/dev/null)
IMAGE ?= localhost/gofer:dev
RUNTIME_IMAGE ?= localhost/gofer-runtime:dev
# The registry the DURABLE substrate (ate) pulls actor images from, by digest. In dev
# this is the local registry `make install-agent-substrate` stands up on the AEI side
# (published on the host at localhost:5001 and wired into the kind nodes). In a real
# deployment point this at a registry the substrate can pull (e.g. ghcr.io/you) and
# your CI pushes there. Only AGENT_PROVIDER_CLASS=durable uses it.
SUBSTRATE_REGISTRY ?= localhost:5001
RUNTIME_REGISTRY_IMAGE := $(SUBSTRATE_REGISTRY)/gofer-runtime:dev
KIND_CLUSTER ?= gofer
KUBE_NS ?= gofer
# Which cluster the kubectl targets act on. By default, the kind dev cluster's context
# in your usual kubeconfig. To act on any other cluster, pass a kubeconfig file on the
# command line: `make deploy KUBECONFIG=/path/to/kubeconfig`; that file's current
# context is then used unless KUBE_CONTEXT names another. A KUBECONFIG that is merely
# exported in your shell does not change the default context, so the dev targets keep
# pointing at kind.
KUBECONFIG_GIVEN := $(filter command line,$(origin KUBECONFIG))
KUBE_CONTEXT ?= $(if $(KUBECONFIG_GIVEN),,kind-$(KIND_CLUSTER))
KUBECTL = kubectl $(if $(KUBECONFIG_GIVEN),--kubeconfig "$(abspath $(KUBECONFIG))") $(if $(KUBE_CONTEXT),--context $(KUBE_CONTEXT))
KIND_CONFIG ?= deploy/kind/cluster.yaml
# The AgentProviderClass gofer's runs dispatch to — a well-known, admin-published name
# (e.g. "isolated", "durable", "k8s-job"). gofer is substrate-agnostic: it names a
# capability, not a platform. Empty selects the cluster default. `make dev-up` patches
# the AgentApp to this value, so the class must already exist on the AEI side. The
# default, "isolated", is AEI's agent-sandbox class (pods under gVisor); use "k8s-job"
# for a cluster with no extra substrate installed.
AGENT_PROVIDER_CLASS ?= isolated
WEB_LOCAL_PORT ?= 8080
# An address outside the cluster (deploy/k8s/public/service.yaml): PUBLIC_SERVICE=true
# adds the gofer-public Service of type LoadBalancer, with any PUBLIC_SERVICE_ANNOTATIONS
# (comma-separated key=value, e.g. the cloud's static-IP annotations; a piece with no
# "=" continues the previous value, so a=x,y,b=z gives a: "x,y" and b: "z") and
# PUBLIC_SOURCE_RANGES (comma-separated CIDRs allowed to connect; empty = anyone).
# PUBLIC_SERVICE=false (the default) leaves an existing one alone; `make public-down`
# removes it.
PUBLIC_SERVICE ?= false
PUBLIC_SERVICE_ANNOTATIONS ?=
PUBLIC_SOURCE_RANGES ?=
# The browser-facing base URL, written into gofer-config on every deploy when set
# (the OAuth callback is BASE_URL/auth/github/callback). Empty keeps the configmap's
# value (http://localhost:8080, the port-forward). With a public Service it is the
# address `make public-address` prints, or the static IP's DNS name.
BASE_URL ?=
# Who may sign in: comma-separated GitHub logins, written into gofer-config when set.
# It persists across later deploys that omit it (the key is not in the base manifest,
# so `kubectl apply` leaves it alone); clear it with
#   kubectl -n gofer patch configmap gofer-config --type=json -p '[{"op":"remove","path":"/data/ALLOWED_GITHUB_LOGINS"}]'
# Unset on the cluster, sign-in is open to any account (the server logs a warning).
ALLOWED_GITHUB_LOGINS ?=
# How long public-address waits for the cloud to assign the address, in seconds.
PUBLIC_ADDRESS_TIMEOUT ?= 300
# HTTPS (see README, "HTTPS"). `make install-ingress` installs Traefik and cert-manager
# once per cluster; `make deploy TLS_HOST=<dns name>` then serves gofer over HTTPS at
# that name with a Let's Encrypt certificate, instead of the plain-HTTP gofer-public.
#   TLS_HOST                     the public DNS name; setting it switches HTTPS on
#   ACME_ENV                     production (default) or staging (untrusted test certs)
#   ALLOWED_SOURCE_RANGES        comma-separated CIDRs allowed through to gofer; empty
#                                allows everyone. Enforced by Traefik per route, so the
#                                ACME challenge stays reachable for Let's Encrypt.
#   INGRESS_SERVICE_ANNOTATIONS  install-ingress only: annotations for Traefik's
#                                LoadBalancer Service, same format as
#                                PUBLIC_SERVICE_ANNOTATIONS (e.g. the AKS static IP)
TLS_HOST ?=
ACME_ENV ?= production
ALLOWED_SOURCE_RANGES ?=
INGRESS_SERVICE_ANNOTATIONS ?=
ACME_SERVER_production = https://acme-v02.api.letsencrypt.org/directory
ACME_SERVER_staging = https://acme-staging-v02.api.letsencrypt.org/directory
ACME_SERVER = $(ACME_SERVER_$(ACME_ENV))
# Pinned chart versions (bump deliberately). Traefik replaces ingress-nginx, which was
# retired in March 2026.
TRAEFIK_CHART_VERSION ?= 42.2.0
CERT_MANAGER_CHART_VERSION ?= v1.21.3
HELM = helm $(if $(KUBECONFIG_GIVEN),--kubeconfig "$(abspath $(KUBECONFIG))") $(if $(KUBE_CONTEXT),--kube-context $(KUBE_CONTEXT))
# How long deploy waits for the web tier. A first rollout on a cloud cluster also
# provisions and attaches the data disk.
ROLLOUT_TIMEOUT ?= 180s
# ---- data protection (docs/backup-restore.md) ----
# The claim that holds gofer's database.
DATA_CLAIM := data-gofer-0
# The reclaim policy deploy sets on the data volume once it is bound. Retain keeps the
# disk if the claim or the gofer namespace is ever deleted (a restore depends on
# this too); storage classes default to Delete. The kind loop uses Delete, so
# `make dev-down` leaves nothing behind.
DATA_RECLAIM_POLICY ?= Retain
# Scheduled snapshots (`make backup-apply`). SNAPSHOT_CLASS names a VolumeSnapshotClass
# the cluster's administrator created; it decides where snapshots are stored, so it is
# never written into this repository. The schedule is cron syntax, in UTC.
SNAPSHOT_CLASS ?=
SNAPSHOT_SCHEDULE ?= 0 */6 * * *
SNAPSHOT_KEEP ?= 28
# `make snapshot` waits this long for an on-demand snapshot, in seconds.
SNAPSHOT_TIMEOUT ?= 900
# `make restore SNAPSHOT=<name> CONFIRM=<name>`: the snapshot to restore, named twice.
SNAPSHOT ?=
CONFIRM ?=
# Optional monitoring stack (opt-in; see `make dev-monitoring`).
MONITORING_NS ?= gofer-monitoring
PROMETHEUS_IMAGE ?= docker.io/prom/prometheus:v3.1.0
GRAFANA_IMAGE ?= docker.io/grafana/grafana:11.4.0
GRAFANA_LOCAL_PORT ?= 3000
PROM_LOCAL_PORT ?= 9090
# The platform the images are built for, e.g. PLATFORM=linux/amd64 for a cloud cluster
# built from an arm64 laptop. Empty builds for the host's architecture, which is what
# the kind nodes run (arm64 on Apple Silicon).
PLATFORM ?=
PLATFORM_FLAG = $(if $(PLATFORM),--platform $(PLATFORM))
GOARCH ?= $(if $(PLATFORM),$(word 2,$(subst /, ,$(PLATFORM))),$(shell go env GOARCH))

# Image commands for CONTAINER_ENGINE. Apple's CLI groups them under `image`
# (`container image push`); podman and docker take them at the top level.
ifeq ($(notdir $(CONTAINER_ENGINE)),container)
ENGINE_PUSH = $(CONTAINER_ENGINE) image push
else
ENGINE_PUSH = $(CONTAINER_ENGINE) push
endif

# kind drives podman only when told to; derive it from KIND_ENGINE so a podman-only
# machine does not fail with "docker: executable file not found".
ifeq ($(notdir $(KIND_ENGINE)),podman)
export KIND_EXPERIMENTAL_PROVIDER = podman
# podman needs --tls-verify=false to push to the local HTTP registry; docker treats
# localhost registries as insecure by default and needs no flag.
RUNTIME_PUSH_FLAGS = --tls-verify=false
endif

# Check the image engine is installed and, for Apple's CLI, that its services run.
engine:
	@test -n "$(CONTAINER_ENGINE)" || { echo "no container engine found (on macOS: brew install container; otherwise install podman or docker)"; exit 1; }
	@if [ "$(notdir $(CONTAINER_ENGINE))" = container ]; then \
		$(CONTAINER_ENGINE) system status >/dev/null 2>&1 || { \
			echo "ERROR: Apple container services are not running. Start them with: container system start"; \
			echo "       (the first time: container system start --enable-kernel-install)"; exit 1; }; \
	fi
	@echo "using container engine: $(CONTAINER_ENGINE)"

# Check the kind loop's engine: kind needs podman or docker, never Apple's CLI.
kind-engine:
	@test -n "$(KIND_ENGINE)" || { echo "the kind dev loop needs podman or docker (kind supports no other node provider)"; exit 1; }
	@case "$(notdir $(KIND_ENGINE))" in podman|docker) ;; *) \
		echo "ERROR: KIND_ENGINE=$(KIND_ENGINE): kind supports only podman or docker."; exit 1;; esac
	@echo "using kind engine: $(KIND_ENGINE)"

# Cross-compile the web + runtime binaries for the kind nodes (static, stripped).
build-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) go build -trimpath -ldflags="-s -w" -o bin/linux/server ./cmd/server
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) go build -trimpath -ldflags="-s -w" -o bin/linux/runtime ./cmd/runtime
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) go build -trimpath -ldflags="-s -w" -o bin/linux/snapshot ./cmd/snapshot

# Build the web-tier image (packages the prebuilt bin/linux/server and bin/linux/snapshot).
image: engine build-linux
	$(CONTAINER_ENGINE) build $(PLATFORM_FLAG) -f Dockerfile -t $(IMAGE) .

# Build the agent-runtime image (packages the prebuilt bin/linux/runtime).
image-runtime: engine build-linux
	$(CONTAINER_ENGINE) build $(PLATFORM_FLAG) -f Dockerfile.runtime -t $(RUNTIME_IMAGE) .

# A non-kind cluster pulls both images from a registry, so they need registry names and
# the nodes' platform. The dev defaults (localhost/...) are not pushable. Log in to the
# registry first; `make deploy` then takes the same IMAGE / RUNTIME_IMAGE.
#     make push IMAGE=<registry>/gofer:<tag> RUNTIME_IMAGE=<registry>/gofer-runtime:<tag> PLATFORM=linux/amd64
push-check:
	@for i in "$(IMAGE)" "$(RUNTIME_IMAGE)"; do case "$$i" in localhost/*) \
		echo "ERROR: $$i is a kind dev tag. Set IMAGE=<registry>/gofer:<tag> and RUNTIME_IMAGE=<registry>/gofer-runtime:<tag>."; exit 1;; esac; done

push: push-check image image-runtime  ## build + push the web and runtime images
	$(ENGINE_PUSH) $(IMAGE)
	$(ENGINE_PUSH) $(RUNTIME_IMAGE)

# Load the web image into kind via an engine-agnostic archive. Built with KIND_ENGINE,
# not CONTAINER_ENGINE: the image has to be in the store kind loads from, and an
# archive from Apple's CLI is not a format kind is known to import.
kind-load: kind-engine
	@$(MAKE) --no-print-directory image CONTAINER_ENGINE=$(KIND_ENGINE)
	$(KIND_ENGINE) save $(IMAGE) -o gofer-image.tar
	kind load image-archive gofer-image.tar --name $(KIND_CLUSTER)
	rm -f gofer-image.tar

# Load the runtime image into kind so a per-run launcher resolves it without a
# registry pull.
kind-load-runtime: kind-engine
	@$(MAKE) --no-print-directory image-runtime CONTAINER_ENGINE=$(KIND_ENGINE)
	$(KIND_ENGINE) save $(RUNTIME_IMAGE) -o gofer-runtime-image.tar
	kind load image-archive gofer-runtime-image.tar --name $(KIND_CLUSTER)
	rm -f gofer-runtime-image.tar

# Publish the runtime image to the substrate registry (durable path). The DURABLE
# substrate pulls actor images from a registry by digest — a tag invalidates ate's
# golden snapshot, and kind-loaded images are not registry-pullable — so `make dev-up`
# with AGENT_PROVIDER_CLASS=durable pushes here and pins the AgentApp image to the
# pushed digest automatically. This target is the manual build+push half.
# Part of the kind loop (the registry is the kind-side one), so it uses KIND_ENGINE.
push-runtime: kind-engine  ## build + push the runtime image to SUBSTRATE_REGISTRY (for durable)
	@$(MAKE) --no-print-directory image-runtime CONTAINER_ENGINE=$(KIND_ENGINE)
	$(KIND_ENGINE) tag $(RUNTIME_IMAGE) $(RUNTIME_REGISTRY_IMAGE)
	$(KIND_ENGINE) push $(RUNTIME_PUSH_FLAGS) $(RUNTIME_REGISTRY_IMAGE)

# Create the kind cluster if it does not already exist. Install AEI separately (its
# own targets) before `make dev-up`.
#
# The topology comes from KIND_CONFIG (three nodes by default), but only when GOFER
# creates the cluster. AEI's `kind-up` also creates it — from its own KIND_CONFIG
# (hack/kind/cluster.yaml, a single node) — and both targets reuse an existing cluster
# untouched, so whoever runs first owns the node count. kind cannot reshape a cluster
# in place, so a smaller existing cluster is called out rather than silently tolerated:
# the node count is what bounds a bulk review's per-run pod fan-out.
cluster-up:
	@command -v kind >/dev/null || { echo "kind not installed (https://kind.sigs.k8s.io)"; exit 1; }
	@kind get clusters 2>/dev/null | grep -qx $(KIND_CLUSTER) || kind create cluster --name $(KIND_CLUSTER) --config $(KIND_CONFIG)
	@nodes=$$($(KUBECTL) get nodes --no-headers 2>/dev/null | wc -l | tr -d ' '); \
		want=$$(grep -c '^  - role:' $(KIND_CONFIG)); \
		[ "$${nodes:-0}" -ge "$$want" ] || { \
			echo "NOTE: cluster '$(KIND_CLUSTER)' already exists with $$nodes node(s); $(KIND_CONFIG) asks for $$want."; \
			echo "      kind cannot resize in place, and re-running dev-up will NOT help: AEI's installer"; \
			echo "      creates the cluster from its own single-node config. Recreate it from this one:"; \
			echo "        make cluster-down && make -C ../agent-execution-interface kind-up \\"; \
			echo "            KIND_CLUSTER=$(KIND_CLUSTER) KIND_CONFIG=$$PWD/$(KIND_CONFIG)"; \
			echo "      then finish AEI's install and re-run 'make dev-up'."; }

# Delete the kind cluster.
cluster-down:
	-kind delete cluster --name $(KIND_CLUSTER)

# ---- deploy ---------------------------------------------------------------------
#
# Two entry points share one apply path (deploy-apply):
#
#   make dev-up                       the kind dev loop: create the cluster, build and
#                                     load the images, generate the Secret, then apply.
#   make deploy KUBECONFIG=<file>     what a CI job runs against a real cluster: apply
#                                     only. gofer does not create, size or manage the
#                                     cluster; it needs a kubeconfig that reaches one.
#
# `deploy` expects, already in place: AEI installed on the cluster, the gofer-secrets
# Secret (see deploy/k8s/base/secret.example.yaml), and IMAGE / RUNTIME_IMAGE pushed
# to a registry the cluster can pull from, for example:
#
#   make deploy KUBECONFIG=$$HOME/.kube/aks-prod \
#       IMAGE=ghcr.io/you/gofer:v1 RUNTIME_IMAGE=ghcr.io/you/gofer-runtime:v1 \
#       AGENT_PROVIDER_CLASS=isolated
#
# With AEI selected as the agent-runtime backend, gofer CANNOT run standalone: both
# entry points fail fast if the AEI CRDs are absent. AEI (its controller, CRDs, and an
# AgentProviderClass) is installed separately on the AEI side, including its
# run-credential signing authority (AEI owns the keypair as sole minter; gofer's web
# tier verifies with AEI's public key, MINT_PUBLIC_KEY — it holds no minting key).

# `deploy` must be told which cluster: a kubeconfig file or an explicit context. It
# never falls back to the kind default, so a bare `make deploy` cannot land somewhere
# unintended.
# The backup and restore targets use the same guard.
deploy-target:
	@[ -n "$(KUBECONFIG_GIVEN)" ] || [ "$(origin KUBE_CONTEXT)" != "file" ] || { \
		echo "ERROR: 'make $(firstword $(MAKECMDGOALS))' needs an explicit target cluster:"; \
		echo "         make $(firstword $(MAKECMDGOALS)) KUBECONFIG=/path/to/kubeconfig [KUBE_CONTEXT=name]"; \
		echo "       (for the kind dev cluster, use 'make dev-up')."; \
		exit 1; }
	@[ -z "$(KUBECONFIG_GIVEN)" ] || [ -f "$(abspath $(KUBECONFIG))" ] || { \
		echo "ERROR: kubeconfig not found: $(abspath $(KUBECONFIG))"; exit 1; }
	@echo "cluster: context '$$($(KUBECTL) config current-context 2>/dev/null)' at $$($(KUBECTL) config view --minify -o jsonpath='{.clusters[0].cluster.server}')"

aei-check:
	@$(KUBECTL) get crd agentapps.agents.x-k8s.io >/dev/null 2>&1 || { \
		echo "ERROR: AEI is not installed (agents.x-k8s.io CRDs missing). This build dispatches to"; \
		echo "       the aei-controller and cannot run without it. Install AEI, then re-run."; \
		exit 1; }

# `deploy` does not create secrets: a real cluster's keys come from your secret manager,
# not from a make target. It only checks that the ones the server refuses to start
# without are present.
secrets-check:
	@for k in SESSION_SECRET VAULT_ENCRYPTION_KEY MINT_PUBLIC_KEY; do \
		$(KUBECTL) -n $(KUBE_NS) get secret gofer-secrets -o jsonpath="{.data.$$k}" 2>/dev/null | grep -q . || { \
			echo "ERROR: Secret $(KUBE_NS)/gofer-secrets is missing or has no $$k."; \
			echo "       Create it before deploying (template: deploy/k8s/base/secret.example.yaml)."; \
			exit 1; }; \
	done

deploy: deploy-target aei-check secrets-check deploy-apply

# The kind dev loop. SESSION_SECRET and VAULT_ENCRYPTION_KEY are generated;
# MINT_PUBLIC_KEY is AEI's published verify key (read from
# the cluster's aei-signing secret, or passed explicitly); GITHUB_CLIENT_ID /
# GITHUB_CLIENT_SECRET and AI_TOKEN (the chat-model token gofer brokers to runtimes;
# unset = stub ranker, no LLM) are seeded from your shell env when set. AI_CONNECTIONS
# defaults from the configmap but can likewise be overridden from the shell.
# The GitHub OAuth callback for the port-forward is http://localhost:$(WEB_LOCAL_PORT)/auth/github/callback.
dev-secrets:
	@$(KUBECTL) create namespace $(KUBE_NS) --dry-run=client -o yaml | $(KUBECTL) apply -f -
	@# AEI owns the run-credential keypair and is the sole minter; gofer's web tier only
	@# VERIFIES, with AEI's PUBLIC key. Provide it via MINT_PUBLIC_KEY, or let dev-up read
	@# AEI's published key from the cluster (both share one kind cluster in dev). gofer
	@# feeds AEI nothing secret.
	@mpk="$${MINT_PUBLIC_KEY:-$$($(KUBECTL) -n aei-system get secret aei-signing -o jsonpath='{.data.AEI_ED25519_PUBLIC_KEY}' 2>/dev/null | base64 -d)}"; \
		test -n "$$mpk" || { \
			echo "ERROR: no AEI run-credential verify key found. On the AEI side run 'make authority'"; \
			echo "       (provisions the platform keypair), then 'make public-key', and re-run this with"; \
			echo "       MINT_PUBLIC_KEY=<that key>."; \
			exit 1; }; \
		$(KUBECTL) -n $(KUBE_NS) get secret gofer-secrets >/dev/null 2>&1 || \
			$(KUBECTL) -n $(KUBE_NS) create secret generic gofer-secrets \
				--from-literal=SESSION_SECRET="$$(openssl rand -base64 48)" \
				--from-literal=VAULT_ENCRYPTION_KEY="$$(openssl rand -base64 32)" \
				--from-literal=MINT_PUBLIC_KEY="$$mpk" \
				--from-literal=GITHUB_CLIENT_ID="$${GITHUB_CLIENT_ID:-}" \
				--from-literal=GITHUB_CLIENT_SECRET="$${GITHUB_CLIENT_SECRET:-}" \
				--from-literal=AI_TOKEN="$${AI_TOKEN:-}"; \
		$(KUBECTL) -n $(KUBE_NS) patch secret gofer-secrets --type=merge \
			-p "{\"stringData\":{\"MINT_PUBLIC_KEY\":\"$$mpk\"}}"
	@[ -z "$$GITHUB_CLIENT_ID" ] || $(KUBECTL) -n $(KUBE_NS) patch secret gofer-secrets --type=merge -p "{\"stringData\":{\"GITHUB_CLIENT_ID\":\"$$GITHUB_CLIENT_ID\"}}"
	@[ -z "$$GITHUB_CLIENT_SECRET" ] || $(KUBECTL) -n $(KUBE_NS) patch secret gofer-secrets --type=merge -p "{\"stringData\":{\"GITHUB_CLIENT_SECRET\":\"$$GITHUB_CLIENT_SECRET\"}}"
	@[ -z "$$AI_TOKEN" ] || $(KUBECTL) -n $(KUBE_NS) patch secret gofer-secrets --type=merge -p "{\"stringData\":{\"AI_TOKEN\":\"$$AI_TOKEN\"}}"
	@# A gofer-secrets created before persistent storage has no token-encryption key.
	@# Add one if it is missing; never replace an existing key (it seals stored tokens).
	@$(KUBECTL) -n $(KUBE_NS) get secret gofer-secrets -o jsonpath='{.data.VAULT_ENCRYPTION_KEY}' | grep -q . || \
		$(KUBECTL) -n $(KUBE_NS) patch secret gofer-secrets --type=merge \
			-p "{\"stringData\":{\"VAULT_ENCRYPTION_KEY\":\"$$(openssl rand -base64 32)\"}}"

# The shared apply path: the web tier, its config, and the AgentApp, with the images
# set to IMAGE / RUNTIME_IMAGE, then a rollout. Cluster-agnostic.
deploy-apply:
	@$(KUBECTL) create namespace $(KUBE_NS) --dry-run=client -o yaml | $(KUBECTL) apply -f -
	@# The web tier was a Deployment before it had a volume. Remove a leftover one so it
	@# does not keep serving, in memory, beside the StatefulSet behind the same Service.
	$(KUBECTL) -n $(KUBE_NS) delete deployment gofer --ignore-not-found
	@# The manifests name the kind-loaded dev images; substitute the ones being deployed
	@# (a no-op for the dev defaults).
	$(KUBECTL) kustomize deploy/k8s/base | sed 's|image: localhost/gofer:dev$$|image: $(IMAGE)|' | $(KUBECTL) apply -f -
	@[ -z "$$AI_CONNECTIONS" ] || $(KUBECTL) -n $(KUBE_NS) patch configmap gofer-config --type=merge -p "$$(jq -nc --arg v "$$AI_CONNECTIONS" '{data:{AI_CONNECTIONS:$$v}}')"
	@[ -z "$(BASE_URL)" ] || $(KUBECTL) -n $(KUBE_NS) patch configmap gofer-config --type=merge -p "$$(jq -nc --arg v "$(BASE_URL)" '{data:{BASE_URL:$$v}}')"
	@[ -z "$(ALLOWED_GITHUB_LOGINS)" ] || $(KUBECTL) -n $(KUBE_NS) patch configmap gofer-config --type=merge -p "$$(jq -nc --arg v "$(ALLOWED_GITHUB_LOGINS)" '{data:{ALLOWED_GITHUB_LOGINS:$$v}}')"
	@[ "$(PUBLIC_SERVICE)" != true ] || $(MAKE) --no-print-directory deploy-public
	@[ -z "$(TLS_HOST)" ] || $(MAKE) --no-print-directory deploy-tls
	sed 's|image: localhost/gofer-runtime:dev$$|image: $(RUNTIME_IMAGE)|' deploy/k8s/overlays/dev/agentapp.yaml | $(KUBECTL) apply -f -
	@# Point the AgentApp at the well-known class name the admin published on the AEI side
	@# (AGENT_PROVIDER_CLASS). gofer references a capability by name, not a substrate — an
	@# empty value would select the cluster-default class.
	$(KUBECTL) -n $(KUBE_NS) patch agentapp gofer --type=merge \
		-p '{"spec":{"providerClassName":"$(AGENT_PROVIDER_CLASS)"}}'
	$(KUBECTL) rollout restart statefulset/gofer -n $(KUBE_NS)
	$(KUBECTL) rollout status statefulset/gofer -n $(KUBE_NS) --timeout=$(ROLLOUT_TIMEOUT)
	@# Keep the data disk if its claim or the namespace is ever deleted. The volume is
	@# bound by now (the pod that mounts it is ready).
	@pv=$$($(KUBECTL) -n $(KUBE_NS) get pvc $(DATA_CLAIM) -o jsonpath='{.spec.volumeName}' 2>/dev/null); \
		if [ -n "$$pv" ]; then \
			$(KUBECTL) patch pv "$$pv" -p '{"spec":{"persistentVolumeReclaimPolicy":"$(DATA_RECLAIM_POLICY)"}}' >/dev/null && \
			echo "data volume $$pv: reclaim policy $(DATA_RECLAIM_POLICY)"; \
		else echo "WARNING: $(DATA_CLAIM) is not bound; its reclaim policy was not set."; fi
	@# Keep the backup job, if set up, on the image just deployed.
	@if $(KUBECTL) -n $(KUBE_NS) get cronjob gofer-snapshot >/dev/null 2>&1; then \
		$(KUBECTL) -n $(KUBE_NS) set image cronjob/gofer-snapshot snapshot=$(IMAGE) >/dev/null; fi

# The public Service, rendered with its annotations and source ranges, then applied.
# Re-applying with a different PUBLIC_SERVICE_ANNOTATIONS replaces the set.
deploy-public:
	kubectl patch --local -o yaml -f deploy/k8s/public/service.yaml --type=merge -p "$$(jq -nc \
		--arg a "$(PUBLIC_SERVICE_ANNOTATIONS)" --arg r "$(PUBLIC_SOURCE_RANGES)" \
		'{metadata: {annotations: (reduce ($$a | split(",")[] | select(. != "")) as $$t ([]; \
			if ($$t | test("=")) then . + [$$t] else .[:-1] + [.[-1] + "," + $$t] end) \
			| map({key: (split("=")[0]), value: (split("=")[1:] | join("="))}) | from_entries)}, \
		  spec: {loadBalancerSourceRanges: (if $$r == "" then null else ($$r | split(",")) end)}}')" | \
		$(KUBECTL) apply -f -

# Wait for the public Service's external address and print it as a URL (the value for
# BASE_URL when there is no DNS name), or fail after PUBLIC_ADDRESS_TIMEOUT seconds.
public-address:
	@i=0; while :; do \
		a="$$($(KUBECTL) -n $(KUBE_NS) get service gofer-public -o jsonpath='{.status.loadBalancer.ingress[0].ip}{.status.loadBalancer.ingress[0].hostname}' 2>/dev/null)"; \
		[ -z "$$a" ] || { echo "http://$$a"; exit 0; }; \
		[ $$i -lt $(PUBLIC_ADDRESS_TIMEOUT) ] || { echo "ERROR: service $(KUBE_NS)/gofer-public has no external address after $(PUBLIC_ADDRESS_TIMEOUT)s:" >&2; \
			$(KUBECTL) -n $(KUBE_NS) describe service gofer-public 2>&1 | tail -15 >&2; exit 1; }; \
		sleep 5; i=$$((i + 5)); done

public-down:
	$(KUBECTL) -n $(KUBE_NS) delete service gofer-public --ignore-not-found

# ---- HTTPS ------------------------------------------------------------------------
#
# Install the ingress layer gofer's HTTPS route needs, once per cluster: cert-manager
# (with its CRDs) and Traefik, at the pinned chart versions. Like AEI, this is cluster
# infrastructure, so it is a separate, explicit step and `deploy` never runs it.
#
# Traefik's Service is the cluster's public entry point: pass the static-IP annotations
# in INGRESS_SERVICE_ANNOTATIONS (on AKS, the same pair gofer-public used; delete
# gofer-public first with `make public-down`, since one address cannot serve both). It
# accepts ports 80 and 443 from everywhere (Let's Encrypt must reach port 80), with
# externalTrafficPolicy: Local so Traefik sees real client addresses and can apply
# ALLOWED_SOURCE_RANGES per route. HTTP is redirected to HTTPS at the lowest priority,
# which leaves cert-manager's challenge route answering on port 80.
install-ingress: deploy-target
	@[ "$(PUBLIC_SERVICE)" != true ] || { echo "ERROR: install-ingress replaces gofer-public; do not pass PUBLIC_SERVICE=true"; exit 1; }
	$(HELM) upgrade --install cert-manager cert-manager --repo https://charts.jetstack.io \
		--version $(CERT_MANAGER_CHART_VERSION) --namespace cert-manager --create-namespace \
		--set crds.enabled=true --wait --timeout 5m
	jq -n --arg a "$(INGRESS_SERVICE_ANNOTATIONS)" '{ \
		ingressClass: {enabled: true, isDefaultClass: false, name: "traefik"}, \
		ports: {web: {http: {redirections: {entryPoint: {to: "websecure", scheme: "https", permanent: true, priority: 1}}}}}, \
		service: {spec: {externalTrafficPolicy: "Local"}, annotations: (reduce ($$a | split(",")[] | select(. != "")) as $$t ([]; \
			if ($$t | test("=")) then . + [$$t] else .[:-1] + [.[-1] + "," + $$t] end) \
			| map({key: (split("=")[0]), value: (split("=")[1:] | join("="))}) | from_entries)}}' | \
		$(HELM) upgrade --install traefik traefik --repo https://traefik.github.io/charts \
			--version $(TRAEFIK_CHART_VERSION) --namespace traefik --create-namespace \
			--values - --wait --timeout 5m
	@echo "ingress installed. Its address (point the DNS name / TLS_HOST at it):"
	@$(KUBECTL) -n traefik get service traefik -o jsonpath='{.status.loadBalancer.ingress[0].ip}{.status.loadBalancer.ingress[0].hostname}{"\n"}'

# gofer's HTTPS route: the ACME Issuer, the allow-list, and the Ingress for TLS_HOST,
# then BASE_URL=https://TLS_HOST (unless BASE_URL is given) and COOKIE_SECURE=true so
# the session cookie never travels over plain HTTP. Run by deploy-apply when TLS_HOST
# is set; the rollout that follows picks up the configuration.
deploy-tls:
	@[ "$(PUBLIC_SERVICE)" != true ] || { echo "ERROR: TLS_HOST and PUBLIC_SERVICE=true are alternatives; drop PUBLIC_SERVICE"; exit 1; }
	@[ -n "$(ACME_SERVER)" ] || { echo "ERROR: ACME_ENV must be production or staging (got '$(ACME_ENV)')"; exit 1; }
	@$(KUBECTL) get crd certificates.cert-manager.io middlewares.traefik.io >/dev/null 2>&1 || { \
		echo "ERROR: cert-manager and Traefik are not installed on this cluster. Run 'make install-ingress' first."; exit 1; }
	@# gofer-public held the address before HTTPS; Traefik's Service holds it now.
	$(KUBECTL) -n $(KUBE_NS) delete service gofer-public --ignore-not-found
	sed 's|__ACME_SERVER__|$(ACME_SERVER)|' deploy/k8s/tls/issuer.yaml | $(KUBECTL) apply -f -
	kubectl patch --local -o yaml -f deploy/k8s/tls/middleware.yaml --type=merge -p "$$(jq -nc --arg r "$(ALLOWED_SOURCE_RANGES)" \
		'{spec: {ipAllowList: {sourceRange: (if $$r == "" then ["0.0.0.0/0", "::/0"] else ($$r | split(",") | map(select(. != ""))) end)}}}')" | \
		$(KUBECTL) apply -f -
	sed 's|__TLS_HOST__|$(TLS_HOST)|g' deploy/k8s/tls/ingress.yaml | $(KUBECTL) apply -f -
	$(KUBECTL) -n $(KUBE_NS) patch configmap gofer-config --type=merge -p "$$(jq -nc \
		--arg b "$(if $(BASE_URL),$(BASE_URL),https://$(TLS_HOST))" '{data: {BASE_URL: $$b, COOKIE_SECURE: "true"}}')"
	@echo "HTTPS route applied for $(TLS_HOST) ($(ACME_ENV) certificate). Watch it with: make tls-status"

# The certificate's progress: Ready once Let's Encrypt has issued it. Orders and
# challenges appear only while an issuance is in flight.
tls-status:
	@$(KUBECTL) -n $(KUBE_NS) get certificate,certificaterequest,order,challenge 2>&1

dev-up: DATA_RECLAIM_POLICY = Delete
dev-up: cluster-up kind-load kind-load-runtime aei-check dev-secrets deploy-apply
	@# Durable pulls actor images from a registry BY DIGEST (a tag invalidates ate's golden
	@# snapshot; kind-loaded images are not registry-pullable). So for the durable class,
	@# push the runtime to the substrate registry and pin the AgentApp image to the pushed
	@# digest. Every other class keeps the kind-loaded :dev tag applied elsewhere.
	@if [ "$(AGENT_PROVIDER_CLASS)" = "durable" ]; then \
		command -v curl >/dev/null 2>&1 || { echo "ERROR: curl is required to read the pushed image digest"; exit 1; }; \
		curl -sSf -o /dev/null http://$(SUBSTRATE_REGISTRY)/v2/ 2>/dev/null || { \
			echo "ERROR: substrate registry $(SUBSTRATE_REGISTRY) is not reachable."; \
			echo "       Run 'make install-agent-substrate' on the AEI side first (it stands up the registry),"; \
			echo "       or set SUBSTRATE_REGISTRY=<a registry ate can pull>."; \
			exit 1; }; \
		echo "durable: pushing $(RUNTIME_IMAGE) -> $(RUNTIME_REGISTRY_IMAGE)"; \
		$(KIND_ENGINE) tag $(RUNTIME_IMAGE) $(RUNTIME_REGISTRY_IMAGE); \
		$(KIND_ENGINE) push $(RUNTIME_PUSH_FLAGS) $(RUNTIME_REGISTRY_IMAGE); \
		digest=$$(curl -sSf -D - -o /dev/null \
			-H 'Accept: application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json, application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json' \
			http://$(SUBSTRATE_REGISTRY)/v2/gofer-runtime/manifests/dev | \
			awk 'tolower($$1) ~ /docker-content-digest/ {print $$2}' | tr -d '\r' | tail -1); \
		test -n "$$digest" || { echo "ERROR: could not read the pushed image digest from $(SUBSTRATE_REGISTRY)"; exit 1; }; \
		echo "durable: pinning AgentApp image to $(SUBSTRATE_REGISTRY)/gofer-runtime@$$digest"; \
		$(KUBECTL) -n $(KUBE_NS) patch agentapp gofer --type=merge \
			-p "{\"spec\":{\"image\":\"$(SUBSTRATE_REGISTRY)/gofer-runtime@$$digest\"}}"; \
	fi

# ---- backup and restore (docs/backup-restore.md) ------------------------------
#
# Scheduled CSI snapshots of the data volume, and a guarded restore. Cluster-agnostic,
# for any cluster with CSI snapshot support; not kind. Every target needs an explicit
# cluster (KUBECONFIG=<file> or KUBE_CONTEXT=<name>), like `make deploy`.
#
#   make backup-apply SNAPSHOT_CLASS=<class> [SNAPSHOT_SCHEDULE="0 */6 * * *"] [SNAPSHOT_KEEP=28]
#   make snapshot                                take one now and wait for it
#   make snapshots                               list snapshots
#   make restore SNAPSHOT=<name> CONFIRM=<name>  replace the data volume with a snapshot
#   make backup-down                             stop scheduling (keeps every snapshot)

# Install or update the snapshot CronJob, on the image the web tier runs.
backup-apply: deploy-target
	@[ -n "$(SNAPSHOT_CLASS)" ] || { echo "ERROR: set SNAPSHOT_CLASS=<a VolumeSnapshotClass> (see docs/backup-restore.md)."; exit 1; }
	@case "$(SNAPSHOT_CLASS)" in *[!a-z0-9.-]*) echo "ERROR: SNAPSHOT_CLASS must be a Kubernetes object name."; exit 1;; esac
	@case "$(SNAPSHOT_KEEP)" in ''|*[!0-9]*|0) echo "ERROR: SNAPSHOT_KEEP must be a positive integer."; exit 1;; esac
	@case "$(SNAPSHOT_SCHEDULE)" in *[!0-9\ \*/,-]*|'') echo "ERROR: SNAPSHOT_SCHEDULE must be a five-field cron expression, e.g. \"0 */6 * * *\"."; exit 1;; esac
	@$(KUBECTL) get crd volumesnapshots.snapshot.storage.k8s.io >/dev/null 2>&1 || { \
		echo "ERROR: this cluster has no CSI snapshot support (the volumesnapshots CRD is missing)."; exit 1; }
	@$(KUBECTL) get volumesnapshotclass "$(SNAPSHOT_CLASS)" >/dev/null 2>&1 || { \
		echo "ERROR: VolumeSnapshotClass $(SNAPSHOT_CLASS) not found. Create it first (docs/backup-restore.md)."; exit 1; }
	@policy=$$($(KUBECTL) get volumesnapshotclass "$(SNAPSHOT_CLASS)" -o jsonpath='{.deletionPolicy}'); \
		[ "$$policy" = Retain ] || echo "WARNING: $(SNAPSHOT_CLASS) has deletionPolicy $$policy: deleting the gofer namespace would delete every stored snapshot. Retain is recommended."
	@image=$$($(KUBECTL) -n $(KUBE_NS) get statefulset gofer -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null); \
		[ -n "$$image" ] || { echo "ERROR: gofer is not deployed here; deploy it first."; exit 1; }; \
		$(KUBECTL) kustomize deploy/k8s/backup | sed \
			-e "s|image: localhost/gofer:dev$$|image: $$image|" \
			-e 's|@SNAPSHOT_CLASS@|$(SNAPSHOT_CLASS)|' \
			-e 's|@SNAPSHOT_KEEP@|$(SNAPSHOT_KEEP)|' \
			-e 's|@SNAPSHOT_SCHEDULE@|$(SNAPSHOT_SCHEDULE)|' | $(KUBECTL) apply -f -
	@echo "scheduled snapshots of $(DATA_CLAIM): '$(SNAPSHOT_SCHEDULE)' (UTC), keeping $(SNAPSHOT_KEEP). Take one now with 'make snapshot'."

# Stop taking snapshots. Existing snapshots, and the storage behind them, stay.
backup-down: deploy-target
	$(KUBECTL) kustomize deploy/k8s/backup | $(KUBECTL) delete --ignore-not-found -f -

# Take a snapshot now (through the CronJob, so it is labeled, waited for and pruned
# like a scheduled one) and wait for the result.
snapshot: deploy-target
	@$(KUBECTL) -n $(KUBE_NS) get cronjob gofer-snapshot >/dev/null 2>&1 || { \
		echo "ERROR: scheduled snapshots are not set up here; run 'make backup-apply' first."; exit 1; }
	@job=gofer-snapshot-manual-$$(date -u +%Y%m%d%H%M%S); \
		$(KUBECTL) -n $(KUBE_NS) create job "$$job" --from=cronjob/gofer-snapshot >/dev/null; \
		echo "job $$job started; waiting up to $(SNAPSHOT_TIMEOUT)s"; \
		end=$$(( $$(date +%s) + $(SNAPSHOT_TIMEOUT) )); state=; \
		while [ $$(date +%s) -lt $$end ]; do \
			state=$$($(KUBECTL) -n $(KUBE_NS) get job "$$job" -o jsonpath='{range .status.conditions[?(@.status=="True")]}{.type}{" "}{end}'); \
			case "$$state" in *Complete*|*Failed*) break;; esac; sleep 5; \
		done; \
		$(KUBECTL) -n $(KUBE_NS) logs "job/$$job" --all-containers 2>/dev/null | tail -20; \
		case "$$state" in *Complete*) echo "snapshot taken.";; \
			*Failed*) echo "ERROR: the snapshot job failed (logs above)."; exit 1;; \
			*) echo "ERROR: the snapshot job did not finish in $(SNAPSHOT_TIMEOUT)s; check 'kubectl -n $(KUBE_NS) get job $$job'."; exit 1;; esac

# List the data volume's snapshots, oldest first.
snapshots: deploy-target
	@$(KUBECTL) -n $(KUBE_NS) get volumesnapshot --sort-by=.metadata.creationTimestamp \
		-o custom-columns='NAME:.metadata.name,READY:.status.readyToUse,SIZE:.status.restoreSize,CREATED:.metadata.creationTimestamp,SOURCE:.spec.source.persistentVolumeClaimName'

# The replacement claim: the current claim's name, labels, class and access, with the
# snapshot as its data source and its size (one line: make would hand the shell a
# backslash-newline inside the quoted jq program).
RESTORE_CLAIM_JQ = {apiVersion: "v1", kind: "PersistentVolumeClaim", metadata: {name: .metadata.name, namespace: .metadata.namespace, labels: (.metadata.labels // {})}, spec: {accessModes: .spec.accessModes, storageClassName: .spec.storageClassName, volumeMode: .spec.volumeMode, resources: {requests: {storage: (if $$size != "" then $$size else .spec.resources.requests.storage end)}}, dataSource: {apiGroup: "snapshot.storage.k8s.io", kind: "VolumeSnapshot", name: $$snap}}}

# Replace the data volume with one restored from a snapshot. Everything written after
# the snapshot is rolled back. It stops gofer for the swap, and keeps the current
# volume (reclaim policy Retain) so you can go back. CONFIRM must repeat the snapshot
# name; without it, the target prints the plan and changes nothing.
restore: deploy-target
	@[ -n "$(SNAPSHOT)" ] || { echo "usage: make restore SNAPSHOT=<name> CONFIRM=<name>  (names: make snapshots)"; exit 1; }
	@ready=$$($(KUBECTL) -n $(KUBE_NS) get volumesnapshot "$(SNAPSHOT)" -o jsonpath='{.status.readyToUse}' 2>/dev/null); \
		[ "$$ready" = true ] || { echo "ERROR: snapshot $(SNAPSHOT) not found in namespace $(KUBE_NS), or not ready to use."; exit 1; }
	@$(KUBECTL) -n $(KUBE_NS) get pvc $(DATA_CLAIM) >/dev/null 2>&1 || { \
		echo "ERROR: there is no $(DATA_CLAIM) to replace. For a new cluster or namespace, see docs/backup-restore.md."; exit 1; }
	@pv=$$($(KUBECTL) -n $(KUBE_NS) get pvc $(DATA_CLAIM) -o jsonpath='{.spec.volumeName}'); \
		taken=$$($(KUBECTL) -n $(KUBE_NS) get volumesnapshot "$(SNAPSHOT)" -o jsonpath='{.status.creationTime}'); \
		echo "This replaces $(DATA_CLAIM) (volume $$pv) with a new volume restored from $(SNAPSHOT), taken $$taken."; \
		echo "gofer stops for the swap. Everything written after the snapshot is rolled back."; \
		echo "The current volume $$pv is kept, so you can go back (the steps are printed at the end)."
	@[ "$(CONFIRM)" = "$(SNAPSHOT)" ] || { echo; echo "Nothing changed. To proceed, repeat the snapshot name: CONFIRM=$(SNAPSHOT)"; exit 1; }
	@set -e; \
		pv=$$($(KUBECTL) -n $(KUBE_NS) get pvc $(DATA_CLAIM) -o jsonpath='{.spec.volumeName}'); \
		size=$$($(KUBECTL) -n $(KUBE_NS) get volumesnapshot "$(SNAPSHOT)" -o jsonpath='{.status.restoreSize}'); \
		claim=$$(mktemp); trap 'rm -f "$$claim"' EXIT; \
		$(KUBECTL) -n $(KUBE_NS) get pvc $(DATA_CLAIM) -o json | jq --arg snap "$(SNAPSHOT)" --arg size "$$size" '$(RESTORE_CLAIM_JQ)' > "$$claim"; \
		echo "1/5 keeping the current volume $$pv"; \
		$(KUBECTL) patch pv "$$pv" -p '{"spec":{"persistentVolumeReclaimPolicy":"Retain"}}' >/dev/null; \
		echo "2/5 stopping gofer"; \
		$(KUBECTL) -n $(KUBE_NS) scale statefulset/gofer --replicas=0 >/dev/null; \
		$(KUBECTL) -n $(KUBE_NS) wait --for=delete pod/gofer-0 --timeout=120s 2>/dev/null || true; \
		echo "3/5 replacing $(DATA_CLAIM)"; \
		$(KUBECTL) -n $(KUBE_NS) delete pvc $(DATA_CLAIM) --wait=true >/dev/null; \
		$(KUBECTL) apply -f "$$claim" >/dev/null || { \
			echo "ERROR: could not create $(DATA_CLAIM) from $(SNAPSHOT). gofer is stopped; volume $$pv is intact."; \
			echo "To go back to it, see 'Going back' in docs/backup-restore.md (volume $$pv)."; exit 1; }; \
		echo "4/5 starting gofer on the restored volume"; \
		$(KUBECTL) -n $(KUBE_NS) scale statefulset/gofer --replicas=1 >/dev/null; \
		$(KUBECTL) -n $(KUBE_NS) rollout status statefulset/gofer --timeout=$(ROLLOUT_TIMEOUT); \
		new=$$($(KUBECTL) -n $(KUBE_NS) get pvc $(DATA_CLAIM) -o jsonpath='{.spec.volumeName}'); \
		echo "5/5 setting the restored volume $$new to reclaim policy $(DATA_RECLAIM_POLICY)"; \
		$(KUBECTL) patch pv "$$new" -p '{"spec":{"persistentVolumeReclaimPolicy":"$(DATA_RECLAIM_POLICY)"}}' >/dev/null; \
		$(KUBECTL) -n $(KUBE_NS) logs gofer-0 | grep -m1 'persistent storage' || true; \
		echo; echo "Restored $(DATA_CLAIM) from $(SNAPSHOT). Check the app, then:"; \
		echo "  keep it:  delete the old volume and its disk with"; \
		echo "            kubectl patch pv $$pv -p '{\"spec\":{\"persistentVolumeReclaimPolicy\":\"Delete\"}}'"; \
		echo "  undo it:  see 'Going back' in docs/backup-restore.md (old volume $$pv)."

# Tear down gofer's app components (leaves the cluster + AEI in place).
dev-down:
	-$(KUBECTL) delete -f deploy/k8s/overlays/dev/agentapp.yaml --ignore-not-found
	-$(KUBECTL) delete -k deploy/k8s/base
	-$(KUBECTL) delete namespace $(KUBE_NS)

# Port-forward the web tier to the host.
dev-forward:
	$(KUBECTL) -n $(KUBE_NS) port-forward svc/gofer $(WEB_LOCAL_PORT):8080

# Tail the web tier logs.
dev-logs:
	$(KUBECTL) -n $(KUBE_NS) logs -l app.kubernetes.io/name=gofer -f

# ---- optional monitoring stack (Prometheus + Grafana) ------------------------
# Scrapes gofer's own /metrics (app-level aggregates) and the aei-controller's
# /metrics (the aei_run_* per-run timing). Opt-in; not part of dev-up.
monitoring-load: kind-engine
	$(KIND_ENGINE) pull $(PROMETHEUS_IMAGE) && $(KIND_ENGINE) save $(PROMETHEUS_IMAGE) -o prom.tar && kind load image-archive prom.tar --name $(KIND_CLUSTER) && rm -f prom.tar
	$(KIND_ENGINE) pull $(GRAFANA_IMAGE) && $(KIND_ENGINE) save $(GRAFANA_IMAGE) -o graf.tar && kind load image-archive graf.tar --name $(KIND_CLUSTER) && rm -f graf.tar

dev-monitoring: cluster-up monitoring-load
	$(KUBECTL) apply -k deploy/k8s/monitoring
	$(KUBECTL) -n $(MONITORING_NS) rollout status deploy/prometheus --timeout=120s
	$(KUBECTL) -n $(MONITORING_NS) rollout status deploy/grafana --timeout=120s

dev-monitoring-down:
	-$(KUBECTL) delete -k deploy/k8s/monitoring

dev-forward-grafana:
	$(KUBECTL) -n $(MONITORING_NS) port-forward svc/grafana $(GRAFANA_LOCAL_PORT):3000

dev-forward-prometheus:
	$(KUBECTL) -n $(MONITORING_NS) port-forward svc/prometheus $(PROM_LOCAL_PORT):9090
