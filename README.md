# Curity Operator

A Kubernetes operator that deploys and manages the [Curity Identity Server](https://curity.io/docs/).

Describe the Identity Server cluster you want with a handful of custom resources, and the operator
creates and keeps in sync the Deployments, Services, Secrets, HPAs, PodDisruptionBudgets,
NetworkPolicies and Prometheus ServiceMonitors behind them. Configuration (base config, licence,
log4j2, post-commit scripts) is delivered through ordinary labelled ConfigMaps and Secrets, so it
fits straight into a GitOps workflow.

- [How it works](#how-it-works)
- [Requirements](#requirements)
- [Installation](#installation)
- [Quick start](#quick-start)
- [Accessing the admin UI](#accessing-the-admin-ui)
- [Configuration reference](#configuration-reference)
  - [IdentityServerCluster](#identityservercluster-isc)
  - [IdentityServerNode](#identityservernode-isn)
  - [IdentityServerDatabase](#identityserverdatabase-isdb)
  - [How nodes inherit cluster settings](#how-nodes-inherit-cluster-settings)
- [Identity Server configuration](#identity-server-configuration)
- [TLS keystores](#tls-keystores)
- [Logging](#logging)
- [Observability](#observability)
- [Packages (plugins)](#packages-plugins)
- [Network policy](#network-policy)
- [Troubleshooting](#troubleshooting)
- [Uninstalling](#uninstalling)
- [Getting help](#getting-help)

## How it works

The operator introduces three custom resources in the `curity.io/v1alpha1` API group:

| Kind | Short name | Purpose |
|---|---|---|
| `IdentityServerCluster` | `isc` | Cluster-wide settings: version, credentials, logging, scheduling, packages, observability |
| `IdentityServerNode` | `isn` | An individual node deployment — one `admin` node and any number of `runtime` nodes |
| `IdentityServerDatabase` | `isdb` | A run-once Job that creates or upgrades the Identity Server database schema |

Each node references a cluster and inherits its settings, with optional per-node overrides. From
those resources the operator reconciles the Deployments, Services and Secrets that actually run the
Identity Server, and reports progress on the resources' `status.conditions`.

```
IdentityServerCluster (version, credentials, packages, defaults)
├── IdentityServerNode  type: admin    →  Deployment + Service (+ admin UI)
├── IdentityServerNode  type: runtime  →  Deployment + Service (+ HPA, PDB)
└── IdentityServerNode  type: runtime  →  Deployment + Service (+ HPA, PDB)

ConfigMaps / Secrets labelled curity.io/managed=true  →  mounted into the pods
```

## Requirements

- A Kubernetes cluster, version 1.25 or later (the CRDs use CEL validation rules)
- `kubectl`, and Helm 3.8+ for the Helm installation
- A Curity Identity Server licence — see [curity.io](https://curity.io/) to get one for free
- Optional: the Prometheus Operator, if you want metrics scraped automatically

The operator watches all namespaces and needs cluster-scoped RBAC (it creates Deployments,
Services, Secrets, Jobs, HPAs, PodDisruptionBudgets, NetworkPolicies and ServiceMonitors on your
behalf).

## Installation

### Helm (recommended)

The chart is published as an OCI artifact alongside the operator image on `curity.azurecr.io`,
which allows anonymous pulls — no registry login needed. Check the
[releases page](https://github.com/curityio/curity-operator/releases) for the latest version and
substitute it below.

```bash
helm install curity-operator oci://curity.azurecr.io/charts/curity-operator \
  --version 0.0.1 \
  --namespace curity-operator \
  --create-namespace
```

Useful values:

| Value | Default | Description |
|---|---|---|
| `controllerManager.manager.image.repository` | `curity.azurecr.io/curity/operator` | Operator image; point at a mirror for air-gapped clusters |
| `controllerManager.manager.image.tag` | `v<chart version>` (e.g. `v0.0.1`) | Operator image tag |
| `controllerManager.manager.env.packageFetcherImage` | `alpine:3.19` | Image used by the package init containers, see [Packages](#packages-plugins) |
| `controllerManager.replicas` | `1` | Operator replicas |
| `controllerManager.manager.resources` | 10m/128Mi requests | Operator resource requests and limits |
| `imagePullSecrets` | `[]` | Pull secrets for the operator image |
| `crds.keep` | `true` | Keep the CRDs (and therefore your resources) on `helm uninstall` |

`curity.azurecr.io` allows anonymous pulls, so no pull secret is needed for a default install. If
you mirror the image into a private registry, create a pull secret and reference it:

```bash
kubectl create secret docker-registry operator-pull-secret \
  --docker-server=my-registry.example.com \
  --docker-username=<user> \
  --docker-password=<token> \
  --namespace curity-operator

helm install curity-operator oci://curity.azurecr.io/charts/curity-operator \
  --version 0.0.1 --namespace curity-operator --create-namespace \
  --set controllerManager.manager.image.repository=my-registry.example.com/curity/operator \
  --set imagePullSecrets[0].name=operator-pull-secret
```

### Plain manifests

Every release also ships a single consolidated manifest:

```bash
kubectl apply -f https://github.com/curityio/curity-operator/releases/download/v0.0.1/install.yaml
```

### Verify

```bash
kubectl -n curity-operator get pods
kubectl get crd | grep curity.io
```

## Quick start

This creates a cluster with one admin node and two runtime nodes in a `demo` namespace.

**1. Create the namespace and the cluster resource**

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

When `adminCredentials` is omitted, the operator creates a Secret named `<cluster>-admin-creds`
with generated values for `ADMIN_PASSWORD` and `CONFIG_ENCRYPTION_KEY`. To use a Secret you already
manage, set it explicitly:

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
```

**2. Create the admin node**

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
  # admin is single-active: replicas must not be set (always 1)
  ui:
    enabled: true
    secure: false
  service:
    type: ClusterIP
    port: 6749
```

**3. Create the runtime nodes**

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

**4. Check what was created**

```bash
kubectl -n demo get isc          # IdentityServerClusters
kubectl -n demo get isn          # IdentityServerNodes
kubectl -n demo get deployments  # Deployments created by the operator
kubectl -n demo get services     # Services created by the operator
```

`kubectl get isc` shows the version, readiness, node counts and the number of managed-resource
issues; `kubectl get isn` shows type, role, readiness and replica counts.

**5. Add your configuration and licence** — see
[Identity Server configuration](#identity-server-configuration).

## Accessing the admin UI

The admin UI is **disabled by default**. Set `ui.enabled: true` on the admin node to enable it.

```bash
kubectl -n demo port-forward svc/admin 6749:6749
```

- HTTPS (default): `https://localhost:6749/admin`
- HTTP (`ui.secure: false`): `http://localhost:6749/admin`

Log in as `admin`. If the credentials were auto-generated, read the password from the Secret:

```bash
kubectl -n demo get secret my-cluster-admin-creds -o jsonpath='{.data.ADMIN_PASSWORD}' | base64 -d
```

## Configuration reference

### IdentityServerCluster (`isc`)

| Field | Type | Description |
|---|---|---|
| `version` | string, required | Curity Identity Server version |
| `image` | string | Override container image (for private mirrors) |
| `imagePullSecret` | string | Secret name for pulling images |
| `fipsMode` | bool | Enable FIPS mode (`--fips-mode`). Requires a special licence and a FIPS-enabled image; contact sales@curity.io. Defaults to `false` |
| `adminCredentials` | object | Secret ref with `ADMIN_PASSWORD`, `CONFIG_ENCRYPTION_KEY`; auto-generated if omitted |
| `logging` | object | Log level, log streams to tail, sidecar config |
| `resources` | object | Default CPU/memory requests/limits |
| `probes` | object | Default liveness/readiness probe config |
| `imagePullPolicy` | enum: `Always`/`Never`/`IfNotPresent` | Pull policy for the main Curity container. Unset → tag-aware default (`Always` for `:latest`/untagged, else `IfNotPresent`) |
| `terminationGracePeriodSeconds` | int64 (0–3600) | Pod shutdown grace period (default `30`); raise (60–180) for Curity's JVM drain |
| `securityContext` | object | Pod-level security context; **merged** over the operator's required defaults (`runAsUser 10001`, `runAsGroup`/`fsGroup 10000`) so omitting a field never strips the required UID/GID |
| `containerSecurityContext` | object | Security context for the main Curity container (privilege, capabilities, etc.) |
| `initContainers` | list | Your init containers, **appended** (never override operator-managed containers); name `curity` reserved; max 16 |
| `extraContainers` | list | Your sidecar containers, **appended** (never override operator-managed containers); same naming rules as `initContainers`; max 16 |
| `networkPolicy` | object | Operator-managed NetworkPolicy protecting the cluster's admin node (cluster-scoped, opt-in by presence). See [Network policy](#network-policy) |
| `autoscaling` | object | HPA defaults (minReplicas, maxReplicas, targetCPU) |
| `podDisruptionBudget` | object | PodDisruptionBudget default. Set **exactly one** of `minAvailable`/`maxUnavailable`, each an integer (`2`) or percentage (`"50%"`). Runtime nodes only; ignored on admin nodes with a Warning event |
| `podAnnotations` | map | Applied to all managed pods |
| `podLabels` | map | Applied to all managed pods |
| `nodeSelector` | map | Pod scheduling constraints |
| `tolerations` | list | Pod toleration specs |
| `topologySpreadConstraints` | list | Pod spread policies |
| `affinity` | object | Advanced scheduling constraints |
| `packages` | list | Remote ZIP archives downloaded and unpacked into every Curity container at startup. See [Packages](#packages-plugins) |
| `convertKeystore` | list | Convert `kubernetes.io/tls` Secrets to base64 PKCS#12 keystores, published as env vars (e.g. `SSL_SERVER_KEY`) on every node. See [TLS keystores](#tls-keystores) |
| `observability.serviceMonitor` | object | Prometheus `ServiceMonitor` for the cluster's metrics. **On by default** where the Prometheus Operator is installed. See [Observability](#observability) |

### IdentityServerNode (`isn`)

| Field | Type | Description |
|---|---|---|
| `type` | enum: `admin`/`runtime`, required | Node type; only one admin per cluster |
| `role` | string, required | Unique node identifier within the cluster |
| `identityServerClusterRef` | object, required | `{name: "<cluster>"}` reference |
| `replicas` | int32 (runtime only) | Runtime Deployment replicas; omitted defaults to 1. Must not be set on admin nodes — rejected at admission; the admin is always 1 |
| `ui` | object | Admin UI config (see [Accessing the admin UI](#accessing-the-admin-ui)) |
| `skipInstall` | bool (admin only) | When `true`, passes `SKIP_INSTALL=1` so the admin starts without first-run setup. Set at bootstrap; provide config another way. Rejected on runtime nodes |
| `service` | object | Service `type` (ClusterIP/LoadBalancer/NodePort) and `port` |
| `environmentVariables` | list | Standard Kubernetes env vars |
| `resources` | object | Overrides cluster-level resources |
| `probes` | object | Overrides cluster-level probes |
| `logging` | object | Overrides cluster-level logging |
| `imagePullPolicy` | enum: `Always`/`Never`/`IfNotPresent` | Pull policy for the main Curity container; overrides cluster. Unset → tag-aware default |
| `terminationGracePeriodSeconds` | int64 (0–3600) | Pod shutdown grace period (default `30`); overrides cluster |
| `securityContext` | object | Pod-level security context; merged over operator defaults (UID/GID preserved); overrides cluster |
| `containerSecurityContext` | object | Security context for the main Curity container; overrides cluster |
| `initContainers` | list | Your init containers, appended after package fetchers; `curity` reserved; max 16; overrides cluster |
| `extraContainers` | list | Your sidecar containers, appended after log sidecars; `curity` reserved; max 16; overrides cluster |
| `autoscaling` | object | HPA configuration |
| `podDisruptionBudget` | object | PodDisruptionBudget; node overrides cluster. Set **exactly one** of `minAvailable`/`maxUnavailable` (integer or percentage string). Ignored on admin nodes (Warning event `PDBIgnored`) |
| `podAnnotations` | map | Merges with cluster-level annotations |
| `podLabels` | map | Merges with cluster-level labels |
| `nodeSelector` | map | Merges with cluster-level nodeSelector (node keys win) |
| `tolerations` | list | Overrides cluster-level tolerations (`[]` clears) |
| `topologySpreadConstraints` | list | Overrides cluster-level topology (`[]` clears) |
| `affinity` | object | Overrides cluster-level affinity |

> **Pod customization** fields (`initContainers`, `extraContainers`, `securityContext`,
> `containerSecurityContext`, `terminationGracePeriodSeconds`, `imagePullPolicy`) live on both
> specs. Your `initContainers`/`extraContainers` are appended after the operator's own containers
> (never overriding them). A container that omits its own `imagePullPolicy` gets the same tag-aware
> default as the main container (`Always` for `:latest`/untagged, else `IfNotPresent`).
>
> Avoid the container names the operator generates: **`curity`** (the main container — rejected at
> admission), **`package-fetch-<n>`** (one per `spec.packages`), and **one per
> `spec.logging.logs` entry** (the log sidecars, e.g. `request`). `curity` is blocked by CEL; the
> dynamic ones (package/log) can't be — a collision is caught at Deployment creation and surfaced
> as `Degraded=InvalidSpec` with a message naming the conflicting source.

### IdentityServerDatabase (`isdb`)

Manages the Identity Server database schema. Creating one triggers a **run-once Job** that executes
`/opt/idsvr/bin/idsvr -I` using the **same container image as the referenced
`IdentityServerCluster`**. The Job is re-run whenever the resolved cluster image (a `version` bump
or `image` override) **or** this resource's own spec changes — handy for re-pointing at a new
database.

| Field | Type | Description |
|---|---|---|
| `identityServerClusterRef` | object, required | `{name: "<cluster>"}` reference (same namespace). Supplies the image; a change to its `version`/`image` re-triggers the Job |
| `connection` | object, required | JDBC settings projected onto `JDBC_URL`, `JDBC_USERNAME`, `JDBC_PASSWORD`. See below |
| `jobTemplate` | object | Standard Kubernetes Job/Pod settings for the schema-management Job (all optional). See below |

**`connection`** — JDBC settings may be set inline, per-field from a Secret, or sourced wholesale
from one Secret:

| Field | Type | Description |
|---|---|---|
| `secretRef` | string | Name of a Secret whose `JDBC_URL`/`JDBC_USERNAME`/`JDBC_PASSWORD` keys are loaded as env vars (`envFrom`, optional). Use it to keep the whole connection in one Secret |
| `url` | object | Sets `JDBC_URL`. **Required unless `secretRef` is set** (enforced at admission). `{value: "..."}` or `{valueFrom: {secretKeyRef: {...}}}` |
| `username` | object | Sets `JDBC_USERNAME`. Optional. Same value/valueFrom shape |
| `password` | object | Sets `JDBC_PASSWORD`. Optional; prefer `valueFrom.secretKeyRef` over an inline value |

A `ValueSource` (`url`/`username`/`password`) must set **exactly one** of `value`/`valueFrom`.
Per-field settings are applied after `secretRef`, so an explicit field **overrides** the matching
key from `secretRef`. When `secretRef` is the only URL source and the referenced Secret has no
`JDBC_URL` key, the operator does not launch a Job and reports `Ready=False` with reason
`JDBCURLMissing`.

<details>
<summary><b><code>jobTemplate</code></b> — every field optional</summary>

| Field | Type | Description |
|---|---|---|
| `resources` | object | CPU/memory requests/limits on the `idsvr` container |
| `securityContext` | object | Pod-level security context; merged over operator defaults (`runAsUser 10001`, `runAsGroup`/`fsGroup 10000`) |
| `containerSecurityContext` | object | Security context for the `idsvr` container |
| `serviceAccountName` | string | ServiceAccount for the pod |
| `automountServiceAccountToken` | bool | Defaults to `false` (no Kubernetes API access needed) |
| `imagePullPolicy` | enum: `Always`/`Never`/`IfNotPresent` | Pull policy; unset → tag-aware default |
| `imagePullSecret` | string | Image pull Secret; unset falls back to the cluster's `imagePullSecret` |
| `env` | list | Extra env vars, appended after the `JDBC_*` vars. Redefining a `JDBC_*` var is rejected at admission |
| `backoffLimit` | int32 (0–100) | Job retries before it is marked failed (default `3`) |
| `activeDeadlineSeconds` | int64 | Hard time limit for the Job |
| `ttlSecondsAfterFinished` | int32 | Garbage-collect the finished Job after this many seconds |
| `nodeSelector` / `tolerations` / `affinity` / `topologySpreadConstraints` | map/list/object | Pod scheduling constraints |
| `priorityClassName` | string | Pod PriorityClass |
| `terminationGracePeriodSeconds` | int64 (0–3600) | Pod shutdown grace period |
| `podAnnotations` / `podLabels` | map | Applied to the Job's pod template (operator-owned `curity.io/*` keys rejected in `podLabels`) |

</details>

Status conditions: `Complete` (Job succeeded), `Failed` (Job exhausted retries), `Progressing`
(running), and `Ready` (mirrors `Complete`). The Job is owned by the `IdentityServerDatabase`, so
deleting the resource removes the Job.

Minimal example (inline URL, password from a Secret):

```yaml
apiVersion: curity.io/v1alpha1
kind: IdentityServerDatabase
metadata:
  name: acct-schema
  namespace: curity
spec:
  identityServerClusterRef:
    name: demo
  connection:
    url:
      value: "jdbc:postgresql://postgres:5432/curity"
    username:
      value: curity
    password:
      valueFrom:
        secretKeyRef:
          name: db-credentials
          key: password
```

Whole connection from one Secret (the Secret holds `JDBC_URL`, `JDBC_USERNAME`, `JDBC_PASSWORD`):

```yaml
spec:
  identityServerClusterRef:
    name: demo
  connection:
    secretRef: db-connection
  jobTemplate:
    serviceAccountName: db-migrator
    resources:
      requests:
        cpu: 250m
        memory: 256Mi
    containerSecurityContext:
      readOnlyRootFilesystem: true
      allowPrivilegeEscalation: false
```

### How nodes inherit cluster settings

Most cluster-level settings apply to every node and can be overridden per node. How a node value
combines with the cluster value depends on the field's type:

- **Maps merge** — `nodeSelector`, `podLabels`, `podAnnotations`: cluster and node keys are
  combined, with the node winning on conflicting keys.
- **Everything else replaces** — `resources`, `probes`, `logging`, `affinity`, `autoscaling`,
  `podDisruptionBudget`, `securityContext`, `containerSecurityContext`,
  `terminationGracePeriodSeconds`, `imagePullPolicy`, `initContainers`, `extraContainers`,
  `tolerations`, `topologySpreadConstraints`: when a node sets the field it supplies the **whole**
  value (no field-level merge with the cluster); when a node omits it, the cluster value is
  inherited.
- **An explicit empty list clears** — for the list overrides (`initContainers`, `extraContainers`,
  `tolerations`, `topologySpreadConstraints`), setting `[]` on the node drops the inherited cluster
  list, whereas omitting the field inherits it.

`securityContext` is replaced like the rest; the operator then applies its required
`runAsUser 10001` / `runAsGroup`/`fsGroup 10000` floor to any of those three the value leaves
unset, so a node override can't strip the UID/GID the Curity image needs.

Cluster-only fields — `version`, `image`, `imagePullSecret`, `adminCredentials`, `packages`,
`networkPolicy` — have no node-level override.

## Identity Server configuration

The operator discovers ConfigMaps and Secrets labelled `curity.io/managed: "true"` in the same
namespace as the cluster, validates them, and mounts them into the Curity pods. This is how you
deliver base configuration, your licence, a log4j2 file, post-commit scripts and environment
variables.

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

### Config types

Set the `curity.io/config-type` annotation to control where a config is mounted. If omitted, it is
treated as `base`. The operator never modifies the annotation.

| Type | Annotation value | Mount path | Mounted on |
|---|---|---|---|
| Base config | `base` (default) | `/opt/idsvr/etc/init/{kind}_{resource-name}_{filename}` | Admin (all nodes if no admin) |
| Licence | `license` | `/opt/idsvr/etc/init/license/{kind}_{resource-name}_{filename}` | Admin (all nodes if no admin) |
| Logging | `logging` | `/opt/idsvr/etc/log4j2.xml` (single file; replaces the shipped default) | Every node |
| Post-commit script | `postCommitScript` | `/opt/idsvr/usr/bin/post-commit-scripts/{kind}_{resource-name}_{filename}` (executable) | Admin only |
| Environment variables | `env` | injected via `envFrom` — no file mount | Every node |

Mount filenames are prefixed with the resource kind and name to prevent collisions when multiple
ConfigMaps/Secrets contain the same data key. The `{kind}` prefix is `cm` for ConfigMaps and
`secret` for Secrets (e.g. `cm_my-config_base-config.xml`). The `logging` type is the exception —
it mounts a single `log4j2.xml` at a fixed path with no mangling.

A resource with an unknown `curity.io/config-type` value is skipped (not mounted) and the operator
emits an `UnknownConfigType` Warning event on the offending ConfigMap/Secret itself. Other managed
resources in the namespace are unaffected.

### Where configs land

Where a config mounts depends on its type and whether an admin node exists:

- **`base` / `license`** — the admin pod only when an admin node exists; all runtime pods in a
  runtime-only cluster.
- **`logging`** — every node (admin and runtime).
- **`postCommitScript`** — the admin node only; never on runtime, and nothing in a runtime-only
  cluster (post-commit scripts run only where ConfigD runs).

### Post-commit scripts

A `postCommitScript`-typed ConfigMap (or Secret) mounts each data key as an **executable** file
(`0755`, or `0555` for Secrets) into `/opt/idsvr/usr/bin/post-commit-scripts/` on the **admin node
only**. Curity runs these after a configuration commit — the operator's role is to deliver them
executable to the right place. Editing a script rolls the admin pod so the new content is picked
up. Stream their output with `spec.logging.logs: [post-commit-scripts]`.

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: provision-hooks
  labels:
    curity.io/managed: "true"
  annotations:
    curity.io/config-type: postCommitScript
data:
  notify.sh: |
    #!/bin/sh
    echo "config committed at $(date)"
```

If a `postCommitScript` is applied to a cluster with no admin node, it mounts nowhere — the
operator emits a `PostCommitScriptNoAdmin` Warning event on the IdentityServerCluster.

### Namespace scoping

All managed configs in a namespace are discovered by all clusters in that namespace. To scope
configs to a specific cluster, use separate namespaces.

## TLS keystores

Curity expects its TLS server key in the `SSL_SERVER_KEY` environment variable as a
**base64-encoded PKCS#12 keystore**. A Kubernetes `kubernetes.io/tls` Secret gives you **PEM**
(`tls.crt` + `tls.key`) instead — the wrong format.

`spec.convertKeystore` on the `IdentityServerCluster` bridges that gap:

1. The operator reads each source TLS Secret and converts it to a PKCS#12 keystore in-process
   (no Job, no extra pod).
2. It writes the results into one cluster-owned Secret, `<cluster>-convert-ks-env`
   (config-type `env`).
3. That Secret is injected via `envFrom` onto every node, so `SSL_SERVER_KEY` is set in each
   Curity pod.

```yaml
apiVersion: curity.io/v1alpha1
kind: IdentityServerCluster
metadata:
  name: my-cluster
spec:
  version: "11.0.0"
  convertKeystore:
    - sourceTls:
        keyName: SSL_SERVER_KEY        # env var that receives the keystore
        fromSecretRef: my-tls          # source kubernetes.io/tls Secret (same namespace)
        cert: SSL_SERVER_CERT          # optional: also publish the raw PEM cert
```

Then reference `${SSL_SERVER_KEY}` from your base configuration (the `ssl-server-keystore` element)
so Curity serves it. Notes:

- Conversion re-runs automatically when the source certificate rotates.
- A node's Deployment is deferred until its keystore is published, so no pod boots referencing a
  missing `${SSL_SERVER_KEY}`.
- Status is on the cluster's `ConvertKeystoreReady` condition: `SourceSecretMissing` /
  `SourceKeysMissing` (waiting on the source), `ConversionFailed` (bad cert — nothing published),
  or `Ready`.
- There is no keystore password to manage: the operator emits the keystore in exactly the form
  Curity loads, so you never supply one.

## Logging

`spec.logging` (on the cluster, overridable per node) controls the Curity server log level and
optional log-to-stdout streaming.

```yaml
spec:
  logging:
    level: INFO          # ERROR, WARN, INFO, DEBUG, TRACE, OFF (default INFO)
    logs:                # a non-empty list enables one stdout-tailing sidecar per stream
      - request
      - audit
```

- **`level`** sets the server log level (delivered as the `LOGGING_LEVEL` env var). `OFF`
  additionally suppresses the log sidecars and the shared log volume.
- **`logs`** drives the sidecars: a **non-empty list** adds one lightweight sidecar container per
  stream, each tailing the matching file from `/opt/idsvr/var/log/` so it appears in
  `kubectl logs <pod> -c <log-name>`. An empty or omitted list (or `level: OFF`) means no sidecars.
  Allowed values: `audit`, `request`, `cluster`, `confsvc`, `confsvc-internal`,
  `post-commit-scripts`.
- Node-level `logging` overrides cluster-level entirely (not merged).
- To replace Curity's log4j2 configuration wholesale, mount a `logging` config-type ConfigMap —
  see [Config types](#config-types).

## Observability

Every Curity node exposes Prometheus metrics on port `4466` at `/metrics`. The operator can manage
a Prometheus Operator
[`ServiceMonitor`](https://prometheus-operator.dev/docs/api-reference/api/#monitoring.coreos.com/v1.ServiceMonitor)
that scrapes the metrics endpoint of **all** nodes in the cluster (admin and runtime).

**Scraping is on by default.** Wherever the Prometheus Operator (the `monitoring.coreos.com` CRDs)
is installed, the operator creates one `ServiceMonitor` per cluster — you don't have to configure
anything. Where the CRD is **not** installed, the feature is a silent no-op (no resource, no error).

```yaml
spec:
  observability:
    serviceMonitor:
      enabled: true                    # default true; set false to opt out
      interval: 30s                    # scrape interval (default 30s)
      labels:                          # added to the ServiceMonitor metadata
        release: kube-prometheus-stack # so a label-selecting Prometheus picks it up
```

- **`enabled`** — `true` by default; omit the whole block to keep scraping on. Set
  `enabled: false` to disable and delete the ServiceMonitor for this cluster.
- **`labels`** — extra labels on the ServiceMonitor. A Prometheus deployed by the Prometheus
  Operator only scrapes ServiceMonitors matching its `serviceMonitorSelector`.
  `kube-prometheus-stack` defaults that selector to `release: <helm-release-name>`, so you usually
  need a matching `labels` entry; a Prometheus with an empty selector needs none. Keys in the
  operator-owned namespaces `curity.io/*` and `app.kubernetes.io/*` are rejected; removing a key
  later does not strip it from an existing ServiceMonitor.
- **`interval`** — Prometheus scrape interval (`30s`, `1m`, …).

The operator owns the ServiceMonitor (it is garbage-collected when the cluster is deleted) and
selects node Services by the `curity.io/cluster=<name>` label, so nodes added or removed later are
picked up automatically. The created ServiceMonitor's name is reported in
`status.serviceMonitorName`.

```bash
# See the ServiceMonitor the operator created
kubectl get servicemonitor -n <ns> -l app.kubernetes.io/managed-by=curity-operator

# Or read its name from the cluster status
kubectl get isc <cluster> -n <ns> -o jsonpath='{.status.serviceMonitorName}'
```

> **Prerequisite:** the Prometheus Operator must be installed (it provides the `ServiceMonitor`
> CRD). If you enable scraping explicitly (`enabled: true`) without it, the cluster emits a
> `ServiceMonitorCRDMissing` Warning event; with the default-on behaviour and no CRD, the operator
> stays silent. Installing the Prometheus Operator after the fact requires restarting the operator
> pod (CRD discovery happens at startup).

## Packages (plugins)

`spec.packages` declares remote ZIP archives that the operator downloads and unpacks into every
Curity container at startup. Typical use: shipping plugin JARs without baking them into a custom
image.

Each entry produces one init container per pod. The init container downloads the archive, unzips it
into an `emptyDir` volume, and the main Curity container mounts that volume at the configured
`mountPath`. Removing an entry removes its init container and volume on the next reconcile,
triggering a rolling restart.

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

- **`tls.skipVerify: true`** disables certificate verification — non-production only. Mutually
  exclusive with `tls.ca` / `tls.clientCert` (rejected at admission, since `skipVerify=true` would
  otherwise silently drop both).
- **`tls.enabled` defaults to `false`**; the rest of the TLS block is ignored unless `enabled` is
  `true`.
- **At most one of `auth.basicAuth` / `auth.bearerToken`** per package (rejected at admission).
- **At most 20 packages** per cluster.
- **The init container image is `alpine:3.19`** (the Curity image lacks `curl`/`unzip`). For
  air-gapped clusters that mirror to a private registry, override at operator install time via the
  `PACKAGE_FETCHER_IMAGE` env var on the operator pod — there is no per-CR override. The Helm chart
  exposes it as `controllerManager.manager.env.packageFetcherImage`:
  ```bash
  helm upgrade --install curity-operator oci://curity.azurecr.io/charts/curity-operator \
    --set controllerManager.manager.env.packageFetcherImage=my-registry.example.com/alpine:3.19
  ```
- **Each package volume is capped at 256MiB** (emptyDir `sizeLimit`) to bound zip-bomb /
  disk-full impact.
- **Credentials never appear on `curl` argv** — bearer tokens are piped via stdin, basic auth uses
  a netrc file with mode 0600.

### How the operator reacts to changes

- **Spec change** (URL, auth ref, TLS ref, mount path, order): the `curity.io/packages-hash`
  pod-template annotation changes → rolling restart on the next reconcile.
- **Secret edit while the cluster is healthy** (`PackagesReady=True`): no automatic action. The
  packages hash covers refs, not Secret contents, so running pods keep their cached env value. This
  is intentional — a routine token rotation should not churn pods. Use
  `kubectl rollout restart deployment/<name>` if you need the new value picked up immediately.
- **Secret edit while the cluster is failing** (`PackagesReady=False` with a recoverable reason):
  the operator deletes the failing pod automatically. The fresh pod reads the corrected Secret and
  the cluster recovers in seconds, no manual restart needed.

### Events

| Reason | Type | When |
|---|---|---|
| `PackagesConfigured` | Normal | First time `packages` becomes non-empty for a node |
| `PackagesUpdated` | Normal | `packages-hash` changes (any spec edit) |
| `PackagesRemoved` | Normal | `packages` is cleared from the cluster |

## Network policy

`isc.spec.networkPolicy` is cluster-scoped — there is no `networkPolicy` field on
`IdentityServerNode`. Setting it (even as `{}`) makes the operator create and own a NetworkPolicy
that restricts ingress to the **admin** node: only same-cluster runtime pods may reach the config
and distributed-service ports, plus — when `apiGatewayNamespace` is set and the admin UI is enabled
— that namespace may reach the admin-UI port.

It is **opt-in by presence** (omit it to manage no policy) and only takes effect on a cluster whose
CNI enforces NetworkPolicy.

## Troubleshooting

Start with the resource status — the operator reports everything it knows there.

```bash
kubectl get isc -n <ns>              # Ready, node counts, managed-config issue count
kubectl describe isc <cluster> -n <ns>
kubectl get isn -n <ns>              # per-node Ready / replicas
kubectl describe isn <node> -n <ns>
kubectl logs -n curity-operator -l control-plane=controller-manager --tail=200
```

### Managed ConfigMaps and Secrets

Issues with managed configs surface on two complementary surfaces:

**Warning events on the offending resource** — real-time signal, deduped per resource regardless of
how many clusters share the namespace:

```bash
kubectl describe cm <name>      # or: kubectl describe secret <name>
kubectl get events -n <namespace> --field-selector type=Warning
```

Events fire for every issue type: `UnknownConfigType`, `UnknownClusterInScope`, `EmptyClusterScope`,
`DuplicateConfigKey`. Native Kubernetes event TTL is ~1 hour; events refresh on every reconcile
while the issue persists.

**`status.managedResourceIssues` on the cluster** — a durable per-cluster list of issues affecting
*this cluster's* mount behaviour (a resource was skipped, or a duplicate-key collision could merge
configs). Persists indefinitely and is GitOps-safe (the status subresource is operator-only):

```bash
kubectl get isc                 # the 'Issues' column shows the count
kubectl describe isc <cluster>  # full list with Kind, Name, Reason, Message
```

Scope-only issues (`UnknownClusterInScope`, `EmptyClusterScope`) appear only in events on the
resource — they don't change what mounts on any cluster, so they are not represented in cluster
status. The `PostCommitScriptNoAdmin` warning fires on the **IdentityServerCluster** (not the
ConfigMap): the config is valid, but the cluster has no admin node to run it.

### Packages

Package-fetch failures surface on both `IdentityServerCluster.status.conditions` and
`IdentityServerNode.status.conditions` under a `PackagesReady` condition. Start there before
drilling into pod state.

```bash
# Cluster-level condition first — same view, fewer drills.
kubectl get isc <name> -o jsonpath='{.status.conditions[?(@.type=="PackagesReady")]}{"\n"}'

# Per-node condition (useful when only one node in the cluster failed).
kubectl get isn <node> -o jsonpath='{.status.conditions[?(@.type=="PackagesReady")]}{"\n"}'
```

The `Message` names the failing package index, mountPath, and (for runtime failures) the curl exit
code or kubelet error string. The `Reason` tells you which class of failure occurred:

| Reason | Cause |
|---|---|
| `AllPackagesFetched` | True. Every init container exited 0. |
| `PackageSecretMissing` | A referenced Secret does not exist (or was deleted). |
| `PackageSecretKeyMissing` | The Secret exists, but the named key is absent. |
| `PackageImagePullFailed` | The package-fetcher image (`alpine:3.19` or override) cannot be pulled. |
| `PackageTLSVerifyFailed` | curl exit 60 — the server's TLS cert does not chain to the configured `tls.ca`. |
| `PackageClientCertInvalid` | curl exits 58/82 — the configured `tls.clientCert` Secret data could not be used. |
| `PackageHTTPError` | curl exit 22 — the URL returned 4xx/5xx. |
| `PackageInvalidArchive` | Download succeeded but `unzip` rejected the file (server returned HTML, partial download, corrupt archive). |
| `PackageFetchFailed` | Open-set catch-all (DNS, TCP, timeout, size limit, OOMKilled, future kubelet messages). The Message includes the exit code or verbatim kubelet text. |
| Ready overlay `PackagesNotReady` | Set on the `Ready` condition (delegating to `PackagesReady` for the detail) so `kubectl get` shows the failure at a glance. |

If `PackagesReady` does not give you enough detail, the next step depends on the init container's
state:

- **`Init:CreateContainerConfigError` or `Init:ImagePullBackOff`** — kubelet failed before the
  container ran. The verbatim reason is on the pod, not in logs (the container never produced
  output):
  ```bash
  kubectl describe pod <pod>
  ```
- **`Init:Error` or `Init:CrashLoopBackOff`** — the script ran and exited non-zero. The cause is in
  the container's logs:
  ```bash
  kubectl logs <pod> -c package-fetch-<N>
  ```

<details>
<summary>curl exit codes you might see in <code>PackageFetchFailed</code> messages</summary>

The script uses `curl -fsSL --max-time 120 --max-filesize 268435456`, so the reachable exit codes
are:

| Exit | Meaning |
|---|---|
| 6 | DNS — could not resolve host |
| 7 | Connection refused / failed to connect |
| 22 | HTTP 4xx/5xx (`PackageHTTPError`) |
| 28 | Operation timed out (>120s) |
| 51 | Peer cert verification failed (server-side; ambiguous between hostname mismatch and chain trust) |
| 58 | Could not use client cert file (`PackageClientCertInvalid`) |
| 60 | TLS verify failed against `--cacert` (`PackageTLSVerifyFailed`) |
| 63 | `--max-filesize` (256 MiB) exceeded |
| 77 | CA cert file could not be read |
| 82 | Could not initialize SSL engine (`PackageClientCertInvalid`) |
| 100 | Operator-emitted — `unzip` failed (`PackageInvalidArchive`); the artifact downloaded but is not a valid ZIP |
| 137 | OOMKilled — increase the init container's memory request |

</details>

**Secret deletion in steady state.** If you delete a Secret a package references while the pod is
healthy and serving, the operator stays silent — the pre-check does not re-run for an unchanged
spec, and `PackagesReady` stays True. The existing pod is unaffected (it already fetched its
packages at init time). The condition only flips to False when a *new* pod tries to start and
kubelet fails to project the Secret — typically on a manual `kubectl delete pod`, a node drain, an
OOM eviction, or any future rolling restart.

**Secret data rotation.** Editing a Secret's data in place (same name, same key, new bytes — e.g.
rotating a token) does **not** trigger a rolling restart. The packages hash covers Secret
*references*, not bytes. To force a fetch with new credentials, edit `spec.packages` (e.g. bump
`mountPath`) or `kubectl delete pod`.

**Other useful commands**

```bash
# Confirm the rolled-out hash on the Deployment
kubectl get deployment <owned-name> \
  -o jsonpath='{.spec.template.metadata.annotations.curity\.io/packages-hash}'

# View operator events (PackagesConfigured / PackagesUpdated / PackagesRemoved fire on spec
# transitions; warnings fire on PackagesReady=False transitions).
kubectl describe isn <node-name>
kubectl get events --field-selector reason=PackageSecretMissing
```

## Uninstalling

Delete your Identity Server resources first, so the operator can clean up the workloads it owns:

```bash
kubectl -n demo delete isn --all
kubectl -n demo delete isc --all
```

Then remove the operator:

```bash
helm uninstall curity-operator -n curity-operator
```

The chart sets `crds.keep: true`, so the CRDs survive a `helm uninstall`. Remove them explicitly
when you are done — **this deletes every remaining IdentityServer* resource in the cluster**:

```bash
kubectl delete crd identityserverclusters.curity.io \
                   identityservernodes.curity.io \
                   identityserverdatabases.curity.io
```

## Getting help

- Curity Identity Server documentation: https://curity.io/docs/
- Questions and licence enquiries: sales@curity.io
- Bugs and feature requests: [GitHub issues](https://github.com/curityio/curity-operator/issues)

Working on the operator itself? See [DEVELOPMENT.md](DEVELOPMENT.md) for the build, test and
release workflow, and [MANUAL_TESTING.md](MANUAL_TESTING.md) for a hands-on walkthrough.
