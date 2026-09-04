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
# OPERATOR_NAME is the image repository name only (the binary is BINARY_NAME,
# the chart is HELM_CHART_NAME, the namespace is OPERATOR_NS). Kept short so it
# reads as a pair with the Identity Server image: curity/idsvr, curity/operator.
OPERATOR_NAME ?= operator
OPERATOR_NS ?= curity-operator
DOCKER_REPO_BASE ?= curity.azurecr.io/curity
# IMAGE_REPO is the image reference baked into the chart and install.yaml by
# version-sync. It tracks IMG by default; CI overrides both when publishing
# main-branch builds to GHCR instead of the release registry.
IMAGE_REPO ?= $(DOCKER_REPO_BASE)/$(OPERATOR_NAME)
IMG ?= $(IMAGE_REPO):v$(VERSION)
CONTAINER_TOOL ?= docker

BINARY_NAME ?= curity-operator
LOCALBIN = $(shell pwd)/bin
TEST_CLUSTER_NAME ?= e2e-test-cluster
KIND_VERSION ?= v0.27.0

## Tool Versions
KUSTOMIZE_VERSION ?= v5.6.0
CONTROLLER_TOOLS_VERSION ?= v0.20.1
HELMIFY_VERSION ?= v0.4.19
YQ_VERSION ?= v4.44.1
ENVTEST_VERSION ?= release-0.19
ENVTEST_K8S_VERSION ?= 1.31.0

## Tool Binaries
KUBECTL ?= kubectl
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
HELMIFY ?= $(LOCALBIN)/helmify
YQ ?= $(LOCALBIN)/yq
ENVTEST ?= $(LOCALBIN)/setup-envtest

## Helm
HELM_CHART_DIR ?= charts/curity-operator
HELM_CHART_NAME ?= curity-operator
HELM_REGISTRY ?= curity.azurecr.io/charts
HELMIFY_POSTPROCESS ?= hack/helm/postprocess.sh

.PHONY: all
all: build

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Generation

.PHONY: manifests
manifests: controller-gen ## Generate CRD and RBAC manifests from Go markers.
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd paths="./..." output:crd:artifacts:config=config/crd/bases output:rbac:artifacts:config=config/rbac

.PHONY: generate
generate: controller-gen ## Generate DeepCopy methods.
	$(CONTROLLER_GEN) object paths="./..."

##@ Build

.PHONY: build
build: generate ## Build the operator binary.
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
test: envtest ## Run unit tests.
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" go test $$(go list ./... | grep -v /test/) -v -count=1

.PHONY: test-e2e
test-e2e: generate deploy-kind install ## Build, load into Kind, install CRDs, run e2e tests.
	go test -v -timeout 1200s ./test/e2e/...

.PHONY: test-e2e-remote
test-e2e-remote: generate deploy-remote install ## Build, push to registry, install CRDs, run e2e tests.
	E2E_REMOTE=true go test -v -timeout 1200s ./test/e2e/...

.PHONY: test-e2e-update-snapshots
test-e2e-update-snapshots: generate deploy-kind install ## Run e2e tests and update snapshots.
	go test -v -timeout 1200s ./test/e2e/...

##@ Docker

TARGET_PLATFORM ?= linux/arm64

.PHONY: docker-build
docker-build: ## Build docker image.
	$(CONTAINER_TOOL) build --platform $(TARGET_PLATFORM) --build-arg VERSION=$(VERSION) -t $(IMG) .

.PHONY: docker-push
docker-push: ## Push docker image.
	$(CONTAINER_TOOL) push $(IMG)

PLATFORMS ?= linux/arm64,linux/amd64
.PHONY: docker-buildx
docker-buildx: ## Build and push multi-arch docker image.
	- $(CONTAINER_TOOL) buildx create --name curity-builder
	$(CONTAINER_TOOL) buildx use curity-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) --build-arg VERSION=$(VERSION) --tag $(IMG) .
	- $(CONTAINER_TOOL) buildx rm curity-builder

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster.
	$(KUSTOMIZE) build config/crd | $(KUBECTL) apply -f -

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster.
	$(KUSTOMIZE) build config/crd | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -

# Kustomize overlay used by kustomize-deploy/undeploy. config/default is the
# released layout (public registry, no pull secret); CI sets DEPLOY_OVERLAY to
# config/e2e, which adds the pull secret for the private GHCR test image. The
# e2e suite shells out to `make kustomize-deploy` and inherits the environment,
# so exporting DEPLOY_OVERLAY in the workflow is enough to select the overlay.
DEPLOY_OVERLAY ?= config/default

.PHONY: kustomize-deploy
kustomize-deploy: manifests kustomize ## Deploy operator to the K8s cluster via Kustomize.
	cd config/manager && $(KUSTOMIZE) edit set image controller=$(IMG)
	$(KUSTOMIZE) build $(DEPLOY_OVERLAY) | $(KUBECTL) apply -f -

.PHONY: undeploy
undeploy: kustomize ## Undeploy operator from the K8s cluster.
	$(KUSTOMIZE) build $(DEPLOY_OVERLAY) | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -

.PHONY: build-installer
build-installer: manifests kustomize ## Generate a consolidated install.yaml.
	mkdir -p dist
	cd config/manager && $(KUSTOMIZE) edit set image controller=$(IMG)
	$(KUSTOMIZE) build config/default > dist/install.yaml

##@ Helm

.PHONY: helmify
helmify: manifests kustomize helmify-bin yq ## Generate Helm chart from Kustomize manifests.
	$(KUSTOMIZE) build config/default | $(YQ) 'del(.. | .imagePullSecrets?)' | $(HELMIFY) -image-pull-secrets $(HELM_CHART_DIR)
	@if [ -n "$(HELMIFY_POSTPROCESS)" ] && [ -x "$(HELMIFY_POSTPROCESS)" ]; then \
		echo "Running Helm postprocessing..."; \
		HELM_CHART_DIR=$(HELM_CHART_DIR) YQ=$(YQ) $(HELMIFY_POSTPROCESS); \
	fi

.PHONY: helm-lint
helm-lint: ## Lint the generated Helm chart.
	helm lint $(HELM_CHART_DIR) --strict

.PHONY: helm-template
helm-template: ## Render Helm templates locally (dry-run).
	helm template $(HELM_CHART_NAME) $(HELM_CHART_DIR)

.PHONY: helm-package
helm-package: helm-lint ## Package the Helm chart.
	rm -f $(HELM_CHART_NAME)-*.tgz
	helm package $(HELM_CHART_DIR)

.PHONY: helm-push
helm-push: helm-package ## Push Helm chart to OCI registry.
	helm push $(HELM_CHART_NAME)-*.tgz oci://$(HELM_REGISTRY)

.PHONY: version-sync
version-sync: yq ## Patch all version and image-repository references to VERSION/IMAGE_REPO.
	$(SED) -i 's/^version: .*/version: $(VERSION)/' $(HELM_CHART_DIR)/Chart.yaml
	$(SED) -i 's/^appVersion: .*/appVersion: "$(VERSION)"/' $(HELM_CHART_DIR)/Chart.yaml
	$(YQ) -i '.controllerManager.manager.image.tag = "v$(VERSION)"' $(HELM_CHART_DIR)/values.yaml
	$(YQ) -i '.controllerManager.manager.image.repository = "$(IMAGE_REPO)"' $(HELM_CHART_DIR)/values.yaml
	$(SED) -i 's/newTag: .*/newTag: v$(VERSION)/' config/manager/kustomization.yaml
	$(SED) -i 's|newName: .*|newName: $(IMAGE_REPO)|' config/manager/kustomization.yaml

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

.PHONY: deploy-kind
deploy-kind: docker-build cluster ## Build and load image into Kind cluster.
	$(KIND) load docker-image $(IMG) --name $(TEST_CLUSTER_NAME)

.PHONY: deploy-remote
deploy-remote: docker-build docker-push ## Build and push image to registry.

.PHONY: deploy
deploy: deploy-kind ## Alias for deploy-kind (backward compat).

.PHONY: deploy-helm
deploy-helm: deploy-kind helmify ## Build, load into Kind, and deploy via Helm.
	helm upgrade --install curity-operator $(HELM_CHART_DIR) \
		--namespace $(OPERATOR_NS) \
		--create-namespace \
		--set controllerManager.manager.image.repository=$(DOCKER_REPO_BASE)/$(OPERATOR_NAME) \
		--set controllerManager.manager.image.tag=v$(VERSION) \
		--wait --timeout 120s

.PHONY: undeploy-helm
undeploy-helm: ## Uninstall the operator Helm release.
	helm uninstall curity-operator --namespace $(OPERATOR_NS)

##@ Tools

.PHONY: kustomize
kustomize: ## Download kustomize locally if necessary.
ifeq (,$(wildcard $(KUSTOMIZE)))
	@{ \
	set -e ;\
	mkdir -p $(LOCALBIN) ;\
	curl -sSLo $(LOCALBIN)/kustomize.tar.gz https://github.com/kubernetes-sigs/kustomize/releases/download/kustomize/$(KUSTOMIZE_VERSION)/kustomize_$(KUSTOMIZE_VERSION)_$(OS)_$(ARCH).tar.gz ;\
	tar -xzf $(LOCALBIN)/kustomize.tar.gz -C $(LOCALBIN) ;\
	rm -f $(LOCALBIN)/kustomize.tar.gz ;\
	chmod +x $(KUSTOMIZE) ;\
	}
endif

.PHONY: controller-gen
controller-gen: ## Download controller-gen locally if necessary.
ifeq (,$(wildcard $(CONTROLLER_GEN)))
	@{ \
	set -e ;\
	mkdir -p $(LOCALBIN) ;\
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION) ;\
	}
endif

.PHONY: envtest
envtest: ## Download setup-envtest locally if necessary.
ifeq (,$(wildcard $(ENVTEST)))
	@{ \
	set -e ;\
	mkdir -p $(LOCALBIN) ;\
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(ENVTEST_VERSION) ;\
	}
endif

.PHONY: helmify-bin
helmify-bin: ## Download helmify locally if necessary.
ifeq (,$(wildcard $(HELMIFY)))
	@{ \
	set -e ;\
	mkdir -p $(LOCALBIN) ;\
	GOBIN=$(LOCALBIN) go install github.com/arttor/helmify/cmd/helmify@$(HELMIFY_VERSION) ;\
	}
endif

.PHONY: yq
yq: ## Download yq locally if necessary.
ifeq (,$(wildcard $(YQ)))
	@{ \
	set -e ;\
	mkdir -p $(LOCALBIN) ;\
	curl -sSLo $(YQ) https://github.com/mikefarah/yq/releases/download/$(YQ_VERSION)/yq_$(OS)_$(ARCH) ;\
	chmod +x $(YQ) ;\
	}
endif

##@ Clean

.PHONY: clean
clean: ## Remove build artifacts.
	rm -rf $(LOCALBIN)/$(BINARY_NAME) dist/
