# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0

GO       ?= go
VERSION  ?= dev
LDFLAGS  := -X main.version=$(VERSION)

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-16s %s\n", $$1, $$2}'

.PHONY: build
build: ## Build nbsandboxd into bin/
	$(GO) build -ldflags "$(LDFLAGS)" -o bin/nbsandboxd ./cmd/nbsandboxd

.PHONY: test
# A throwaway PostgreSQL for unit tests (data in tmpfs, gone when stopped).
# Each test gets its own schema (internal/store/storetest).
TEST_DB_NAME := nbsandbox-test-db
TEST_DB_URL  := postgres://nbsandbox:nbsandbox@127.0.0.1:55432/nbsandbox_test?sslmode=disable

.PHONY: test-db
test-db: ## Start the unit tests' PostgreSQL on 127.0.0.1:55432 (Docker needed)
	@if [ -z "$$(docker ps -q -f name=^$(TEST_DB_NAME)$$)" ]; then \
		docker container rm --force $(TEST_DB_NAME) >/dev/null 2>&1 || true; \
		docker run -d --name $(TEST_DB_NAME) -p 127.0.0.1:55432:5432 --tmpfs /var/lib/postgresql/data \
			-e POSTGRES_USER=nbsandbox -e POSTGRES_PASSWORD=nbsandbox -e POSTGRES_DB=nbsandbox_test \
			postgres:17-alpine@sha256:3540c0362c809df084ffac0ae69fe794515f9de92692f02178bcdce0ef0302ac -c max_connections=300 -c fsync=off >/dev/null; \
	fi
	@for i in $$(seq 1 60); do docker exec $(TEST_DB_NAME) pg_isready -q -U nbsandbox -d nbsandbox_test && exit 0; sleep 0.5; done; \
		echo "test database did not become ready"; exit 1

# The object storage adapters' shared test suite against real stores:
# RustFS (S3) and Azurite (Azure Blob Storage's emulator; its account key is
# Microsoft's published development key). Both throwaway, loopback only.
AZURITE_KEY := Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==

.PHONY: test-blob
test-blob: ## Object storage adapters against RustFS and Azurite (Docker needed)
	@docker container rm --force nbsandbox-test-s3 nbsandbox-test-azurite >/dev/null 2>&1 || true
	@docker run -d --name nbsandbox-test-s3 -p 127.0.0.1:59000:9000 \
		-e RUSTFS_ACCESS_KEY=nbsandbox -e RUSTFS_SECRET_KEY=nbsandbox-test rustfs/rustfs:1.0.4@sha256:ed9210d6411d77ffceb70d94b964a3cb7d225983368bc25bebe01e995293dcc2 >/dev/null
	@docker run -d --name nbsandbox-test-azurite -p 127.0.0.1:59001:10000 \
		mcr.microsoft.com/azure-storage/azurite:3.35.0 azurite-blob --blobHost 0.0.0.0 --skipApiVersionCheck --loose >/dev/null
	@sleep 3
	NBSANDBOX_TEST_S3_ENDPOINT=127.0.0.1:59000 NBSANDBOX_TEST_S3_ACCESS_KEY=nbsandbox NBSANDBOX_TEST_S3_SECRET_KEY=nbsandbox-test \
	NBSANDBOX_TEST_AZURE_CONNECTION_STRING="DefaultEndpointsProtocol=http;AccountName=devstoreaccount1;AccountKey=$(AZURITE_KEY);BlobEndpoint=http://127.0.0.1:59001/devstoreaccount1;" \
		$(GO) test -count=1 -v -run 'PassesSuite' ./internal/blob/... ; status=$$?; \
		docker container rm --force nbsandbox-test-s3 nbsandbox-test-azurite >/dev/null 2>&1; exit $$status

# Object storage for ui-preview and run-native: a RustFS on 127.0.0.1:59010
# that keeps running (its data goes with the container).
DEV_S3_NAME := nbsandbox-dev-s3
DEV_S3_ENV  := NBSANDBOX_STORAGE=s3 NBSANDBOX_S3_ENDPOINT=127.0.0.1:59010 NBSANDBOX_S3_BUCKET=nbsandbox \
	NBSANDBOX_S3_ACCESS_KEY=nbsandbox NBSANDBOX_S3_SECRET_KEY=nbsandbox-dev-secret NBSANDBOX_S3_CREATE_BUCKET=true

.PHONY: dev-s3
dev-s3: ## Start a local RustFS (object storage) on 127.0.0.1:59010 for ui-preview and run-native
	@if [ -z "$$(docker ps -q -f name=^$(DEV_S3_NAME)$$)" ]; then \
		docker container rm --force $(DEV_S3_NAME) >/dev/null 2>&1 || true; \
		docker run -d --name $(DEV_S3_NAME) -p 127.0.0.1:59010:9000 \
			-e RUSTFS_ACCESS_KEY=nbsandbox -e RUSTFS_SECRET_KEY=nbsandbox-dev-secret rustfs/rustfs:1.0.4@sha256:ed9210d6411d77ffceb70d94b964a3cb7d225983368bc25bebe01e995293dcc2 >/dev/null; \
	fi
	@for i in $$(seq 1 60); do curl -s -o /dev/null http://127.0.0.1:59010/ && exit 0; sleep 0.5; done; \
		echo "object storage did not become ready"; exit 1

.PHONY: test-db-stop
test-db-stop: ## Stop the unit tests' PostgreSQL (its data is discarded)
	@docker container rm --force $(TEST_DB_NAME) >/dev/null 2>&1 || true

test: test-db ## Run unit tests (starts the test PostgreSQL)
	$(GO) test ./...

.PHONY: test-race
test-race: test-db ## Run unit tests with the race detector and coverage
	$(GO) test -race -coverprofile=coverage.out ./...

.PHONY: cover
cover: test-race ## Race-tested unit tests plus the coverage gate
	@scripts/coverage-gate.sh coverage.out

FUZZTIME ?= 30s

.PHONY: fuzz
fuzz: ## Run every fuzz target for FUZZTIME each (default 30s; nightly CI uses 5m)
	$(GO) test ./internal/spaces/ -run '^$$' -fuzz FuzzValidatePath -fuzztime $(FUZZTIME)
	$(GO) test ./internal/runs/ -run '^$$' -fuzz FuzzNormalizeParameters -fuzztime $(FUZZTIME)
	$(GO) test ./internal/deps/ -run '^$$' -fuzz FuzzScanners -fuzztime $(FUZZTIME)
	$(GO) test ./internal/isolation/docker/ -run '^$$' -fuzz FuzzLineWriter -fuzztime $(FUZZTIME)
	$(GO) test ./internal/auth/ -run '^$$' -fuzz FuzzVerify -fuzztime $(FUZZTIME)

.PHONY: fmt
fmt: ## Format Go code
	$(GO) fmt ./...

.PHONY: vet
vet: ## Run go vet
	$(GO) vet ./...

.PHONY: lint
lint: ## Run golangci-lint (installed separately, see CONTRIBUTING.md)
	@command -v golangci-lint >/dev/null 2>&1 || { echo "golangci-lint not found: see CONTRIBUTING.md > Tools"; exit 1; }
	golangci-lint run ./...

.PHONY: vuln
vuln: ## Check dependencies for known vulnerabilities (govulncheck, installed separately)
	@command -v govulncheck >/dev/null 2>&1 || { echo "govulncheck not found: see CONTRIBUTING.md > Tools"; exit 1; }
	govulncheck ./...

.PHONY: headers
headers: ## Check licence headers on source files
	@scripts/check-license-headers.sh .

.PHONY: check
check: headers vet lint cover ## Everything a PR must pass locally

.PHONY: tools
tools: ## Show which developer tools are installed
	@for t in go golangci-lint govulncheck docker; do \
		if command -v $$t >/dev/null 2>&1; then printf "  %-14s ok   (%s)\n" $$t "$$(command -v $$t)"; \
		else printf "  %-14s MISSING\n" $$t; fi; \
	done

RUNNER_IMAGE ?= nbsandbox-runner:dev
NODE_IMAGE   ?= node:22.11.0-alpine3.20
PY_BASE      := python:3.13-slim@sha256:09a17cad02668ae61ece136b4fd36ac3849e14f670ebedb637ea3e15580525bc

.PHONY: runner-image
runner-image: ## Build the runner image with its manifest label (Docker needed)
	docker build -q -t $(RUNNER_IMAGE)-unlabelled runner-image >/dev/null
	@mkdir -p bin
	docker run --rm --network none --entrypoint python $(RUNNER_IMAGE)-unlabelled /opt/nbsandbox/export_manifest.py > bin/runner-manifest.json
	@# Second build is fully cached; it only adds the manifest label.
	docker build -q --label "io.nbsandbox.manifest=$$(cat bin/runner-manifest.json)" -t $(RUNNER_IMAGE) runner-image >/dev/null
	@echo "built $(RUNNER_IMAGE) ($$(wc -c < bin/runner-manifest.json) byte manifest)"

.PHONY: runner-test
runner-test: ## Runner image tests and wrapper unit tests (Docker needed)
	@runner-image/tests/image_test.sh $(RUNNER_IMAGE)

.PHONY: runner-lock
runner-lock: ## Re-lock runner-image/requirements.txt with hashes (Docker needed)
	docker run --rm -v "$(CURDIR)/runner-image":/work -w /work $(PY_BASE) sh -c \
		'pip install -q --root-user-action=ignore pip-tools && pip-compile -q --generate-hashes --allow-unsafe --strip-extras --no-header --output-file requirements.txt requirements.in'

.PHONY: workloads
workloads: ## Regenerate test workload notebooks (runner image needed)
	docker run --rm --user "$$(id -u):$$(id -g)" -v "$(CURDIR)/tests/workloads":/w -w /w --entrypoint python $(RUNNER_IMAGE) make_notebooks.py

# Local data directory (Docker Desktop shares /Users by default).
NBSANDBOX_DATA_DIR ?= $(HOME)/.local/share/nbsandbox
DESKTOP_SOCK     := unix://$(HOME)/.docker/run/docker.sock
COMPOSE          := NBSANDBOX_DATA_DIR=$(NBSANDBOX_DATA_DIR) docker compose -f deploy/docker-compose.dev.yml
export NBSANDBOX_DATA_DIR

.PHONY: dev-data
dev-data: ## Create the local data dir and install the demo users file
	@mkdir -p "$(NBSANDBOX_DATA_DIR)/spaces" && chmod 700 "$(NBSANDBOX_DATA_DIR)"
	@install -m 600 config/users.yaml "$(NBSANDBOX_DATA_DIR)/users.yaml"

.PHONY: up
up: dev-data ## Start the dev stack: API on 127.0.0.1:8080, web UI on 127.0.0.1:8090
	@$(STOP_KIND)
	-@$(CLUSTER) stop >/dev/null 2>&1
	$(COMPOSE) up -d --build

.PHONY: down
down: ## Stop the dev stack
	$(COMPOSE) down

# A small cluster (two API processes behind a load balancer, two workers)
# with its own data folder and volumes, on the same port as the dev stack.
CLUSTER_DATA_DIR ?= $(NBSANDBOX_DATA_DIR)-cluster
CLUSTER          := NBSANDBOX_DATA_DIR=$(CLUSTER_DATA_DIR) docker compose -f deploy/docker-compose.cluster.yml
CLUSTER_TEST_ENV := NBSANDBOX_CLUSTER=1 NBSANDBOX_COMPOSE=$(CURDIR)/deploy/docker-compose.cluster.yml \
	NBSANDBOX_DATA_DIR=$(CLUSTER_DATA_DIR) NBSANDBOX_SHUTDOWN_GRACE=5s NBSANDBOX_RUN_TIMEOUT=60s

.PHONY: cluster-data cluster-up cluster-logs cluster-down wipe-cluster test-cluster test-cluster-failures
cluster-data:
	@mkdir -p "$(CLUSTER_DATA_DIR)/spaces" && chmod 700 "$(CLUSTER_DATA_DIR)"
	@install -m 600 config/users.yaml "$(CLUSTER_DATA_DIR)/users.yaml"

cluster-up: cluster-data ## Start the cluster stack (stops the dev stack): API on 127.0.0.1:8080
	$(COMPOSE) stop
	@$(STOP_KIND)
	$(CLUSTER) up -d --build

cluster-logs: ## Follow the cluster stack's logs
	$(CLUSTER) logs -f --tail=100

cluster-down: ## Stop the cluster stack
	$(CLUSTER) down

wipe-cluster: ## Stop the cluster stack and delete its volumes (the data folder is left to you)
	$(CLUSTER) down --volumes

test-cluster: cluster-data ## Integration tests through the cluster stack (Docker needed; stops the dev stack)
	$(COMPOSE) stop
	@$(STOP_KIND)
	$(CLUSTER_TEST_ENV) $(CLUSTER) up -d --build --force-recreate
	$(CLUSTER_TEST_ENV) $(GO) test -tags integration -count=1 -timeout 40m $(TESTFLAGS) ./tests/integration/

test-cluster-failures: cluster-data ## Only the cluster failure tests: kill workers, API nodes, Postgres (stops the dev stack)
	$(COMPOSE) stop
	@$(STOP_KIND)
	$(CLUSTER_TEST_ENV) $(CLUSTER) up -d --build --force-recreate
	$(CLUSTER_TEST_ENV) $(GO) test -tags integration -count=1 -timeout 20m -run '^TestCluster' $(TESTFLAGS) ./tests/integration/

# A Kubernetes cluster in Docker (kind), with the chart installed: API on
# 127.0.0.1:8080, two API processes, two workers starting runs as Pods.
# kind and helm run through go run; kubectl is the one inside the kind node.
KIND        := $(GO) run sigs.k8s.io/kind@v0.27.0
HELM        := $(GO) run helm.sh/helm/v3/cmd/helm@v3.17.3
KIND_DIR    := deploy/kubernetes/kind
# The local cluster's network plugin: cilium (default) or calico, which both
# enforce NetworkPolicy, or kindnet (kind's own), which doesn't fully: with
# it the workers must refuse to run notebooks (make test-k8s-cni-refuses).
# Each plugin gets its own cluster; one runs at a time (they share :8080).
K8S_CNI     ?= cilium
# Run Pods' RuntimeClass: "" (the node's default, runc) or gvisor, which
# installs gVisor in the kind node first (scripts/kind-gvisor.sh).
K8S_RUNTIME_CLASS ?=
# The node's pids limit per Pod. With runc it is the run's process limit.
# With gVisor it also counts gVisor's own threads, so it is only a backstop
# (4 x 256 + 256); the run's limit is RLIMIT_NPROC inside the sandbox.
KIND_POD_PIDS := $(if $(filter gvisor,$(K8S_RUNTIME_CLASS)),1280,300)
KIND_NAME   := nbsandbox$(if $(filter cilium,$(K8S_CNI)),,-$(K8S_CNI))
KIND_NODE   := $(KIND_NAME)-control-plane
KIND_CONFIG := $(CURDIR)/bin/kind-kubeconfig-$(K8S_CNI)
KUBECTL     := docker exec -i $(KIND_NODE) kubectl --kubeconfig=/etc/kubernetes/admin.conf
CILIUM_VERSION := 1.17.2
CALICO_VERSION := 3.29.3
# Every local kind cluster publishes 127.0.0.1:8080 like the Compose stacks:
# those stop them (they keep their state; make k8s-up starts one again).
STOP_KIND := scripts/stop-kind.sh

K8S_TEST_ENV := NBSANDBOX_CLUSTER=1 NBSANDBOX_K8S=1 NBSANDBOX_KIND_NODE=$(KIND_NODE) NBSANDBOX_SHUTDOWN_GRACE=5s NBSANDBOX_RUN_TIMEOUT=150s

.PHONY: k8s-images k8s-cluster k8s-up k8s-logs k8s-down test-k8s test-k8s-conformance test-k8s-cni-refuses
k8s-images: runner-image
	docker build -q -f deploy/nbsandboxd.Dockerfile -t nbsandboxd:dev . >/dev/null

# The cluster and its network plugin, with the images, PostgreSQL and RustFS.
k8s-cluster: k8s-images
	-$(COMPOSE) stop
	-$(CLUSTER) stop
	@$(STOP_KIND)
	@mkdir -p bin
	-docker start $(KIND_NODE) >/dev/null 2>&1
	@sed 's/^name: nbsandbox$$/name: $(KIND_NAME)/; $(if $(filter kindnet,$(K8S_CNI)),s/disableDefaultCNI: true/disableDefaultCNI: false/,)' \
		$(KIND_DIR)/kind.yaml > bin/kind-$(K8S_CNI).yaml
	$(KIND) get clusters 2>/dev/null | grep -qx $(KIND_NAME) || $(KIND) create cluster --config bin/kind-$(K8S_CNI).yaml
	$(KIND) export kubeconfig --name $(KIND_NAME) --kubeconfig $(KIND_CONFIG)
	@# A node that was stopped needs a moment before its API server answers.
	@for i in $$(seq 90); do $(KUBECTL) get --raw /readyz >/dev/null 2>&1 && exit 0; sleep 2; done; echo "API server not ready" >&2; exit 1
ifeq ($(K8S_CNI),cilium)
	$(HELM) upgrade --install cilium cilium --repo https://helm.cilium.io --version $(CILIUM_VERSION) -n kube-system \
		--kubeconfig $(KIND_CONFIG) --set image.pullPolicy=IfNotPresent --set ipam.mode=kubernetes --wait --timeout 10m
else ifeq ($(K8S_CNI),calico)
	$(KUBECTL) apply --server-side -f https://raw.githubusercontent.com/projectcalico/calico/v$(CALICO_VERSION)/manifests/calico.yaml >/dev/null
	@# kind's nodes are containers: Felix must not insist on strict reverse-path filtering.
	$(KUBECTL) -n kube-system set env daemonset/calico-node FELIX_IGNORELOOSERPF=true
	$(KUBECTL) -n kube-system rollout status daemonset/calico-node --timeout=10m
else ifneq ($(K8S_CNI),kindnet)
	$(error K8S_CNI must be cilium, calico or kindnet)
endif
	$(KUBECTL) wait --for=condition=Ready nodes --all --timeout=300s
	$(KIND) load docker-image nbsandboxd:dev nbsandbox-runner:dev --name $(KIND_NAME)
	$(KUBECTL) apply -f - < $(KIND_DIR)/deps.yaml
	$(KUBECTL) -n nbsandbox delete secret nbsandbox-users --ignore-not-found
	$(KUBECTL) -n nbsandbox create secret generic nbsandbox-users --from-file=users.yaml=/dev/stdin < config/users.yaml
	$(KUBECTL) -n nbsandbox rollout status deploy/postgres deploy/rustfs --timeout=300s
	@docker exec $(KIND_NODE) sh -c 'grep -qx "podPidsLimit: $(KIND_POD_PIDS)" /var/lib/kubelet/config.yaml || \
		{ sed -i "s/^podPidsLimit: .*/podPidsLimit: $(KIND_POD_PIDS)/" /var/lib/kubelet/config.yaml && systemctl restart kubelet && echo "podPidsLimit $(KIND_POD_PIDS)"; }'
ifeq ($(K8S_RUNTIME_CLASS),gvisor)
	scripts/kind-gvisor.sh $(KIND_NODE)
else ifneq ($(K8S_RUNTIME_CLASS),)
	$(error K8S_RUNTIME_CLASS must be empty or gvisor)
endif

k8s-up: k8s-cluster ## Start nbsandbox on a local Kubernetes (kind, in Docker; K8S_CNI=cilium|calico, K8S_RUNTIME_CLASS=gvisor): API on 127.0.0.1:8080
	$(HELM) upgrade --install nbsandbox deploy/helm/nbsandbox -n nbsandbox -f $(KIND_DIR)/values.yaml \
		--set runs.runtimeClass=$(K8S_RUNTIME_CLASS) --kubeconfig $(KIND_CONFIG) --wait --timeout 5m
	$(KUBECTL) -n nbsandbox rollout restart deploy/nbsandbox-api deploy/nbsandbox-worker
	$(KUBECTL) -n nbsandbox rollout status deploy/nbsandbox-api deploy/nbsandbox-worker --timeout=180s

k8s-logs: ## Follow the local Kubernetes stack's logs
	$(KUBECTL) -n nbsandbox logs -f -l app.kubernetes.io/instance=nbsandbox --prefix --max-log-requests 10 --tail=100

k8s-down: ## Delete the local Kubernetes cluster (K8S_CNI picks which)
	$(KIND) delete cluster --name $(KIND_NAME)

test-k8s-conformance: k8s-up ## The backend conformance suite against the local Kubernetes cluster
	NBSANDBOX_TEST_KUBECONFIG=$(KIND_CONFIG) $(GO) test -count=1 -timeout 20m -run Conformance $(TESTFLAGS) ./internal/isolation/kubernetes/

test-k8s: k8s-up ## Integration tests through the local Kubernetes stack (K8S_CNI=cilium|calico)
	$(K8S_TEST_ENV) $(GO) test -tags integration -count=1 -timeout 40m $(TESTFLAGS) ./tests/integration/

# kind's own network plugin (kindnet) doesn't enforce the deny-all policy
# (run Pods reach the API server, kubelet, workers, internet): the workers must notice and refuse runs.
test-k8s-cni-refuses: ## On kindnet, workers find the network open and refuse runs
	$(MAKE) k8s-cluster K8S_CNI=kindnet
	$(HELM) upgrade --install nbsandbox deploy/helm/nbsandbox -n nbsandbox -f $(KIND_DIR)/values.yaml \
		--kubeconfig $(CURDIR)/bin/kind-kubeconfig-kindnet --timeout 5m
	docker exec nbsandbox-kindnet-control-plane kubectl --kubeconfig=/etc/kubernetes/admin.conf -n nbsandbox \
		rollout status deploy/nbsandbox-api --timeout=300s
	NBSANDBOX_CLUSTER=1 NBSANDBOX_K8S=1 NBSANDBOX_KIND_NODE=nbsandbox-kindnet-control-plane NBSANDBOX_EXPECT_OPEN_NETWORK=1 \
		$(GO) test -tags integration -count=1 -timeout 15m -v -run '^TestNetworkNotEnforcedRefusesRuns$$' ./tests/integration/

.PHONY: soak-cluster soak-k8s
soak-cluster: cluster-data ## The soak on the cluster stack (two API processes, two workers), each process judged on its own
	$(COMPOSE) stop
	@$(STOP_KIND)
	$(CLUSTER_TEST_ENV) $(SOAK_ENV) NBSANDBOX_RUN_CONCURRENCY=2 $(CLUSTER) up -d --build --force-recreate
	$(CLUSTER_TEST_ENV) $(SOAK_ENV) SOAK_DURATION=$(SOAK_DURATION) REPORT_DIR="$(CURDIR)/bin/soak-cluster" $(GO) test -tags integration -count=1 -timeout 12h -v -run '^TestSoak$$' ./tests/integration/

soak-k8s: k8s-up ## The soak on the local Kubernetes stack, cleanup timers shortened
	$(HELM) upgrade nbsandbox deploy/helm/nbsandbox -n nbsandbox -f $(KIND_DIR)/values.yaml -f $(KIND_DIR)/soak.yaml \
		--kubeconfig $(KIND_CONFIG) --wait --timeout 5m
	$(KUBECTL) -n nbsandbox rollout status deploy/nbsandbox-api deploy/nbsandbox-worker --timeout=180s
	$(K8S_TEST_ENV) NBSANDBOX_RUN_RETENTION=10m NBSANDBOX_EVENT_RETENTION=10m NBSANDBOX_IDEMPOTENCY_WINDOW=10m NBSANDBOX_CLEANUP_EVERY=2m \
		SOAK_DURATION=$(SOAK_DURATION) REPORT_DIR="$(CURDIR)/bin/soak-k8s" $(GO) test -tags integration -count=1 -timeout 12h -v -run '^TestSoak$$' ./tests/integration/
	$(HELM) upgrade nbsandbox deploy/helm/nbsandbox -n nbsandbox -f $(KIND_DIR)/values.yaml --kubeconfig $(KIND_CONFIG) --wait --timeout 5m

.PHONY: report-k8s
report-k8s: k8s-up ## Run the integration suite on the local Kubernetes stack and write docs/isolation-report-kubernetes.md (K8S_RUNTIME_CLASS=gvisor adds gVisor beside runc)
	rm -rf bin/findings-k8s/$(or $(K8S_RUNTIME_CLASS),runc)
	$(K8S_TEST_ENV) REPORT_DIR="$(CURDIR)/bin/findings-k8s" $(GO) test -tags integration -count=1 -timeout 60m -v ./tests/integration/
	$(GO) run ./cmd/nbsandbox-report -in bin/findings-k8s -out docs/isolation-report-kubernetes.md -require

.PHONY: proto
proto: ## Regenerate the gRPC Go code from api/proto (buf through go run; nothing to install)
	cd api/proto && $(GO) run github.com/bufbuild/buf/cmd/buf@v1.55.1 lint
	cd api/proto && $(GO) run github.com/bufbuild/buf/cmd/buf@v1.55.1 generate

.PHONY: wipe-dev
wipe-dev: ## Stop the dev stack and delete its database volume (the data folder is left to you)
	$(COMPOSE) down --volumes
	@echo "dev database removed; delete $(NBSANDBOX_DATA_DIR) yourself for a completely fresh start"

.PHONY: logs
logs: ## Follow the dev stack's logs
	$(COMPOSE) logs -f

.PHONY: run-native
run-native: build dev-data test-db dev-s3 ## Run nbsandboxd on this machine against Docker Desktop (no proxy; database: the test PostgreSQL)
	NBSANDBOX_DATABASE_URL="$(TEST_DB_URL)&search_path=native" $(DEV_S3_ENV) NBSANDBOX_S3_PREFIX=native/ \
	NBSANDBOX_USERS_FILE="$(NBSANDBOX_DATA_DIR)/users.yaml" NBSANDBOX_LISTEN=127.0.0.1:8080 \
	NBSANDBOX_UI_ENABLED=true NBSANDBOX_UI_LISTEN=127.0.0.1:8090 \
	NBSANDBOX_DOCKER_HOST="$${NBSANDBOX_DOCKER_HOST:-$(DESKTOP_SOCK)}" \
	./bin/nbsandboxd -config config/settings.yaml

UI_PREVIEW_DATA ?= $(HOME)/.local/share/nbsandbox-ui-preview

.PHONY: ui-preview
ui-preview: build test-db dev-s3 ## Work on the web UI: nbsandboxd on 127.0.0.1:18080, UI on :18090, own data folder and schema; runs can't start
	@mkdir -p "$(UI_PREVIEW_DATA)" && chmod 700 "$(UI_PREVIEW_DATA)"
	@install -m 600 config/users.yaml "$(UI_PREVIEW_DATA)/users.yaml"
	NBSANDBOX_DATA_DIR="$(UI_PREVIEW_DATA)" NBSANDBOX_USERS_FILE="$(UI_PREVIEW_DATA)/users.yaml" \
	NBSANDBOX_DATABASE_URL="$(TEST_DB_URL)&search_path=ui_preview" $(DEV_S3_ENV) NBSANDBOX_S3_PREFIX=ui-preview/ \
	NBSANDBOX_LISTEN=127.0.0.1:18080 NBSANDBOX_UI_ENABLED=true NBSANDBOX_UI_LISTEN=127.0.0.1:18090 \
	NBSANDBOX_DOCKER_HOST=unix:///nonexistent/nbsandbox-ui-preview.sock \
	./bin/nbsandboxd -config config/settings.yaml

# Docker Desktop's socket, when present (Go programs ignore docker contexts).
BACKEND_DOCKER_HOST ?= $(shell test -S "$(HOME)/.docker/run/docker.sock" && echo "unix://$(HOME)/.docker/run/docker.sock")

.PHONY: test-backend
test-backend: ## Backend conformance suite against the local Docker daemon
	NBSANDBOX_DOCKER_HOST="$(BACKEND_DOCKER_HOST)" $(GO) test -tags integration -count=1 -timeout 15m $(TESTFLAGS) ./internal/isolation/...

.PHONY: test-integration
# The soak: several spaces at once, sampled every minute, judged on trends;
# cleanup timers shortened so every cleanup path runs many times. 45 min by
# default (nightly); SOAK_DURATION=3h before a release. docs/perf.md has the
# baseline it is compared with.
SOAK_DURATION ?= 45m
SOAK_ENV := NBSANDBOX_SHUTDOWN_GRACE=5s NBSANDBOX_RUN_TIMEOUT=60s NBSANDBOX_RUN_CONCURRENCY=4 \
	NBSANDBOX_RUN_RETENTION=10m NBSANDBOX_EVENT_RETENTION=10m NBSANDBOX_IDEMPOTENCY_WINDOW=10m NBSANDBOX_CLEANUP_EVERY=2m

.PHONY: soak
soak: dev-data ## Soak test (default 45m): leaks judged on trends, cleanup timers shortened (restarts the stack)
	@$(STOP_KIND)
	-@$(CLUSTER) stop >/dev/null 2>&1
	$(SOAK_ENV) $(COMPOSE) up -d --build --force-recreate nbsandboxd
	$(SOAK_ENV) SOAK_DURATION=$(SOAK_DURATION) $(GO) test -tags integration -count=1 -timeout 12h -v -run '^TestSoak$$' ./tests/integration/

test-integration: dev-data ## Integration tests against the dev stack (Docker needed; restarts the stack)
	@$(STOP_KIND)
	-@$(CLUSTER) stop >/dev/null 2>&1
	NBSANDBOX_SHUTDOWN_GRACE=5s NBSANDBOX_RUN_TIMEOUT=60s $(COMPOSE) up -d --build
	NBSANDBOX_SHUTDOWN_GRACE=5s NBSANDBOX_RUN_TIMEOUT=60s $(GO) test -tags integration -count=1 -timeout 30m $(TESTFLAGS) ./tests/integration/

# The slow tests: they wait on real limits (CPU share, the run timeout,
# endless output) or scan a whole container (isolation). make report runs
# them; test-quick skips them, with a shorter run timeout, for everyday
# changes.
SLOW_TESTS := ^(TestIsolationSuite|TestCPUFairness|TestEndlessOutput|TestSoak)$$

.PHONY: test-quick
test-quick: dev-data ## Integration tests minus the slow limit and isolation tests (a few minutes; restarts the stack)
	@$(STOP_KIND)
	-@$(CLUSTER) stop >/dev/null 2>&1
	NBSANDBOX_SHUTDOWN_GRACE=5s NBSANDBOX_RUN_TIMEOUT=25s $(COMPOSE) up -d --build
	NBSANDBOX_SHUTDOWN_GRACE=5s NBSANDBOX_RUN_TIMEOUT=25s $(GO) test -tags integration -count=1 -timeout 20m \
		-skip '$(SLOW_TESTS)' $(TESTFLAGS) ./tests/integration/

FINDINGS := $(CURDIR)/bin/findings

.PHONY: report
report: ## Run the integration suite and write docs/isolation-report.md (Docker needed)
	rm -rf "$(FINDINGS)"
	$(MAKE) test-integration REPORT_DIR="$(FINDINGS)" TESTFLAGS=-v
	$(GO) run ./cmd/nbsandbox-report -in "$(FINDINGS)" -out docs/isolation-report.md -require

# The screenshots of docs/tour.md, retaken against the dev stack.
.PHONY: tour
tour: ## Retake the screenshots of docs/tour.md (needs make up)
	scripts/tour/run.sh

# The reference report: the suite on Linux with Docker Engine, with runc and
# gVisor's runsc, in a Lima VM (LIMA_VM, default gvisor; docs/gvisor.md).
.PHONY: report-linux
report-linux: ## Run the integration suite on Linux (Lima VM) with runc and runsc; write docs/isolation-report-linux.md
	scripts/report-linux.sh

# Production stack on this machine, for verification:
# Caddy on https://localhost:8443, nbsandboxd also on 127.0.0.1:8085.
PROD_DATA ?= $(HOME)/.local/share/nbsandbox-prod
PROD := NBSANDBOX_DATA_DIR="$(PROD_DATA)" NBSANDBOX_USERS="$(PROD_DATA)/users.yaml" NBSANDBOX_SETTINGS=../config/settings.yaml \
	NBSANDBOX_IMAGE=nbsandboxd:prod-local NBSANDBOX_RUNNER_IMAGE=$(RUNNER_IMAGE) NBSANDBOX_HTTP_PORT=8081 NBSANDBOX_HTTPS_PORT=8443 \
	DOCKER_GID=$${DOCKER_GID:-0} docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.local.yml

.PHONY: prod-local
prod-local: ## Start the production stack locally (Caddy on https://localhost:8443)
	@mkdir -p "$(PROD_DATA)/spaces" && chmod 700 "$(PROD_DATA)"
	@install -m 600 config/users.yaml "$(PROD_DATA)/users.yaml"
	$(PROD) up -d --build --wait

.PHONY: prod-local-down
prod-local-down: ## Stop the local production stack (including the optional Prometheus)
	$(PROD) --profile monitoring down

.PHONY: preflight
preflight: ## Check this host's prerequisites
	@scripts/preflight.sh

.PHONY: probes
probes: ## Regenerate isolation probe notebooks (runner image needed)
	docker run --rm --user "$$(id -u):$$(id -g)" -v "$(CURDIR)/tests/probes":/w -w /w --entrypoint python $(RUNNER_IMAGE) make_probes.py

.PHONY: ui-vendor
ui-vendor: ## Rebuild the web UI's editor bundle (CodeMirror) from internal/ui/vendor-src (Docker, network)
	docker run --rm -v "$(CURDIR)/internal/ui/vendor-src":/src -v "$(CURDIR)/internal/ui/web/vendor":/out \
		--entrypoint sh $(NODE_IMAGE) /src/build.sh

.PHONY: clean
clean: ## Remove build output
	rm -f bin/nbsandboxd coverage.out
