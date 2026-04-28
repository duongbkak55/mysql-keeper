## mysql-keeper Makefile
## Requires: go, docker, kubectl, kustomize, controller-gen

# Image settings — override via environment variables.
IMG            ?= ghcr.io/duongnguyen/mysql-keeper:latest
DOCKERHUB_IMG  ?= duongbkak55/mysql-keeper
VERSION        ?= 0.4.8
CONTROLLER_GEN_VERSION ?= v0.14.0
KUSTOMIZE_VERSION ?= v5.3.0

# Go settings.
GOBIN ?= $(shell go env GOPATH)/bin

.PHONY: all build test lint docker-build docker-push \
        generate manifests install uninstall \
        deploy-dc undeploy-dc deploy-dr undeploy-dr \
        help

all: build

## ── Development ─────────────────────────────────────────────────────────────

build: ## Build the controller binary.
	go build -o bin/manager ./cmd/main.go

test: ## Run unit tests.
	go test ./... -v -count=1

lint: ## Run golangci-lint.
	golangci-lint run ./...

## ── Code Generation ─────────────────────────────────────────────────────────

controller-gen: ## Install controller-gen if not present.
	test -s $(GOBIN)/controller-gen || GOBIN=$(GOBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)

generate: controller-gen ## Regenerate deepcopy methods.
	$(GOBIN)/controller-gen object:headerFile="hack/boilerplate.go.txt" paths="./..."

manifests: controller-gen ## Regenerate CRD YAML manifests.
	$(GOBIN)/controller-gen crd rbac:roleName=mysql-keeper-manager-role \
		paths="./..." output:crd:artifacts:config=config/crd/bases

## ── Docker ───────────────────────────────────────────────────────────────────

docker-build: ## Build and tag the controller Docker image.
	docker build -t $(IMG) .

docker-push: ## Push the controller Docker image to the registry.
	docker push $(IMG)

docker-buildx-push: ## Build multi-arch image (amd64+arm64) and push to Docker Hub.
	docker buildx build \
		--platform linux/amd64,linux/arm64 \
		--label "org.opencontainers.image.source=https://github.com/duongbkak55/mysql-keeper" \
		--label "org.opencontainers.image.revision=$$(git rev-parse HEAD)" \
		--label "org.opencontainers.image.title=mysql-keeper" \
		--label "org.opencontainers.image.version=$(VERSION)" \
		-t $(DOCKERHUB_IMG):$(VERSION) \
		-t $(DOCKERHUB_IMG):latest \
		--push .

## ── CRD Install/Uninstall ────────────────────────────────────────────────────

install: ## Install CRD into the current cluster (kubectl context).
	kubectl apply -f config/crd/bases/

uninstall: ## Remove CRD from the current cluster.
	kubectl delete -f config/crd/bases/ --ignore-not-found

## ── Deploy DC Cluster ────────────────────────────────────────────────────────

deploy-dc: ## Deploy mysql-keeper to DC cluster (set KUBECONFIG to DC context).
	kustomize build deploy/dc | kubectl apply -f -

undeploy-dc: ## Remove mysql-keeper from DC cluster.
	kustomize build deploy/dc | kubectl delete -f - --ignore-not-found

## ── Deploy DR Cluster ────────────────────────────────────────────────────────

deploy-dr: ## Deploy mysql-keeper to DR cluster (set KUBECONFIG to DR context).
	kustomize build deploy/dr | kubectl apply -f -

undeploy-dr: ## Remove mysql-keeper from DR cluster.
	kustomize build deploy/dr | kubectl delete -f - --ignore-not-found

## ── Secrets Helper ───────────────────────────────────────────────────────────

create-secrets-dc: ## Create required Secrets in DC cluster. Set env vars first.
	@echo "Creating Secrets in DC cluster..."
	kubectl create secret generic pxc-dc-admin-creds \
		--from-literal=username=$(DC_MYSQL_USER) \
		--from-literal=password=$(DC_MYSQL_PASS) \
		-n mysql-keeper-system --dry-run=client -o yaml | kubectl apply -f -
	kubectl create secret generic pxc-dr-admin-creds \
		--from-literal=username=$(DR_MYSQL_USER) \
		--from-literal=password=$(DR_MYSQL_PASS) \
		-n mysql-keeper-system --dry-run=client -o yaml | kubectl apply -f -
	kubectl create secret generic proxysql-dc-admin-creds \
		--from-literal=username=$(PROXYSQL_ADMIN_USER) \
		--from-literal=password=$(PROXYSQL_ADMIN_PASS) \
		-n mysql-keeper-system --dry-run=client -o yaml | kubectl apply -f -

create-secrets-dr: ## Create required Secrets in DR cluster. Set env vars first.
	@echo "Creating Secrets in DR cluster..."
	kubectl create secret generic pxc-dr-admin-creds \
		--from-literal=username=$(DR_MYSQL_USER) \
		--from-literal=password=$(DR_MYSQL_PASS) \
		-n mysql-keeper-system --dry-run=client -o yaml | kubectl apply -f -
	kubectl create secret generic pxc-dc-admin-creds \
		--from-literal=username=$(DC_MYSQL_USER) \
		--from-literal=password=$(DC_MYSQL_PASS) \
		-n mysql-keeper-system --dry-run=client -o yaml | kubectl apply -f -
	kubectl create secret generic proxysql-dr-admin-creds \
		--from-literal=username=$(PROXYSQL_ADMIN_USER) \
		--from-literal=password=$(PROXYSQL_ADMIN_PASS) \
		-n mysql-keeper-system --dry-run=client -o yaml | kubectl apply -f -

## ── Manual Switchover ────────────────────────────────────────────────────────

switchover: ## Trigger a manual planned switchover. Usage: make switchover CLUSTER=dc|dr
	kubectl patch clusterswitchpolicy dc-dr-policy \
		--type=merge -p '{"spec":{"manualSwitchoverTarget":"promote-remote"}}'

## ── Help ─────────────────────────────────────────────────────────────────────

help: ## Show this help message.
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}'
