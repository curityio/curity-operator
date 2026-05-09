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
| `packages` | list | Remote ZIP archives downloaded and unpacked into every Curity container at startup. See [Packages](#packages) |

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
  annotations:
    curity.io/config-type: "base"       # optional; defaults to "base"
    curity.io/cluster: "my-cluster"     # optional; scopes to listed clusters
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

Set the `curity.io/config-type` annotation to control where configs are mounted. If omitted, it is treated as `base`. The operator does not modify the annotation.

| Type | Annotation Value | Mount Path |
|---|---|---|
| Base config | `base` (default) | `/opt/idsvr/etc/init/{kind}_{resource-name}_{filename}` |
| License | `license` | `/opt/idsvr/etc/init/license/{kind}_{resource-name}_{filename}` |

Mount filenames are prefixed with the resource kind and name to prevent collisions when multiple ConfigMaps/Secrets contain the same data key. The `{kind}` prefix is `cm` for ConfigMaps and `secret` for Secrets (e.g. `cm_my-config_base-config.xml`).

A resource with an unknown `curity.io/config-type` value is skipped (not mounted) and the operator emits an `UnknownConfigType` Warning event on the offending ConfigMap/Secret itself. Other managed resources in the namespace are unaffected.

### Debugging managed resources

Issues with managed ConfigMaps/Secrets are surfaced through two complementary surfaces:

**Warning Events on the offending resource** — real-time signal, deduped per resource regardless of how many clusters share the namespace:

```bash
kubectl describe cm <name>      # or: kubectl describe secret <name>
kubectl get events -n <namespace> --field-selector type=Warning
```

Events fire for every issue type: `UnknownConfigType`, `UnknownClusterInScope`, `EmptyClusterScope`, `DuplicateConfigKey`. Native Kubernetes Event TTL is ~1 hour; events refresh on every reconcile while the issue persists.

**`status.managedResourceIssues` on the cluster CR** — durable per-cluster list of issues affecting *this cluster's* mount behavior (a resource was skipped, or a duplicate-key collision could merge configs). Persists indefinitely, GitOps-safe (status subresource is operator-only):

```bash
kubectl get isc                 # 'Issues' column shows the count
kubectl describe isc <cluster>  # full list with Kind, Name, Reason, Message
```

Scope-only issues (`UnknownClusterInScope`, `EmptyClusterScope`) appear only in events on the resource — they don't change what mounts on any cluster, so they are not represented in cluster status.

### Admin Routing

Config volumes are mounted based on whether an admin node exists:

- **With admin node**: Only the admin pod gets the config volumes. Runtime pods do not.
- **Without admin node** (runtime-only cluster): All runtime pods get the config volumes.

### Namespace Scoping

All managed configs in a namespace are discovered by all clusters in that namespace. To scope configs to a specific cluster, use separate namespaces.

## Packages

`spec.packages` lets you declare remote ZIP archives that the operator downloads and unpacks into every Curity container at startup. Typical use: shipping plugin JARs without baking them into a custom image.

Each entry produces one init container per pod. The init container downloads the archive, unzips it into an `emptyDir` volume, and the main Curity container mounts that volume at the configured `mountPath`. Removing an entry removes its init container and volume on the next reconcile, triggering a rolling restart.

The full package list is hashed into the pod-template annotation `curity.io/packages-hash` so any spec change (URL, auth ref, TLS ref, mount path, order) triggers a rolling restart. Rotating a referenced Secret's value in place does NOT trigger a restart — the hash covers refs, not contents. Use `kubectl rollout restart deployment/<name>` to force a restart on token rotation.

### Public package (no auth, no TLS customization)

```yaml
apiVersion: curity.io/v1alpha1
kind: IdentityServerCluster
metadata:
  name: demo
spec:
  version: "11.0"
  packages:
    - source:
        url: https://example.com/plugin.zip
      mountPath: /etc/plugins/example
```

### Private package with bearer token

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: pkg-token
type: Opaque
stringData:
  token: "ghp_..."
---
apiVersion: curity.io/v1alpha1
kind: IdentityServerCluster
metadata:
  name: prod
spec:
  version: "11.0"
  packages:
    - source:
        url: https://artifacts.example.com/plugin.zip
        auth:
          bearerToken:
            secretRef:
              name: pkg-token
              key: token
      mountPath: /etc/plugins/oauth-extras
```

### Private endpoint with custom CA + mTLS

```yaml
apiVersion: curity.io/v1alpha1
kind: IdentityServerCluster
spec:
  version: "11.0"
  packages:
    - source:
        url: https://artifacts.internal.example.com/plugin.zip
        tls:
          enabled: true       # gate; rest of block is ignored when false
          ca:
            secretRef:
              name: ca-secret
              key: ca.crt
          clientCert:
            secretRef:
              name: mtls-secret
              key: tls.crt    # private key is read from Secret key tls.key
      mountPath: /etc/plugins/secure
```

### Notes

- **`tls.skipVerify: true`** disables certificate verification — non-production only. Mutually exclusive with `tls.ca` / `tls.clientCert` (rejected at admission, since `skipVerify=true` would otherwise silently drop both).
- **`tls.enabled` defaults to `false`**; the rest of the TLS block is ignored unless `enabled` is `true`.
- **At most one of `auth.basicAuth` / `auth.bearerToken`** per package (rejected at admission).
- **At most 20 packages** per cluster.
- **The init container image is `alpine:3.19`** (the Curity image lacks `curl`/`unzip`). For air-gapped clusters that mirror to a private registry, override at operator deploy time via `PACKAGE_FETCHER_IMAGE` env var on the operator pod — there is no per-CR override.
- **Each package volume is capped at 256MiB** (emptyDir `sizeLimit`) to bound zip-bomb / disk-full impact.
- **Credentials never appear on `curl` argv** — bearer tokens are piped via stdin, basic-auth uses a netrc file with mode 0600.

### Events

| Reason | Type | When |
|---|---|---|
| `PackagesConfigured` | Normal | First time `packages` becomes non-empty for a node |
| `PackagesUpdated` | Normal | `packages-hash` changes (any spec edit) |
| `PackagesRemoved` | Normal | `packages` is cleared from the cluster |

### Debugging packages

```bash
# Confirm the rolled-out hash on the Deployment
kubectl get deployment <owned-name> \
  -o jsonpath='{.spec.template.metadata.annotations.curity\.io/packages-hash}'

# View operator events
kubectl describe isn <node-name>

# Check init container exit state
kubectl describe pod <pod-name>
kubectl logs <pod-name> -c package-fetch-0
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

## Cleanup

```bash
make undeploy        # Remove the operator from the cluster (Kustomize)
make undeploy-helm   # Remove the operator from the cluster (Helm)
make uninstall       # Remove CRDs
make cluster-destroy # Delete the Kind cluster
```

Run `make help` to see all available targets.
