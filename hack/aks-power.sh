#!/usr/bin/env bash
# Start or stop the AKS cluster the e2e suite runs on, so it only costs money
# while a test run needs it.
#
#   hack/aks-power.sh start            start the cluster, wait until nodes are Ready
#   hack/aks-power.sh stop             stop (deallocate) the cluster
#   hack/aks-power.sh status           print power and provisioning state
#   hack/aks-power.sh run -- CMD...    start, run CMD against the cluster, then
#                                      stop it whatever CMD's outcome (also on
#                                      Ctrl-C); exits with CMD's status
#
# Needs AKS_RESOURCE_GROUP and AKS_CLUSTER_NAME, and an `az` session allowed to
# read, start and stop that cluster. Both start and stop are idempotent: they
# wait out an operation already in progress and do nothing if the cluster is
# already in the requested state.
#
# `run` points KUBECONFIG at a throwaway file holding only this cluster's
# credentials, so CMD cannot reach whatever context happens to be current.
set -euo pipefail

: "${AKS_RESOURCE_GROUP:?set AKS_RESOURCE_GROUP to the e2e cluster's resource group}"
: "${AKS_CLUSTER_NAME:?set AKS_CLUSTER_NAME to the e2e cluster's name}"

# How long to wait for an in-flight operation, or for nodes after a start.
AKS_WAIT_TIMEOUT="${AKS_WAIT_TIMEOUT:-1200}"

aks=(--resource-group "$AKS_RESOURCE_GROUP" --name "$AKS_CLUSTER_NAME")

log() { echo "[aks-power] $*" >&2; }

# Prints "<powerState> <provisioningState>", e.g. "Running Succeeded".
state() {
  az aks show "${aks[@]}" --query "join(' ', [powerState.code, provisioningState])" -o tsv
}

# Wait until no operation (Starting, Stopping, Updating, ...) is in flight.
wait_settled() {
  local deadline=$((SECONDS + AKS_WAIT_TIMEOUT)) s
  while :; do
    s=$(state)
    case "${s#* }" in
      Succeeded | Failed | Canceled) echo "$s"; return 0 ;;
    esac
    if ((SECONDS >= deadline)); then
      log "timed out waiting for $AKS_CLUSTER_NAME to settle (last state: $s)"
      return 1
    fi
    log "$AKS_CLUSTER_NAME is busy ($s), waiting..."
    sleep 20
  done
}

# Wait until the API server answers and every node reports Ready. Nodes are
# re-created on start, so stale NotReady entries can linger for a while.
wait_ready() {
  local deadline=$((SECONDS + AKS_WAIT_TIMEOUT))
  until kubectl get --raw /readyz >/dev/null 2>&1 &&
    [[ -n "$(kubectl get nodes -o name 2>/dev/null)" ]] &&
    ! kubectl get nodes --no-headers 2>/dev/null | awk '$2 !~ /^Ready/ { bad=1 } END { exit !bad }'; do
    if ((SECONDS >= deadline)); then
      log "timed out waiting for nodes to become Ready"
      kubectl get nodes -o wide >&2 || true
      return 1
    fi
    log "waiting for API server and nodes..."
    sleep 15
  done
  log "cluster is ready"
}

start() {
  local s
  s=$(wait_settled)
  if [[ "${s% *}" == Running && "${s#* }" == Succeeded ]]; then
    log "$AKS_CLUSTER_NAME is already running"
  else
    log "starting $AKS_CLUSTER_NAME (was: $s)"
    # --no-wait, then poll `az aks show`: waiting inside az polls the ARM
    # operation-status endpoint, which needs more than read/start/stop on the
    # cluster. This way the identity needs nothing beyond those three.
    az aks start "${aks[@]}" --no-wait --output none
    s=$(wait_settled)
    if [[ "$s" != "Running Succeeded" ]]; then
      log "start did not succeed (state: $s)"
      return 1
    fi
  fi
  # Only check readiness when kubectl has somewhere to point; CI configures its
  # own context after this, and checks there instead.
  if [[ "${1:-}" == --wait-ready ]]; then
    wait_ready
  fi
}

stop() {
  local s
  s=$(wait_settled)
  if [[ "${s% *}" == Stopped ]]; then
    log "$AKS_CLUSTER_NAME is already stopped"
    return 0
  fi
  log "stopping $AKS_CLUSTER_NAME (was: $s)"
  az aks stop "${aks[@]}" --no-wait --output none
  s=$(wait_settled)
  if [[ "${s% *}" != Stopped ]]; then
    log "stop did not succeed (state: $s)"
    return 1
  fi
  log "$AKS_CLUSTER_NAME stopped"
}

run() {
  [[ "${1:-}" == -- ]] && shift
  (($#)) || { log "run needs a command: run -- make test-e2e-remote ..."; exit 2; }

  local status=0
  # Global, not local: the EXIT trap reads it after run() has been left.
  kubeconfig=$(mktemp)
  # Registered before the start, so a failure or Ctrl-C at any point after it
  # still stops the cluster. The trap keeps the command's exit status.
  trap 'rc=$?; trap - EXIT INT TERM; stop || rc=1; rm -f "$kubeconfig"; exit $rc' EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM

  start
  az aks get-credentials "${aks[@]}" --file "$kubeconfig" --overwrite-existing --output none
  export KUBECONFIG="$kubeconfig"
  wait_ready

  log "running: $*"
  "$@" || status=$?
  exit "$status"
}

case "${1:-}" in
  start) shift; start "$@" ;;
  stop) stop ;;
  status) state ;;
  run) shift; run "$@" ;;
  *)
    echo "usage: $0 start [--wait-ready] | stop | status | run -- CMD..." >&2
    exit 2
    ;;
esac
