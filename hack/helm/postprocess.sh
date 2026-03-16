#!/usr/bin/env bash
# postprocess.sh — Runs after helmify to enhance auto-generated Helm chart.
#
# Called by Makefile via HELMIFY_POSTPROCESS.
# Uses HELM_CHART_DIR env var or defaults to charts/curity-operator.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
CHART_DIR="${HELM_CHART_DIR:-${PROJECT_ROOT}/charts/curity-operator}"

# Verify chart exists
if [ ! -d "$CHART_DIR/templates" ]; then
  echo "ERROR: Chart templates directory not found at $CHART_DIR/templates"
  exit 1
fi

echo "  Helm postprocessing complete."
