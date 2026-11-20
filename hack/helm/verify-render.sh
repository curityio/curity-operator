#!/usr/bin/env bash
# verify-render.sh — Renders the generated Helm chart with different values and
# asserts what postprocess.sh is meant to produce.
#
# Some postprocess.sh edits match helmify's exact output and would silently do
# nothing if that output changed; `helm lint` would still pass. This catches it.
#
# Usage: verify-render.sh [CHART_DIR]   (default: charts/curity-operator)
# Uses YQ env var or falls back to "yq" on PATH (mikefarah/yq v4).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
CHART_DIR="${1:-${HELM_CHART_DIR:-${PROJECT_ROOT}/charts/curity-operator}}"
YQ="${YQ:-yq}"
NS=verify-ns

if [ ! -f "$CHART_DIR/Chart.yaml" ]; then
  echo "ERROR: no chart at $CHART_DIR; run 'make helmify' first"
  exit 1
fi

failures=0
pass() { echo "  ok    $1"; }
fail() { echo "  FAIL  $1"; failures=$((failures + 1)); }

# expect DESCRIPTION EXPECTED ACTUAL
expect() {
  if [ "$2" = "$3" ]; then pass "$1"; else fail "$1 (expected '$2', got '$3')"; fi
}

render() { helm template curity-operator "$CHART_DIR" --namespace "$NS" "$@"; }

# count RENDERED SELECTOR — number of documents matching a yq select().
count() { echo "$1" | "$YQ" ea "[select($2)] | length"; }

# query RENDERED EXPRESSION — yq over every document, dropping the blank
# lines yq prints for documents the expression filtered out.
query() { echo "$1" | "$YQ" "$2" | sed '/^$/d'; }

MANAGER_ROLE='(.metadata.name | test("-manager-role$"))'
MANAGER_BINDING='(.metadata.name | test("-manager-rolebinding$"))'
DEPLOYMENT='.kind == "Deployment"'
WATCH_NS_VALUE='.spec.template.spec.containers[] | select(.name == "manager") | .env[] | select(.name == "WATCH_NAMESPACE") | .value'
CONTAINERS='[.spec.template.spec.containers[].name] | sort | join(",")'

echo "default values (watchScope=cluster):"
out="$(render)"
expect "manager ClusterRole"                1 "$(count "$out" ".kind == \"ClusterRole\" and $MANAGER_ROLE")"
expect "manager ClusterRoleBinding"         1 "$(count "$out" ".kind == \"ClusterRoleBinding\" and $MANAGER_BINDING")"
expect "no namespaced manager Role"         0 "$(count "$out" ".kind == \"Role\" and $MANAGER_ROLE")"
expect "3 CRDs"                             3 "$(count "$out" '.kind == "CustomResourceDefinition"')"
expect "manager and kube-rbac-proxy containers" "kube-rbac-proxy,manager" \
  "$(query "$out" "select($DEPLOYMENT) | $CONTAINERS")"
expect "no WATCH_NAMESPACE"                 "" "$(query "$out" "select($DEPLOYMENT) | $WATCH_NS_VALUE")"
cluster_rules="$(query "$out" "select(.kind == \"ClusterRole\" and $MANAGER_ROLE) | .rules")"

echo "strict namespace-scoped install:"
out="$(render --set watchScope=namespace --set crds.install=false \
  --set controllerManager.kubeRbacProxy.enabled=false)"
expect "no cluster-scoped objects"          0 \
  "$(count "$out" '.kind == "ClusterRole" or .kind == "ClusterRoleBinding" or .kind == "CustomResourceDefinition"')"
expect "manager Role in release namespace"  "$NS" \
  "$(query "$out" "select(.kind == \"Role\" and $MANAGER_ROLE) | .metadata.namespace")"
expect "manager RoleBinding in release namespace" "$NS" \
  "$(query "$out" "select(.kind == \"RoleBinding\" and $MANAGER_BINDING) | .metadata.namespace")"
expect "RoleBinding refers to the Role"     "Role" \
  "$(query "$out" "select(.kind == \"RoleBinding\" and $MANAGER_BINDING) | .roleRef.kind")"
role_rules="$(query "$out" "select(.kind == \"Role\" and $MANAGER_ROLE) | .rules")"
if [ -n "$role_rules" ] && [ "$role_rules" = "$cluster_rules" ]; then
  pass "Role rules match the ClusterRole rules"
else
  fail "Role rules match the ClusterRole rules"
fi
expect "WATCH_NAMESPACE is the release namespace" "$NS" \
  "$(query "$out" "select($DEPLOYMENT) | $WATCH_NS_VALUE")"
expect "manager container only"             "manager" "$(query "$out" "select($DEPLOYMENT) | $CONTAINERS")"
expect "no metrics Service"                 0 "$(count "$out" '.kind == "Service"')"

echo "invalid watchScope:"
if err="$(render --set watchScope=bogus 2>&1)"; then
  fail "watchScope=bogus fails to render"
elif echo "$err" | grep -q 'watchScope must be "cluster" or "namespace"'; then
  pass "watchScope=bogus fails with a clear message"
else
  fail "watchScope=bogus fails with a clear message (got: $err)"
fi

if [ "$failures" -gt 0 ]; then
  echo "$failures check(s) failed"
  exit 1
fi
echo "all chart render checks passed"
