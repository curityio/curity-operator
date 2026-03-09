DETECTED_OS := $(shell uname -s)
OS := $(patsubst Darwin,darwin,$(patsubst Linux,linux,$(DETECTED_OS)))
DETECTED_ARCH := $(shell uname -m)
ARCH := $(patsubst x86_64,amd64,$(patsubst aarch64,arm64,$(DETECTED_ARCH)))

ifeq ($(OS),darwin)
  SED := gsed
  SHELL := /bin/zsh
else
  SED := sed
  SHELL := /bin/bash
endif

VERSION ?= 0.0.1
OPERATOR_NAME ?= curity-operator
DOCKER_REPO_BASE ?= ghcr.io/curityio
IMG ?= $(DOCKER_REPO_BASE)/$(OPERATOR_NAME):v$(VERSION)$(GIT_TAG)
CONTAINER_TOOL ?= docker

BINARY_NAME ?= curity-operator
LOCALBIN = $(shell pwd)/bin
TEST_CLUSTER_NAME ?= e2e-test-cluster
KIND_VERSION ?= v0.27.0

.PHONY: all
all: build

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Build

.PHONY: build
build: ## Build the operator binary.
	go build -o $(LOCALBIN)/$(BINARY_NAME) ./cmd/manager/

.PHONY: run
run: build ## Run the operator locally.
	$(LOCALBIN)/$(BINARY_NAME)

.PHONY: fmt
fmt: ## Run go fmt.
	go fmt ./...

GOLANGCI_LINT_VERSION ?= v2.11.2

.PHONY: vet
vet: ## Run go vet.
	go vet ./...

.PHONY: lint
lint: golangci-lint ## Run golangci-lint.
	$(GOLANGCI_LINT) run ./...

.PHONY: golangci-lint
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint
golangci-lint: ## Download golangci-lint locally if necessary.
ifeq (,$(wildcard $(GOLANGCI_LINT)))
	@{ \
	set -e ;\
	mkdir -p $(LOCALBIN) ;\
	curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/HEAD/install.sh | sh -s -- -b $(LOCALBIN) $(GOLANGCI_LINT_VERSION) ;\
	}
endif

##@ Testing

.PHONY: test
test: ## Run unit tests.
	go test ./... -v -count=1

.PHONY: test-e2e
test-e2e: deploy ## Create Kind cluster, build and load operator, then run e2e tests.
	./tests/run_tests.sh

##@ Docker

.PHONY: docker-build
docker-build: ## Build docker image.
	$(CONTAINER_TOOL) build -t $(IMG) .

.PHONY: docker-push
docker-push: ## Push docker image.
	$(CONTAINER_TOOL) push $(IMG)

PLATFORMS ?= linux/arm64,linux/amd64
.PHONY: docker-buildx
docker-buildx: ## Build and push multi-arch docker image.
	- $(CONTAINER_TOOL) buildx create --name curity-builder
	$(CONTAINER_TOOL) buildx use curity-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) --tag $(IMG) .
	- $(CONTAINER_TOOL) buildx rm curity-builder

##@ Cluster

.PHONY: kind
KIND = $(LOCALBIN)/kind
kind: ## Download kind locally if necessary.
ifeq (,$(wildcard $(KIND)))
	@{ \
	set -e ;\
	mkdir -p $(LOCALBIN) ;\
	curl -sSLo $(KIND) https://github.com/kubernetes-sigs/kind/releases/download/$(KIND_VERSION)/kind-$(OS)-$(ARCH) ;\
	chmod +x $(KIND) ;\
	}
endif

.PHONY: cluster
cluster: kind ## Create a Kind cluster for e2e testing.
	@if $(KIND) get clusters 2>/dev/null | grep -q "^$(TEST_CLUSTER_NAME)$$"; then \
		echo "Kind cluster '$(TEST_CLUSTER_NAME)' already exists"; \
	else \
		echo "Creating Kind cluster '$(TEST_CLUSTER_NAME)'..."; \
		$(KIND) create cluster --name $(TEST_CLUSTER_NAME) --wait 60s; \
	fi
	@kubectl cluster-info --context kind-$(TEST_CLUSTER_NAME)

.PHONY: cluster-destroy
cluster-destroy: kind ## Destroy the Kind e2e cluster.
	$(KIND) delete cluster --name $(TEST_CLUSTER_NAME)

.PHONY: deploy
deploy: docker-build cluster ## Build, load into Kind, and deploy the operator.
	$(KIND) load docker-image $(IMG) --name $(TEST_CLUSTER_NAME)

##@ Clean

.PHONY: clean
clean: ## Remove build artifacts.
	rm -rf $(LOCALBIN)/$(BINARY_NAME)
