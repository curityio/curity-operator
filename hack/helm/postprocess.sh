#!/usr/bin/env bash
# postprocess.sh — Runs after helmify to enhance auto-generated Helm chart.
#
# Called by Makefile via HELMIFY_POSTPROCESS.
# Uses HELM_CHART_DIR env var or defaults to charts/curity-operator.
# Uses YQ env var or falls back to "yq" on PATH.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
CHART_DIR="${HELM_CHART_DIR:-${PROJECT_ROOT}/charts/curity-operator}"
YQ="${YQ:-yq}"

if [ ! -d "$CHART_DIR/templates" ]; then
  echo "ERROR: Chart templates directory not found at $CHART_DIR/templates"
  exit 1
fi

# Annotate CRDs with helm.sh/resource-policy, gated by .Values.crds.keep.
# When crds.keep=true (default), value renders to "keep" and `helm uninstall`
# skips the CRDs — so user CustomResources are not cascade-deleted with the
# release. When crds.keep=false, value renders empty and uninstall behaves
# normally (only the literal string "keep" enables retention per Helm spec).
#
# Uses awk (not yq) because helmify-generated CRD files contain Helm template
# directives (e.g. `{{- include ... }}`) that break YAML parsers.
echo "  Annotating CRDs with helm.sh/resource-policy (gated by .Values.crds.keep)..."
for crd in "$CHART_DIR"/templates/*-crd.yaml; do
  [ -f "$crd" ] || continue
  if grep -q "helm.sh/resource-policy" "$crd"; then
    continue
  fi
  awk '
    /^  annotations:$/ && !done {
      print
      print "    helm.sh/resource-policy: '\''{{ if .Values.crds.keep }}keep{{ end }}'\''"
      done = 1
      next
    }
    { print }
  ' "$crd" > "$crd.tmp"
  mv "$crd.tmp" "$crd"
done

# Default crds.keep to true in values.yaml. Idempotent across regenerations.
if ! "$YQ" -e '.crds.keep' "$CHART_DIR/values.yaml" >/dev/null 2>&1; then
  echo "  Adding crds.keep default to values.yaml..."
  "$YQ" -i '.crds.keep = true' "$CHART_DIR/values.yaml"
fi

# wrap_file FILE CONDITION — guard a whole template with `{{- if CONDITION }}`.
# Idempotent: skipped when the file already starts with the guard.
wrap_file() {
  local file="$1" cond="$2"
  [ -f "$file" ] || return 0
  local guard="{{- if $cond }}"
  if [ "$(head -n 1 "$file")" = "$guard" ]; then
    return 0
  fi
  { echo "$guard"; cat "$file"; echo "{{- end }}"; } > "$file.tmp"
  mv "$file.tmp" "$file"
}

# crds.install: lets a cluster admin install the CRDs once while namespace
# admins install the chart without them (CRDs are always cluster-scoped, and
# Helm will not adopt CRDs owned by another release).
echo "  Gating CRDs on .Values.crds.install..."
for crd in "$CHART_DIR"/templates/*-crd.yaml; do
  wrap_file "$crd" ".Values.crds.install"
done

# kubeRbacProxy.enabled: the sidecar needs cluster-scoped RBAC (tokenreviews,
# subjectaccessreviews), so a strict namespace-scoped install must be able to
# drop it together with its roles and the metrics Service.
echo "  Gating kube-rbac-proxy on .Values.controllerManager.kubeRbacProxy.enabled..."
for f in proxy-rbac.yaml metrics-reader-rbac.yaml metrics-service.yaml; do
  wrap_file "$CHART_DIR/templates/$f" ".Values.controllerManager.kubeRbacProxy.enabled"
done

DEPLOYMENT="$CHART_DIR/templates/deployment.yaml"
if ! grep -q "kubeRbacProxy.enabled" "$DEPLOYMENT"; then
  # The proxy container runs from its `- args:` line up to the manager
  # container's `- args:` line.
  awk '
    /^      - args: .*kubeRbacProxy\.args/ { print "      {{- if .Values.controllerManager.kubeRbacProxy.enabled }}"; inproxy = 1 }
    /^      - args: .*manager\.args/ && inproxy { print "      {{- end }}"; inproxy = 0 }
    { print }
  ' "$DEPLOYMENT" > "$DEPLOYMENT.tmp"
  mv "$DEPLOYMENT.tmp" "$DEPLOYMENT"
fi

# watchScope: cluster (default) watches all namespaces with a ClusterRole;
# namespace limits the operator to the release namespace with a Role.
echo "  Adding watchScope (cluster | namespace)..."
if ! grep -q "WATCH_NAMESPACE" "$DEPLOYMENT"; then
  awk '
    /^      - args: .*manager\.args/ { inmanager = 1 }
    { print }
    inmanager && /^        env:$/ {
      print "        {{- if eq .Values.watchScope \"namespace\" }}"
      print "        - name: WATCH_NAMESPACE"
      print "          value: {{ quote .Release.Namespace }}"
      print "        {{- end }}"
      inmanager = 0
    }
  ' "$DEPLOYMENT" > "$DEPLOYMENT.tmp"
  mv "$DEPLOYMENT.tmp" "$DEPLOYMENT"
fi

MANAGER_RBAC="$CHART_DIR/templates/manager-rbac.yaml"
if ! grep -q "curity-operator.managerRules" "$MANAGER_RBAC"; then
  # Reuse the controller-gen rules verbatim: everything after `rules:` in the
  # ClusterRole document, up to the `---` before the binding.
  RULES="$(awk '/^---/ { exit } found { print } /^rules:$/ { found = 1 }' "$MANAGER_RBAC")"
  if [ -z "$RULES" ]; then
    echo "ERROR: could not extract manager rules from $MANAGER_RBAC"
    exit 1
  fi
  cat > "$MANAGER_RBAC" <<EOF
{{- if not (has .Values.watchScope (list "cluster" "namespace")) }}
{{- fail (printf "watchScope must be \"cluster\" or \"namespace\", got %q" (toString .Values.watchScope)) }}
{{- end }}
{{- define "curity-operator.managerRules" }}
${RULES}
{{- end }}
{{- if eq .Values.watchScope "cluster" }}
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: {{ include "curity-operator.fullname" . }}-manager-role
  labels:
  {{- include "curity-operator.labels" . | nindent 4 }}
rules:
{{- include "curity-operator.managerRules" . }}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: {{ include "curity-operator.fullname" . }}-manager-rolebinding
  labels:
  {{- include "curity-operator.labels" . | nindent 4 }}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: '{{ include "curity-operator.fullname" . }}-manager-role'
subjects:
- kind: ServiceAccount
  name: '{{ include "curity-operator.serviceAccountName" . }}'
  namespace: '{{ .Release.Namespace }}'
{{- else }}
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: {{ include "curity-operator.fullname" . }}-manager-role
  namespace: {{ .Release.Namespace }}
  labels:
  {{- include "curity-operator.labels" . | nindent 4 }}
rules:
{{- include "curity-operator.managerRules" . }}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: {{ include "curity-operator.fullname" . }}-manager-rolebinding
  namespace: {{ .Release.Namespace }}
  labels:
  {{- include "curity-operator.labels" . | nindent 4 }}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: '{{ include "curity-operator.fullname" . }}-manager-role'
subjects:
- kind: ServiceAccount
  name: '{{ include "curity-operator.serviceAccountName" . }}'
  namespace: '{{ .Release.Namespace }}'
{{- end }}
EOF
fi

# Defaults for the new values. Idempotent across regenerations.
if ! "$YQ" -e 'has("watchScope")' "$CHART_DIR/values.yaml" >/dev/null 2>&1; then
  "$YQ" -i '.watchScope = "cluster" | .watchScope line_comment = "cluster: watch all namespaces (ClusterRole); namespace: watch only the release namespace (Role)"' "$CHART_DIR/values.yaml"
fi
if ! "$YQ" -e '.crds | has("install")' "$CHART_DIR/values.yaml" >/dev/null 2>&1; then
  "$YQ" -i '.crds.install = true | .crds.install line_comment = "set false when the CRDs are installed separately, e.g. by a cluster admin or another release"' "$CHART_DIR/values.yaml"
fi
if ! "$YQ" -e '.controllerManager.kubeRbacProxy | has("enabled")' "$CHART_DIR/values.yaml" >/dev/null 2>&1; then
  "$YQ" -i '.controllerManager.kubeRbacProxy.enabled = true | .controllerManager.kubeRbacProxy.enabled line_comment = "the sidecar needs cluster-scoped RBAC; set false for a fully namespace-scoped install"' "$CHART_DIR/values.yaml"
fi

echo "  Helm postprocessing complete."
