# Curity Operator

Kubernetes operator for managing Curity Identity Server deployments.
Built with Go and controller-runtime, distributed via Helm and GHCR.

## Prerequisites

- Go 1.25+
- Docker (with Buildx for multi-arch builds)
- kubectl
- Helm 3.x (for Helm-based deployment)

All other tools (kustomize, controller-gen, kind, golangci-lint, etc.)
are automatically downloaded by the Makefile when needed.

## Local Development

Build the operator binary:

```bash
make build
```

Deploy to a local Kind cluster:

```bash
make deploy-kind       # Builds image, creates Kind cluster, loads image
make install           # Install CRDs into the cluster
make kustomize-deploy  # Deploy the operator
```

Create an IdentityServer resource:

```yaml
apiVersion: curity.io/v1alpha1
kind: IdentityServer
metadata:
  name: my-idsvr
spec:
  replicas: 2
```

```bash
kubectl apply -f identityserver.yaml
kubectl get idsvr
```

## Running Tests

```bash
make test       # Unit tests (uses envtest, no cluster needed)
make test-e2e   # Full E2E: builds, creates Kind cluster, installs CRDs, runs tests
```

Run E2E tests against a remote cluster (AKS/EKS):

```bash
make test-e2e-remote
```

Update E2E snapshots:

```bash
UPDATE_SNAPS=true make test-e2e
```

Tests use Ginkgo v2 + Gomega for assertions and go-snaps for
snapshot testing.

## Code Generation

Run these after changing `api/` types or RBAC markers:

```bash
make generate      # Regenerate DeepCopy methods
make manifests     # Regenerate CRD and RBAC manifests
make helmify       # Regenerate Helm chart from Kustomize
                   # (never edit chart templates manually)
make version-sync  # Sync version across Chart.yaml, values.yaml, kustomization
```

## Linting

```bash
make lint  # Must pass before opening a PR
make fmt   # Run go fmt
make vet   # Run go vet
```

## CRD Reference

| Field | Value |
|---|---|
| API Group | `curity.io` |
| API Version | `v1alpha1` |
| Kind | `IdentityServer` |
| Short Name | `idsvr` |
| Spec Fields | `replicas` (int32, optional) |

## Cleanup

```bash
make undeploy        # Remove the operator from the cluster
make uninstall       # Remove CRDs
make cluster-destroy # Delete the Kind cluster
```

Run `make help` to see all available targets.
