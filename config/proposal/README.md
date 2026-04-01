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
When omitted, the type defaults to `basic`:

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
| `basic` | `/opt/idsvr/etc/init/` | General XML configuration fragments. Each data key becomes a file at that path. Equivalent to `curity.config.configuration[].configMapRef` in the Helm chart. |
| `license` | `/opt/idsvr/etc/init/license/` | License file. Mounted to a dedicated sub-directory under `init` as required by the Identity Server. |

> Additional types (e.g. `post-commit-script`) may be introduced as the operator evolves.
> Each type encapsulates the mount path and any special handling so it does not need to be
> repeated across resources.

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

**Datasource credentials** (`basic` type — mounts to `/opt/idsvr/etc/init/`):

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
