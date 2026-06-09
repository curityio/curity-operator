# Manual Testing Guide

A walkthrough for building, running, and exercising the operator by hand — locally on Kind and against a remote/public cluster. For automated unit and E2E suites, see the `Running Tests` section in [README.md](README.md).

---

## Prerequisites

- Go 1.25+
- A working Docker daemon (Docker Desktop, Colima, or equivalent)
- `kubectl`
- Helm 3.x

`kustomize`, `controller-gen`, `kind`, `helmify`, etc. are downloaded into `bin/` automatically by the Makefile.

---

## 1. Run on Kind (local)

A single command builds the image, creates the Kind cluster, loads the image, and deploys the operator via Helm:

```bash
make deploy-helm
```

This is idempotent — re-running picks up code changes, rebuilds the image, reloads it into Kind, and runs `helm upgrade`. Kind cluster name: `e2e-test-cluster`.

### Verify the operator is running

```bash
kubectl --context kind-e2e-test-cluster -n curity-operator get pods,deploy
kubectl get crd | grep curity
```

Expected: `curity-operator-controller-manager` pod `2/2 Running`, plus `identityserverclusters.curity.io` and `identityservernodes.curity.io` CRDs.

### Alternative: Kustomize instead of Helm

```bash
make deploy-kind                          # build + create Kind + load image
make install                              # apply CRDs
kubectl create namespace curity-operator
make kustomize-deploy                     # apply manager + RBAC
```

### Tear it down

```bash
make undeploy-helm        # or: make undeploy
make uninstall            # remove CRDs
make cluster-destroy      # delete the Kind cluster
```

---

## 2. Run on a cluster

`make deploy-helm` is Kind-only. For a real cluster, install the published chart and image from GHCR.

Point `kubectl` at the target cluster first (your user needs cluster-admin for CRDs + RBAC + namespaces):

```bash
kubectl config use-context <my-cluster>
kubectl config current-context        # verify
```

### Install the published chart from GHCR

Installs a tagged release from `oci://ghcr.io/curityio/charts/curity-operator` with the matching image at `ghcr.io/curityio/curity-operator`. No local build required.

> **Note:** The chart and image are currently published as **private** GHCR packages. You need a GitHub Personal Access Token (PAT) with `read:packages` scope (create one at <https://github.com/settings/tokens>) for both the Helm login and the pull secret below.

**Steps:**

```bash
# 1. Authenticate Helm to ghcr.io
export GHCR_USERNAME=<github-username>
read -s GHCR_TOKEN                          # paste PAT, hidden input, press enter
export GHCR_TOKEN
echo "$GHCR_TOKEN" | helm registry login ghcr.io -u "$GHCR_USERNAME" --password-stdin

# 2. Create the operator namespace
kubectl create namespace curity-operator

# 3. Create the image pull secret
kubectl create secret docker-registry ghcr-pull \
  --docker-server=ghcr.io \
  --docker-username="$GHCR_USERNAME" \
  --docker-password="$GHCR_TOKEN" \
  -n curity-operator

# 4. Install from the OCI chart
helm install curity-operator \
  oci://ghcr.io/curityio/charts/curity-operator \
  --version 0.0.1 \
  --namespace curity-operator \
  --set 'imagePullSecrets[0].name=ghcr-pull'
```

### Verify the install

```bash
kubectl -n curity-operator get pods,deploy
kubectl get crd | grep curity
kubectl -n curity-operator logs deploy/curity-operator-controller-manager -c manager --tail=50
```

Expected: `curity-operator-controller-manager` pod `2/2 Running`, plus `identityserverclusters.curity.io` and `identityservernodes.curity.io` CRDs.

### Tear down the remote install

```bash
helm uninstall curity-operator -n curity-operator
kubectl delete namespace curity-operator
```

`helm uninstall` does **not** delete the Curity CRDs — they ship with `helm.sh/resource-policy: keep` (gated by `crds.keep=true`, default). Existing `IdentityServerCluster` and `IdentityServerNode` resources survive a reinstall. To delete the CRDs and cascade-delete every CR cluster-wide:

```bash
kubectl delete crd identityserverclusters.curity.io identityservernodes.curity.io
```

To opt out of the keep behavior at install time (test/throwaway clusters only):

```bash
helm install ... --set crds.keep=false
```

---

## 3. Set up Curity resources

Once the operator is running, exercise it by creating an `IdentityServerCluster` and one or more `IdentityServerNode`s in an application namespace.

### 3a. Application namespace

```bash
kubectl create namespace demo
```

### 3b. IdentityServerCluster

Minimal — admin credentials are auto-generated:

```yaml
# isc.yaml
apiVersion: curity.io/v1alpha1
kind: IdentityServerCluster
metadata:
  name: my-cluster
  namespace: demo
spec:
  version: "11.0"
```

```bash
kubectl apply -f isc.yaml
kubectl -n demo get isc
```

The operator creates a Secret `my-cluster-admin-creds` with `ADMIN_PASSWORD` and `CONFIG_ENCRYPTION_KEY`.

To use a pre-existing Secret instead, set `spec.adminCredentials` — see [README §Create a Curity Identity Server Deployment](README.md#create-a-curity-identity-server-deployment).

### 3c. Admin IdentityServerNode (with UI)

```yaml
# admin.yaml
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

```bash
kubectl apply -f admin.yaml
```

Only one admin node per cluster is allowed; replicas must not be set on an admin node (rejected at admission) — the admin is always 1.

### 3d. Runtime IdentityServerNode

```yaml
# runtime.yaml
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

```bash
kubectl apply -f runtime.yaml
```

### 3e. Verify reconciliation

```bash
kubectl -n demo get isc,isn
kubectl -n demo get deploy,svc,pod
kubectl -n demo describe isc my-cluster | tail -40   # status conditions
kubectl -n demo describe isn admin | tail -40
```

Expected: `admin` and `runtime` Deployments, matching Services, pods Running and Ready.

### 3f. Access the admin UI

```bash
kubectl -n demo port-forward svc/admin 6749:6749
```

Open `http://localhost:6749/admin` (or `https://` if `ui.secure: true`). Username: `admin`. Password:

```bash
kubectl -n demo get secret my-cluster-admin-creds \
  -o jsonpath='{.data.ADMIN_PASSWORD}' | base64 -d
```

---

## 4. Set up managed ConfigMaps and Secrets

The operator discovers `ConfigMap`s and `Secret`s in the cluster's namespace that carry the label `curity.io/managed: "true"` and mounts them into the Identity Server pods. The annotation `curity.io/config-type` controls the mount path.

### 4a. Base XML config (ConfigMap)

```yaml
# base-config.yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: curity-base-config
  namespace: demo
  labels:
    curity.io/managed: "true"
  # annotations:
  #   curity.io/config-type: "base"   # optional; "base" is the default
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

```bash
kubectl apply -f base-config.yaml
```

### 4b. License (Secret)

```yaml
# license.yaml
apiVersion: v1
kind: Secret
metadata:
  name: curity-license
  namespace: demo
  labels:
    curity.io/managed: "true"
  annotations:
    curity.io/config-type: "license"
type: Opaque
stringData:
  license.json: |
    <paste license.json contents here>
```

```bash
kubectl apply -f license.yaml
```

### 4c. Scope a config to a specific cluster

```yaml
metadata:
  annotations:
    curity.io/cluster: "my-cluster"   # comma-separated list to scope to multiple clusters
```

When omitted, all clusters in the namespace pick up the resource.

### 4d. Verify mounts and reconciliation

```bash
# Operator-side issue surface (durable, status subresource)
kubectl -n demo describe isc my-cluster | grep -A20 "Managed Resource Issues"

# Per-resource events
kubectl -n demo describe configmap curity-base-config
kubectl -n demo get events --field-selector type=Warning

# Inside the admin pod (configs are routed there when an admin node exists)
kubectl -n demo exec deploy/admin -- ls -la /opt/idsvr/etc/init/
kubectl -n demo exec deploy/admin -- ls -la /opt/idsvr/etc/init/license/
```

Files are prefixed `cm_<name>_…` for ConfigMaps and `secret_<name>_…` for Secrets.

---

## 5. Iterating

Code change → reload the operator:

```bash
make deploy-helm    # rebuilds, reloads into Kind, helm upgrade
kubectl -n curity-operator rollout status deploy/curity-operator-controller-manager
```

For a remote cluster, repeat steps 2-2 and 2-3 (build + push + `helm upgrade`).

To inspect operator logs while reproducing an issue:

```bash
kubectl -n curity-operator logs -f deploy/curity-operator-controller-manager -c manager
```

---

## 6. Cleanup checklist

```bash
# Application resources first (so finalizers can run while the operator is alive)
kubectl delete -f runtime.yaml -f admin.yaml -f isc.yaml --ignore-not-found

# Then the operator
make undeploy-helm     # or helm uninstall, for a remote cluster
make uninstall         # CRDs

# And finally the cluster (Kind only)
make cluster-destroy
```
