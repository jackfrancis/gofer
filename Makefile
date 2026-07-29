# gofer — dev loop
#
# The selected agent-runtime backend is AEI (internal/runtime/aei), so gofer is an AEI
# app: it consumes AEI's app SDK (sdks/go/aeiapp) and runtime SDK (aeiruntime) as a
# local sibling checkout via replace directives in go.mod — both stdlib-only. It
# imports NO provider and NO client-go: dispatch is an HTTP call to the pre-installed
# aei-controller, and the runtime is gofer's own image, launched per run.

.PHONY: build test vet tidy run runtime genkey clean \
        engine build-linux image image-runtime kind-load kind-load-runtime push-runtime \
        cluster-up cluster-down dev-up dev-down dev-forward dev-logs \
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

# Container engine autodetection: prefer podman (common on macOS), fall back to docker.
CONTAINER_ENGINE ?= $(shell command -v podman 2>/dev/null || command -v docker 2>/dev/null)
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
KUBE_CONTEXT ?= kind-$(KIND_CLUSTER)
KIND_CONFIG ?= deploy/kind/cluster.yaml
# The AgentProviderClass gofer's runs dispatch to — a well-known, admin-published name
# (e.g. "isolated", "durable", "k8s-job"). gofer is substrate-agnostic: it names a
# capability, not a platform. Empty selects the cluster default. `make dev-up` patches
# the AgentApp to this value, so the class must already exist on the AEI side.
AGENT_PROVIDER_CLASS ?= gofer
WEB_LOCAL_PORT ?= 8080
# Optional monitoring stack (opt-in; see `make dev-monitoring`).
MONITORING_NS ?= gofer-monitoring
PROMETHEUS_IMAGE ?= docker.io/prom/prometheus:v3.1.0
GRAFANA_IMAGE ?= docker.io/grafana/grafana:11.4.0
GRAFANA_LOCAL_PORT ?= 3000
PROM_LOCAL_PORT ?= 9090
# The kind nodes are linux; the arch matches the container engine (arm64 on Apple Silicon).
GOARCH ?= $(shell go env GOARCH)

ifeq ($(findstring podman,$(CONTAINER_ENGINE)),podman)
export KIND_EXPERIMENTAL_PROVIDER = podman
# podman needs --tls-verify=false to push to the local HTTP registry; docker treats
# localhost registries as insecure by default and needs no flag.
RUNTIME_PUSH_FLAGS = --tls-verify=false
endif

engine:
	@test -n "$(CONTAINER_ENGINE)" || { echo "no container engine found (install podman or docker)"; exit 1; }
	@echo "using container engine: $(CONTAINER_ENGINE)"

# Cross-compile the web + runtime binaries for the kind nodes (static, stripped).
build-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) go build -trimpath -ldflags="-s -w" -o bin/linux/server ./cmd/server
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) go build -trimpath -ldflags="-s -w" -o bin/linux/runtime ./cmd/runtime

# Build the web-tier image (packages the prebuilt bin/linux/server).
image: engine build-linux
	$(CONTAINER_ENGINE) build -f Dockerfile -t $(IMAGE) .

# Build the agent-runtime image (packages the prebuilt bin/linux/runtime).
image-runtime: engine build-linux
	$(CONTAINER_ENGINE) build -f Dockerfile.runtime -t $(RUNTIME_IMAGE) .

# Load the web image into kind via an engine-agnostic archive (podman + docker).
kind-load: image
	$(CONTAINER_ENGINE) save $(IMAGE) -o gofer-image.tar
	kind load image-archive gofer-image.tar --name $(KIND_CLUSTER)
	rm -f gofer-image.tar

# Load the runtime image into kind so a per-run launcher resolves it without a
# registry pull.
kind-load-runtime: image-runtime
	$(CONTAINER_ENGINE) save $(RUNTIME_IMAGE) -o gofer-runtime-image.tar
	kind load image-archive gofer-runtime-image.tar --name $(KIND_CLUSTER)
	rm -f gofer-runtime-image.tar

# Publish the runtime image to the substrate registry (durable path). The DURABLE
# substrate pulls actor images from a registry by digest — a tag invalidates ate's
# golden snapshot, and kind-loaded images are not registry-pullable — so `make dev-up`
# with AGENT_PROVIDER_CLASS=durable pushes here and pins the AgentApp image to the
# pushed digest automatically. This target is the manual build+push half.
push-runtime: image-runtime  ## build + push the runtime image to SUBSTRATE_REGISTRY (for durable)
	$(CONTAINER_ENGINE) tag $(RUNTIME_IMAGE) $(RUNTIME_REGISTRY_IMAGE)
	$(CONTAINER_ENGINE) push $(RUNTIME_PUSH_FLAGS) $(RUNTIME_REGISTRY_IMAGE)

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
	@nodes=$$(kubectl --context $(KUBE_CONTEXT) get nodes --no-headers 2>/dev/null | wc -l | tr -d ' '); \
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

# Install gofer's app components onto the cluster: require AEI, ensure the kind
# cluster + images, the run-credential verify key, the namespace, and the secret, apply
# the web app + the AgentApp, then wait for rollout.
#
# With AEI selected as the agent-runtime backend, gofer CANNOT run standalone: dev-up
# fails fast if the AEI CRDs are absent. AEI (its controller, CRDs, and an
# AgentProviderClass) is installed separately on the AEI side, including its
# run-credential signing authority (AEI owns the keypair as sole minter; gofer's web
# tier verifies with AEI's public key, MINT_PUBLIC_KEY — it holds no minting key).
#
# SESSION_SECRET is generated; MINT_PUBLIC_KEY is AEI's published verify key (read from
# the cluster's aei-signing secret, or passed explicitly); GITHUB_CLIENT_ID /
# GITHUB_CLIENT_SECRET and AI_TOKEN (the chat-model token gofer brokers to runtimes;
# unset = stub ranker, no LLM) are seeded from your shell env when set. AI_CONNECTIONS
# defaults from the configmap but can likewise be overridden from the shell.
# The GitHub OAuth callback for the port-forward is http://localhost:$(WEB_LOCAL_PORT)/auth/github/callback.
dev-up: cluster-up kind-load kind-load-runtime
	@kubectl --context $(KUBE_CONTEXT) get crd agentapps.agents.x-k8s.io >/dev/null 2>&1 || { \
		echo "ERROR: AEI is not installed (agents.x-k8s.io CRDs missing). This build dispatches to"; \
		echo "       the aei-controller and cannot run without it. Install AEI, then re-run 'make dev-up'."; \
		exit 1; }
	@kubectl --context $(KUBE_CONTEXT) create namespace $(KUBE_NS) --dry-run=client -o yaml | kubectl --context $(KUBE_CONTEXT) apply -f -
	@# AEI owns the run-credential keypair and is the sole minter; gofer's web tier only
	@# VERIFIES, with AEI's PUBLIC key. Provide it via MINT_PUBLIC_KEY, or let dev-up read
	@# AEI's published key from the cluster (both share one kind cluster in dev). gofer
	@# feeds AEI nothing secret.
	@mpk="$${MINT_PUBLIC_KEY:-$$(kubectl --context $(KUBE_CONTEXT) -n aei-system get secret aei-signing -o jsonpath='{.data.AEI_ED25519_PUBLIC_KEY}' 2>/dev/null | base64 -d)}"; \
		test -n "$$mpk" || { \
			echo "ERROR: no AEI run-credential verify key found. On the AEI side run 'make authority'"; \
			echo "       (provisions the platform keypair), then 'make public-key', and re-run this with"; \
			echo "       MINT_PUBLIC_KEY=<that key>."; \
			exit 1; }; \
		kubectl --context $(KUBE_CONTEXT) -n $(KUBE_NS) get secret gofer-secrets >/dev/null 2>&1 || \
			kubectl --context $(KUBE_CONTEXT) -n $(KUBE_NS) create secret generic gofer-secrets \
				--from-literal=SESSION_SECRET="$$(openssl rand -base64 48)" \
				--from-literal=MINT_PUBLIC_KEY="$$mpk" \
				--from-literal=GITHUB_CLIENT_ID="$${GITHUB_CLIENT_ID:-}" \
				--from-literal=GITHUB_CLIENT_SECRET="$${GITHUB_CLIENT_SECRET:-}" \
				--from-literal=AI_TOKEN="$${AI_TOKEN:-}"; \
		kubectl --context $(KUBE_CONTEXT) -n $(KUBE_NS) patch secret gofer-secrets --type=merge \
			-p "{\"stringData\":{\"MINT_PUBLIC_KEY\":\"$$mpk\"}}"
	@[ -z "$$GITHUB_CLIENT_ID" ] || kubectl --context $(KUBE_CONTEXT) -n $(KUBE_NS) patch secret gofer-secrets --type=merge -p "{\"stringData\":{\"GITHUB_CLIENT_ID\":\"$$GITHUB_CLIENT_ID\"}}"
	@[ -z "$$GITHUB_CLIENT_SECRET" ] || kubectl --context $(KUBE_CONTEXT) -n $(KUBE_NS) patch secret gofer-secrets --type=merge -p "{\"stringData\":{\"GITHUB_CLIENT_SECRET\":\"$$GITHUB_CLIENT_SECRET\"}}"
	@[ -z "$$AI_TOKEN" ] || kubectl --context $(KUBE_CONTEXT) -n $(KUBE_NS) patch secret gofer-secrets --type=merge -p "{\"stringData\":{\"AI_TOKEN\":\"$$AI_TOKEN\"}}"
	kubectl --context $(KUBE_CONTEXT) apply -k deploy/k8s/base
	@[ -z "$$AI_CONNECTIONS" ] || kubectl --context $(KUBE_CONTEXT) -n $(KUBE_NS) patch configmap gofer-config --type=merge -p "$$(jq -nc --arg v "$$AI_CONNECTIONS" '{data:{AI_CONNECTIONS:$$v}}')"
	kubectl --context $(KUBE_CONTEXT) apply -f deploy/k8s/overlays/dev/agentapp.yaml
	@# Point the AgentApp at the well-known class name the admin published on the AEI side
	@# (AGENT_PROVIDER_CLASS). gofer references a capability by name, not a substrate — an
	@# empty value would select the cluster-default class.
	kubectl --context $(KUBE_CONTEXT) -n $(KUBE_NS) patch agentapp gofer --type=merge \
		-p '{"spec":{"providerClassName":"$(AGENT_PROVIDER_CLASS)"}}'
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
		$(CONTAINER_ENGINE) tag $(RUNTIME_IMAGE) $(RUNTIME_REGISTRY_IMAGE); \
		$(CONTAINER_ENGINE) push $(RUNTIME_PUSH_FLAGS) $(RUNTIME_REGISTRY_IMAGE); \
		digest=$$(curl -sSf -D - -o /dev/null \
			-H 'Accept: application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json, application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json' \
			http://$(SUBSTRATE_REGISTRY)/v2/gofer-runtime/manifests/dev | \
			awk 'tolower($$1) ~ /docker-content-digest/ {print $$2}' | tr -d '\r' | tail -1); \
		test -n "$$digest" || { echo "ERROR: could not read the pushed image digest from $(SUBSTRATE_REGISTRY)"; exit 1; }; \
		echo "durable: pinning AgentApp image to $(SUBSTRATE_REGISTRY)/gofer-runtime@$$digest"; \
		kubectl --context $(KUBE_CONTEXT) -n $(KUBE_NS) patch agentapp gofer --type=merge \
			-p "{\"spec\":{\"image\":\"$(SUBSTRATE_REGISTRY)/gofer-runtime@$$digest\"}}"; \
	fi
	kubectl --context $(KUBE_CONTEXT) rollout restart deploy/gofer -n $(KUBE_NS)
	kubectl --context $(KUBE_CONTEXT) rollout status deploy/gofer -n $(KUBE_NS) --timeout=120s

# Tear down gofer's app components (leaves the cluster + AEI in place).
dev-down:
	-kubectl --context $(KUBE_CONTEXT) delete -f deploy/k8s/overlays/dev/agentapp.yaml --ignore-not-found
	-kubectl --context $(KUBE_CONTEXT) delete -k deploy/k8s/base
	-kubectl --context $(KUBE_CONTEXT) delete namespace $(KUBE_NS)

# Port-forward the web tier to the host.
dev-forward:
	kubectl --context $(KUBE_CONTEXT) -n $(KUBE_NS) port-forward svc/gofer $(WEB_LOCAL_PORT):8080

# Tail the web tier logs.
dev-logs:
	kubectl --context $(KUBE_CONTEXT) -n $(KUBE_NS) logs -l app.kubernetes.io/name=gofer -f

# ---- optional monitoring stack (Prometheus + Grafana) ------------------------
# Scrapes gofer's own /metrics (app-level aggregates) and the aei-controller's
# /metrics (the aei_run_* per-run timing). Opt-in; not part of dev-up.
monitoring-load: engine
	$(CONTAINER_ENGINE) pull $(PROMETHEUS_IMAGE) && $(CONTAINER_ENGINE) save $(PROMETHEUS_IMAGE) -o prom.tar && kind load image-archive prom.tar --name $(KIND_CLUSTER) && rm -f prom.tar
	$(CONTAINER_ENGINE) pull $(GRAFANA_IMAGE) && $(CONTAINER_ENGINE) save $(GRAFANA_IMAGE) -o graf.tar && kind load image-archive graf.tar --name $(KIND_CLUSTER) && rm -f graf.tar

dev-monitoring: cluster-up monitoring-load
	kubectl --context $(KUBE_CONTEXT) apply -k deploy/k8s/monitoring
	kubectl --context $(KUBE_CONTEXT) -n $(MONITORING_NS) rollout status deploy/prometheus --timeout=120s
	kubectl --context $(KUBE_CONTEXT) -n $(MONITORING_NS) rollout status deploy/grafana --timeout=120s

dev-monitoring-down:
	-kubectl --context $(KUBE_CONTEXT) delete -k deploy/k8s/monitoring

dev-forward-grafana:
	kubectl --context $(KUBE_CONTEXT) -n $(MONITORING_NS) port-forward svc/grafana $(GRAFANA_LOCAL_PORT):3000

dev-forward-prometheus:
	kubectl --context $(KUBE_CONTEXT) -n $(MONITORING_NS) port-forward svc/prometheus $(PROM_LOCAL_PORT):9090
