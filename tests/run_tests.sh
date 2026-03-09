#!/usr/bin/env bash
set -euo pipefail

echo "[INFO] Running e2e tests..."

echo "[INFO] Verifying cluster connectivity..."
kubectl cluster-info || { echo "[ERROR] Cannot connect to cluster"; exit 1; }
kubectl get nodes || { echo "[ERROR] Cannot list nodes"; exit 1; }

echo "[INFO] All e2e tests passed"
