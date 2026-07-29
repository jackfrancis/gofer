# gofer — dev loop
#
# gofer is a self-contained web app plus an agent-dispatch seam (see AGENTS.md). This
# dev loop builds and runs the WEB TIER only; a concrete agent-runtime backend brings
# its own build/image targets.

.PHONY: build test vet tidy run genkey clean \
        engine build-linux image kind-load \
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
# key (an ephemeral pair is generated here). With the default no-op dispatcher the
# UI/OAuth work but the worklist stays empty; select a backend to populate it.
run:
	@keys="$$(go run ./hack/genkey)"; export $$keys; \
		SESSION_SECRET="$${SESSION_SECRET:-gofer-dev-session-secret-change-me-please-0123456789}" \
		go run ./cmd/server

# Generate an Ed25519 run-credential keypair: MINT_PUBLIC_KEY for gofer's verify-only
# web tier, plus a MINT_PRIVATE_KEY a runtime's minter holds (the web tier never does).
genkey:
	@go run ./hack/genkey

clean:
	rm -rf bin build gofer-image.tar

# ---- container image + kind dev cluster --------------------------------------
#
# gofer ships only its web image + app manifests. The web tier runs standalone; a
# concrete agent-runtime backend adds its own image/install.

# Container engine autodetection: prefer podman (common on macOS), fall back to docker.
CONTAINER_ENGINE ?= $(shell command -v podman 2>/dev/null || command -v docker 2>/dev/null)
IMAGE ?= localhost/gofer:dev
KIND_CLUSTER ?= gofer
KUBE_NS ?= gofer
KUBE_CONTEXT ?= kind-$(KIND_CLUSTER)
KIND_CONFIG ?= deploy/kind/cluster.yaml
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
endif

engine:
	@test -n "$(CONTAINER_ENGINE)" || { echo "no container engine found (install podman or docker)"; exit 1; }
	@echo "using container engine: $(CONTAINER_ENGINE)"

# Cross-compile the web binary for the kind nodes (static, stripped).
build-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) go build -trimpath -ldflags="-s -w" -o bin/linux/server ./cmd/server

# Build the web-tier image (packages the prebuilt bin/linux/server).
image: engine build-linux
	$(CONTAINER_ENGINE) build -f Dockerfile -t $(IMAGE) .

# Load the web image into kind via an engine-agnostic archive (podman + docker).
kind-load: image
	$(CONTAINER_ENGINE) save $(IMAGE) -o gofer-image.tar
	kind load image-archive gofer-image.tar --name $(KIND_CLUSTER)
	rm -f gofer-image.tar

# Create the kind cluster if it does not already exist.
cluster-up:
	@command -v kind >/dev/null || { echo "kind not installed (https://kind.sigs.k8s.io)"; exit 1; }
	@kind get clusters 2>/dev/null | grep -qx $(KIND_CLUSTER) || kind create cluster --name $(KIND_CLUSTER) --config $(KIND_CONFIG)

# Delete the kind cluster.
cluster-down:
	-kind delete cluster --name $(KIND_CLUSTER)

# Deploy the web app to kind: ensure the cluster + image, the run-credential verify
# key, the namespace + secret, apply the base manifests, and wait for rollout. The web
# tier runs standalone; with the default no-op dispatcher the UI works but the worklist
# stays empty until a backend is selected.
#
# SESSION_SECRET is generated; MINT_PUBLIC_KEY comes from a freshly generated keypair
# (its private half is for a runtime's minter, unused by the web tier); GITHUB_CLIENT_ID
# / GITHUB_CLIENT_SECRET and AI_TOKEN are seeded from your shell env when set.
# The GitHub OAuth callback for the port-forward is http://localhost:$(WEB_LOCAL_PORT)/auth/github/callback.
dev-up: cluster-up kind-load
	@keys="$$(go run ./hack/genkey)"; mpk="$$(printf '%s\n' "$$keys" | sed -n 's/^MINT_PUBLIC_KEY=//p')"; \
		kubectl --context $(KUBE_CONTEXT) create namespace $(KUBE_NS) --dry-run=client -o yaml | kubectl --context $(KUBE_CONTEXT) apply -f -; \
		kubectl --context $(KUBE_CONTEXT) -n $(KUBE_NS) get secret gofer-secrets >/dev/null 2>&1 || \
			kubectl --context $(KUBE_CONTEXT) -n $(KUBE_NS) create secret generic gofer-secrets \
				--from-literal=SESSION_SECRET="$$(openssl rand -base64 48)" \
				--from-literal=MINT_PUBLIC_KEY="$$mpk" \
				--from-literal=GITHUB_CLIENT_ID="$${GITHUB_CLIENT_ID:-}" \
				--from-literal=GITHUB_CLIENT_SECRET="$${GITHUB_CLIENT_SECRET:-}" \
				--from-literal=AI_TOKEN="$${AI_TOKEN:-}"
	@[ -z "$$GITHUB_CLIENT_ID" ] || kubectl --context $(KUBE_CONTEXT) -n $(KUBE_NS) patch secret gofer-secrets --type=merge -p "{\"stringData\":{\"GITHUB_CLIENT_ID\":\"$$GITHUB_CLIENT_ID\"}}"
	@[ -z "$$GITHUB_CLIENT_SECRET" ] || kubectl --context $(KUBE_CONTEXT) -n $(KUBE_NS) patch secret gofer-secrets --type=merge -p "{\"stringData\":{\"GITHUB_CLIENT_SECRET\":\"$$GITHUB_CLIENT_SECRET\"}}"
	@[ -z "$$AI_TOKEN" ] || kubectl --context $(KUBE_CONTEXT) -n $(KUBE_NS) patch secret gofer-secrets --type=merge -p "{\"stringData\":{\"AI_TOKEN\":\"$$AI_TOKEN\"}}"
	kubectl --context $(KUBE_CONTEXT) apply -k deploy/k8s/base
	@[ -z "$$AI_CONNECTIONS" ] || kubectl --context $(KUBE_CONTEXT) -n $(KUBE_NS) patch configmap gofer-config --type=merge -p "$$(jq -nc --arg v "$$AI_CONNECTIONS" '{data:{AI_CONNECTIONS:$$v}}')"
	kubectl --context $(KUBE_CONTEXT) rollout restart deploy/gofer -n $(KUBE_NS)
	kubectl --context $(KUBE_CONTEXT) rollout status deploy/gofer -n $(KUBE_NS) --timeout=120s

# Tear down the app (leaves the cluster).
dev-down:
	-kubectl --context $(KUBE_CONTEXT) delete -k deploy/k8s/base
	-kubectl --context $(KUBE_CONTEXT) delete namespace $(KUBE_NS)

# Port-forward the web tier to the host.
dev-forward:
	kubectl --context $(KUBE_CONTEXT) -n $(KUBE_NS) port-forward svc/gofer $(WEB_LOCAL_PORT):8080

# Tail the web tier logs.
dev-logs:
	kubectl --context $(KUBE_CONTEXT) -n $(KUBE_NS) logs -l app.kubernetes.io/name=gofer -f

# ---- optional monitoring stack (Prometheus + Grafana) ------------------------
# Scrapes gofer's own /metrics (app-level aggregates). Opt-in; not part of dev-up.
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
