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

echo "  Helm postprocessing complete."
