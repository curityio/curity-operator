# Curity Operator

Kubernetes operator for managing Curity Identity Server deployments.
Built with Go and controller-runtime, distributed via Helm and GHCR.

## Architecture

The operator uses a **two-CRD design**:

- **IdentityServerCluster** (`isc`) — cluster-wide configuration: version, credentials, logging, scheduling defaults.
- **IdentityServerNode** (`isn`) — individual node deployments (admin or runtime). Each node references a cluster and inherits its settings, with optional node-level overrides.

The operator automatically creates Deployments, Services, and Secrets for each node. Cluster-level settings (resources, probes, scheduling, labels) are inherited by nodes unless overridden.

## Prerequisites

- Go 1.25+
- Docker (with Buildx for multi-arch builds)
- kubectl
- Helm 3.x (for Helm-based deployment)

All other tools (kustomize, controller-gen, kind, golangci-lint, etc.)
are automatically downloaded by the Makefile when needed.

## Local Development

### Option A: Kustomize

```bash
make deploy-kind                               # Build image, create Kind cluster, load image
make install                                   # Install CRDs
kubectl create namespace curity-operator       # Create operator namespace
make kustomize-deploy                          # Deploy the operator
```

### Option B: Helm

```bash
make deploy-helm    # Build, create Kind cluster, load image, deploy via Helm (creates namespace automatically)
```

Verify the operator is running:

```bash
kubectl -n curity-operator get pods
```

### Create a Curity Identity Server Deployment

1. Create a namespace and an IdentityServerCluster:

```bash
kubectl create namespace demo
```

```yaml
apiVersion: curity.io/v1alpha1
kind: IdentityServerCluster
metadata:
  name: my-cluster
  namespace: demo
spec:
  version: "11.0"
```

When `adminCredentials` is omitted, the operator automatically creates a Secret
named `<cluster>-admin-creds` with generated values for `ADMIN_PASSWORD`,
`CONFIG_ENCRYPTION_KEY`, and `KEYSTORE_PASSWORD`.

To use a pre-existing Secret instead, specify `adminCredentials` explicitly:

```yaml
spec:
  adminCredentials:
    valueFrom:
      secretKeyRef:
        name: admin-creds
        items:
          - key: ADMIN_PASSWORD
            path: ADMIN_PASSWORD
          - key: CONFIG_ENCRYPTION_KEY
            path: CONFIG_ENCRYPTION_KEY
          - key: KEYSTORE_PASSWORD
            path: KEYSTORE_PASSWORD
```

2. Create an admin node with the UI enabled:

```yaml
apiVersion: curity.io/v1alpha1
kind: IdentityServerNode
metadata:
  name: admin
  namespace: demo
spec:
  type: admin
  role: admin
  identityServerClusterRef:
    name: my-cluster
  replicas: 1
  ui:
    enabled: true
    secure: false
  service:
    type: ClusterIP
    port: 6749
```

3. Create runtime nodes:

```yaml
apiVersion: curity.io/v1alpha1
kind: IdentityServerNode
metadata:
  name: runtime
  namespace: demo
spec:
  type: runtime
  role: runtime
  identityServerClusterRef:
    name: my-cluster
  replicas: 2
  service:
    type: ClusterIP
    port: 8443
```

4. Check status:

```bash
kubectl -n demo get isc          # IdentityServerClusters
kubectl -n demo get isn          # IdentityServerNodes
kubectl -n demo get deployments  # Auto-created Deployments
kubectl -n demo get services     # Auto-created Services
```

### Accessing the Admin UI

The admin UI is **disabled by default**. Set `ui.enabled: true` on the admin
IdentityServerNode to enable it. Admin credentials are auto-generated if not
provided, or can be set explicitly via `adminCredentials` on the cluster — see
[Create a Curity Identity Server Deployment](#create-a-curity-identity-server-deployment)
for both options.

To access the UI locally:

```bash
kubectl -n demo port-forward svc/admin 6749:6749
```

- HTTPS (default): `https://localhost:6749/admin`
- HTTP (`ui.secure: false`): `http://localhost:6749/admin`

Login with username `admin`. Retrieve the auto-generated password:

```bash
kubectl -n demo get secret my-cluster-admin-creds -o jsonpath='{.data.ADMIN_PASSWORD}' | base64 -d
```

## CRD Reference

### IdentityServerCluster (`isc`)

| Field | Type | Description |
|---|---|---|
| `version` | string, required | Curity Identity Server version |
| `image` | string | Override container image (for private mirrors) |
| `imagePullSecret` | string | Secret name for pulling images |
| `adminCredentials` | object | Secret ref with `ADMIN_PASSWORD`, `CONFIG_ENCRYPTION_KEY`, `KEYSTORE_PASSWORD`; auto-generated if omitted |
| `logging` | object | Log level, stdout tailing, sidecar config |
| `resources` | object | Default CPU/memory requests/limits |
| `probes` | object | Default liveness/readiness probe config |
| `autoscaling` | object | HPA defaults (minReplicas, maxReplicas, targetCPU) |
| `podDisruptionBudget` | object | PodDisruptionBudget default. `minAvailable` accepts integer (`2`) or percentage (`"50%"`). Runtime nodes only; ignored on admin nodes with a Warning event |
| `podAnnotations` | map | Applied to all managed pods |
| `podLabels` | map | Applied to all managed pods |
| `nodeSelector` | map | Pod scheduling constraints |
| `tolerations` | list | Pod toleration specs |
| `topologySpreadConstraints` | list | Pod spread policies |
| `affinity` | object | Advanced scheduling constraints |

### IdentityServerNode (`isn`)

| Field | Type | Description |
|---|---|---|
| `type` | enum: `admin`/`runtime`, required | Node type; only one admin per cluster |
| `role` | string, required | Unique node identifier within the cluster |
| `identityServerClusterRef` | object, required | `{name: "<cluster>"}` reference |
| `replicas` | int32, default: 1 | Deployment replicas; forced to 1 for admin |
| `ui` | object | Admin UI config (see [Accessing the Admin UI](#accessing-the-admin-ui)) |
| `service` | object | Service `type` (ClusterIP/LoadBalancer/NodePort) and `port` |
| `environmentVariables` | list | Standard Kubernetes env vars |
| `resources` | object | Overrides cluster-level resources |
| `probes` | object | Overrides cluster-level probes |
| `logging` | object | Overrides cluster-level logging |
| `autoscaling` | object | HPA configuration |
| `podDisruptionBudget` | object | PodDisruptionBudget; node overrides cluster. `minAvailable` accepts integer or percentage string. Ignored on admin nodes (Warning event `PDBIgnored`) |
| `podAnnotations` | map | Merges with cluster-level annotations |
| `podLabels` | map | Merges with cluster-level labels |
| `nodeSelector` | map | Overrides cluster-level nodeSelector |
| `tolerations` | list | Overrides cluster-level tolerations |
| `topologySpreadConstraints` | list | Overrides cluster-level topology |
| `affinity` | object | Overrides cluster-level affinity |

## Configuration Management

The operator discovers ConfigMaps and Secrets labeled `curity.io/managed: "true"` in the same namespace as the cluster. These are validated, then mounted into the Curity pods.

### Creating a Managed Config

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: curity-base-config
  namespace: demo
  labels:
    curity.io/managed: "true"
data:
  base-config.xml: |
    <config xmlns="http://tail-f.com/ns/config/1.0">
      <environments xmlns="https://curity.se/ns/conf/base">
        <environment>
          <base-url>https://localhost:8443</base-url>
        </environment>
      </environments>
    </config>
```

### Config Types

Set the `curity.io/config-type` annotation to control where configs are mounted. If omitted, the operator defaults it to `base`.

| Type | Annotation Value | Mount Path |
|---|---|---|
| Base config | `base` (default) | `/opt/idsvr/etc/init/{kind}_{resource-name}_{filename}` |
| License | `license` | `/opt/idsvr/etc/init/license/{kind}_{resource-name}_{filename}` |

Mount filenames are prefixed with the resource kind and name to prevent collisions when multiple ConfigMaps/Secrets contain the same data key. The `{kind}` prefix is `cm` for ConfigMaps and `secret` for Secrets (e.g. `cm_my-config_base-config.xml`).

### Validation

When managed configs are created or updated, the operator runs a validation Job that starts an isolated Curity instance to verify the config. The status is visible on the cluster:

```bash
kubectl get isc my-cluster -o jsonpath='{.status.conditions[?(@.type=="ConfigValidationReady")]}'
```

And on each node:

```bash
kubectl get isn admin -o jsonpath='{.status.appliedConfigs}'
```

**What validation catches:**
- Malformed XML (not well-formed)
- Unknown or invalid XML schema elements

**What validation does not catch:**
- Semantic errors that only surface in full cluster mode (e.g., HTTPS service role without an SSL key configured)

If validation fails, the `ConfigValidationReady` condition on the cluster shows the reason. Configs are **not** mounted until validation passes. Fix the config content to retry automatically, or delete the validation Job manually for transient infrastructure failures.

### Admin Routing

Config volumes are mounted based on whether an admin node exists:

- **With admin node**: Only the admin pod gets the config volumes. Runtime pods do not.
- **Without admin node** (runtime-only cluster): All runtime pods get the config volumes.

### Namespace Scoping

All managed configs in a namespace are discovered by all clusters in that namespace. To scope configs to a specific cluster, use separate namespaces.

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

## Cleanup

```bash
make undeploy        # Remove the operator from the cluster (Kustomize)
make undeploy-helm   # Remove the operator from the cluster (Helm)
make uninstall       # Remove CRDs
make cluster-destroy # Delete the Kind cluster
```

Run `make help` to see all available targets.
