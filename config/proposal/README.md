# Configuration Structure

This directory contains the proposed API for managing Curity Identity Server configuration
with the operator. Rather than listing configuration sources explicitly inside
`IdentityServerCluster` or `IdentityServerNode` (as the idsvr Helm chart does via
`curity.config.configuration`), the operator **discovers** configuration resources
automatically through labels and annotations.

---

## How It Works

The operator watches all `ConfigMap` and `Secret` resources in the cluster namespace and
includes any that carry the managed label:

```yaml
labels:
  curity.io/managed: "true"
```

Once a resource is selected, the operator reads the optional `curity.io/config-type`
annotation to determine which business logic to apply when mounting the configuration.
When omitted, the type defaults to `base`:

```yaml
annotations:
  curity.io/config-type: "<type>"
```

This separation follows the Kubernetes convention of using **labels for selection** and
**annotations for behavioral metadata**.

---

## Config Types

| Type | Mounted to | Notes |
|------|-----------|-------|
| `base` | `/opt/idsvr/etc/init/` | General XML configuration fragments. Each data key becomes a file at that path. Equivalent to `curity.config.configuration[].configMapRef` in the Helm chart. |
| `license` | `/opt/idsvr/etc/init/license/` | License file. Mounted to a dedicated sub-directory under `init` as required by the Identity Server. |
| `logging` | `/opt/idsvr/etc/log4j2.xml` | Single log config file (replaces the shipped default). Every node. |
| `postCommitScript` | `/opt/idsvr/usr/bin/post-commit-scripts/` | Executable scripts Curity runs after a config commit. Admin only. |
| `env` | injected via `envFrom` (no file mount) | Environment variables on every node. Produced by `spec.convertKeystore` (TLS→keystore), or a user-supplied `config-type: env` ConfigMap/Secret. |

> Each type encapsulates the mount path and any special handling so it does not need to be
> repeated across resources.

---

## Converting TLS to keystores (`convertKeystore`)

Curity loads its TLS server key from the `SSL_SERVER_KEY` environment variable as a
base64-encoded PKCS#12 keystore, but Kubernetes TLS Secrets are PEM (`tls.crt` +
`tls.key`). `spec.convertKeystore` on the `IdentityServerCluster` converts each source
`kubernetes.io/tls` Secret into that keystore form in-process and publishes them into
one cluster-owned `env`-typed Secret (`<cluster>-convert-ks-env`), injected via
`envFrom` on every node.

```yaml
spec:
  convertKeystore:
    - sourceTls:
        keyName: SSL_SERVER_KEY        # env var that receives the keystore
        fromSecretRef: my-tls          # source kubernetes.io/tls Secret
        cert: SSL_SERVER_CERT          # optional: also publish the raw PEM cert
```

### Why there is no `keystorePasswordSecretRef`

The Helm chart's `convertKeystore` requires a `keystorePasswordSecretRef`
(`KEYSTORE_PASSWORD`); the operator deliberately omits it. In the Helm chart the
conversion is a two-tool pipeline run by a hook Job: `openssl pkcs12 -export` builds an
intermediate keystore, then Curity's `convertks` repackages it. `openssl` requires an
export password, so `KEYSTORE_PASSWORD` exists only to hand that intermediate keystore
from `openssl` to `convertks`. It is **not** the password Curity uses at runtime —
`convertks` writes its output with its own default password (`default`), which is what
Curity's server-keystore loader expects (its config carries no password field).

The operator performs the whole conversion in one in-process step (PEM → PKCS#12 with
`default`), so there is no intermediate keystore and no tool-to-tool hand-off — nothing
for `keystorePasswordSecretRef` to protect. Dropping it removes a required Secret from
the user's setup with no change to what Curity loads.

---

## Mounting Targets: Admin vs Runtime Nodes

The operator applies the following routing logic when projecting configuration into pods:

- **When an admin `IdentityServerNode` exists in the cluster** — configuration is mounted
  only into admin node pods. The admin node then replicates configuration to runtime nodes
  via the Curity clustering protocol, as it does in the Helm-based deployment.
- **When no admin node exists** — configuration is mounted directly into all node pods
  (runtime-only clusters or single-node deployments).

This mirrors the behaviour of `curity.config.configuration` in the idsvr Helm chart, where
the admin deployment receives configuration mounts and distributes them to runtime nodes at
startup.

---

## Resource Examples

### ConfigMap — base configuration fragment

Use a `ConfigMap` for non-sensitive XML fragments such as environment base URL, logging
settings, or feature flags.

```yaml
# server-config-cm.yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: curity-base-config
  namespace: curity
  labels:
    curity.io/managed: "true"
  annotations:
    curity.io/config-type: "base"
data:
  base-config.xml: |
    <?xml version="1.0" encoding="UTF-8"?>
    <config xmlns="http://tail-f.com/ns/config/1.0">
      <environments xmlns="https://curity.se/ns/conf/base">
        <environment>
          <base-url>#{baseUrl | https://localhost:8443}</base-url>
        </environment>
      </environments>
    </config>
```

The XML format follows the
[Curity parameterized configuration guide](https://curity.io/docs/idsvr/latest/configuration-guide/parameterized-configuration.html).
Values in `#{...}` are substituted at load time from environment variables or
`startup.properties`.

### Secret — sensitive configuration fragment

Use a `Secret` for anything that must not live in a `ConfigMap`: credentials, connection
strings, or the license file.

**Datasource credentials** (`base` type — mounts to `/opt/idsvr/etc/init/`):

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: curity-datasource-config
  namespace: curity
  labels:
    curity.io/managed: "true"
type: Opaque
stringData:
  datasource-config.xml: |
    <?xml version="1.0" encoding="UTF-8"?>
    <config xmlns="http://tail-f.com/ns/config/1.0">
      <environments xmlns="https://curity.se/ns/conf/base">
        <environment>
          <facilities>
            <data-sources>
              <data-source>
                <id>default-datasource</id>
                <jdbc xmlns="https://curity.se/ns/ext-conf/jdbc">
                  <driver>PostgreSQL</driver>
                  <connection-string>#{JDBC_CONNECTION_STRING}</connection-string>
                  <username>#{JDBC_USERNAME}</username>
                  <password>#{JDBC_PASSWORD}</password>
                </jdbc>
              </data-source>
            </data-sources>
          </facilities>
        </environment>
      </environments>
    </config>
```

**License file** (`license` type — mounts to `/opt/idsvr/etc/init/license/license.json`):

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: curity-license
  namespace: curity
  labels:
    curity.io/managed: "true"
  annotations:
    curity.io/config-type: "license"
type: Opaque
stringData:
  license.json: |
    <paste license.json content here>
```

Obtain a license file from the [Curity Developer Portal](https://developer.curity.io/).

---

## Reference

- [Curity parameterized configuration](https://curity.io/docs/idsvr/latest/configuration-guide/parameterized-configuration.html)
- [`IdentityServerCluster` proposal](IdentityServerCluster_v1alpha1_propose.yaml)
- [`IdentityServerNode` proposal](IdentityServerNode_v1alpha1_propose.yaml)
