#!/usr/bin/env bash
# Pause and resume the x402 GKE cluster by scaling both node pools to zero and back.
# Called by `make gke-down`, `make gke-up` and `make gke-status`; run it from inside the
# repo so direnv supplies the personal gcloud identity.
#
#   down    autoscaling off, wait for the autoscaler to settle, resize to 0, verify, retry once
#   up      system pool to 1, autoscaling back on, wait for the node to be Ready
#   status  autoscaling, target size and live instances per pool
#
# Autoscaling goes off before the resize because, left on, the autoscaler sees system pods
# go Pending and adds a node straight back.
#
# Why the settle wait: on 2026-10-12 autoscaling was disabled about 3s before the resize,
# and GKE swapped the system node for a new one instead of removing it, so the pool sat
# at 1 node for six days. On 2026-10-18 the same resize, with autoscaling long off, went
# to 0 cleanly. The wait is the cheap guard; the verify-and-retry catches it if not enough.
set -euo pipefail

PROJECT="${PROJECT:-x402-envoy}"
CLUSTER="${CLUSTER:-x402}"
ZONE="${ZONE:-us-central1-a}"
SETTLE="${SETTLE:-60}"     # seconds between disabling autoscaling and resizing
TIMEOUT="${TIMEOUT:-600}"  # seconds to wait for a pool to reach its size

# Autoscaling bounds. These MUST match deploy/terraform/gke.tf, or the next
# `terraform plan` reports drift after gke-up.
SYSTEM_MIN=1 SYSTEM_MAX=2
WORKLOAD_MIN=0 WORKLOAD_MAX=3

log() { printf '%s  %s\n' "$(date +%H:%M:%S)" "$*"; }
die() { log "ERROR: $*" >&2; exit 1; }

# Every gcloud call names the project explicitly, so this can never act on whatever
# project the ambient gcloud config points at.
gc() { gcloud --project "$PROJECT" --quiet "$@"; }

preflight() {
  # Without direnv, gcloud and kubectl fall back to the Radius work config.
  [[ -n "${CLOUDSDK_CONFIG:-}" ]] ||
    die "CLOUDSDK_CONFIG is unset: run from inside the repo with direnv loaded (direnv allow)."
  gc container clusters describe "$CLUSTER" --zone "$ZONE" --format='value(name)' >/dev/null ||
    die "cannot see cluster $CLUSTER in $PROJECT with the current identity."
}

autoscaling_on() {
  [[ "$(gc container node-pools describe "$1" --cluster "$CLUSTER" --zone "$ZONE" \
    --format='value(autoscaling.enabled)')" == "True" ]]
}

mig() {
  gc container node-pools describe "$1" --cluster "$CLUSTER" --zone "$ZONE" \
    --format='value(instanceGroupUrls[0].basename())'
}

target_size() { gc compute instance-groups managed describe "$(mig "$1")" --zone "$ZONE" --format='value(targetSize)'; }
instance_count() {
  gc compute instance-groups managed list-instances "$(mig "$1")" --zone "$ZONE" --format='value(instance)' | grep -c . || true
}

status() {
  printf '%-10s %-12s %-7s %s\n' POOL AUTOSCALING TARGET INSTANCES
  for pool in system workload; do
    local as=off
    autoscaling_on "$pool" && as=on
    printf '%-10s %-12s %-7s %s\n' "$pool" "$as" "$(target_size "$pool")" "$(instance_count "$pool")"
  done
}

# Poll until the pool's target and live instance count both equal $2.
wait_for_size() {
  local pool=$1 want=$2 deadline=$((SECONDS + TIMEOUT)) t n
  while ((SECONDS < deadline)); do
    t=$(target_size "$pool") n=$(instance_count "$pool")
    [[ "$t" == "$want" && "$n" == "$want" ]] && return 0
    log "  $pool: target=$t instances=$n, waiting for $want"
    sleep 15
  done
  return 1
}

scale_to_zero() {
  local pool=$1 attempt
  for attempt in 1 2; do
    if [[ "$(target_size "$pool")" == 0 && "$(instance_count "$pool")" == 0 ]]; then
      log "$pool: at 0"
      return 0
    fi
    log "$pool: resizing to 0 (attempt $attempt; draining can take a few minutes)"
    gc container clusters resize "$CLUSTER" --zone "$ZONE" --node-pool "$pool" --num-nodes 0
    # gcloud returns when the GKE operation is DONE, but on 2026-10-12 it returned DONE
    # with a fresh node in place. Check what is actually running.
    wait_for_size "$pool" 0 && { log "$pool: at 0"; return 0; }
    log "$pool: did not reach 0; GKE kept or replaced a node:"
    gc compute instance-groups managed list-instances "$(mig "$pool")" --zone "$ZONE" \
      --format='table(instance.basename(),instanceStatus,currentAction)' || true
    ((attempt < 2)) && { log "waiting ${SETTLE}s before retrying"; sleep "$SETTLE"; }
  done
  die "$pool is still not at 0 after two attempts; it is billing. Check: make gke-status"
}

down() {
  preflight
  local changed=0
  for pool in workload system; do
    if autoscaling_on "$pool"; then
      log "$pool: disabling autoscaling"
      gc container clusters update "$CLUSTER" --zone "$ZONE" --node-pool "$pool" --no-enable-autoscaling
      changed=1
    fi
  done
  if ((changed)); then
    log "waiting ${SETTLE}s for the cluster autoscaler to stop acting on the pools"
    sleep "$SETTLE"
  fi
  scale_to_zero workload
  scale_to_zero system
  log "paused. Idle now is the reserved IP and Cloud NAT (about \$0.36/day)."
  status
}

# Use the cluster's own kube context explicitly, never the current one.
KCTX="gke_${PROJECT}_${ZONE}_${CLUSTER}"

wait_node_ready() {
  # gke-gcloud-auth-plugin ships in the SDK's bin, which may not be on a non-login PATH.
  PATH="$PATH:$(gcloud info --format='value(installation.sdk_root)')/bin"
  if [[ -z "${KUBECONFIG:-}" || "$KUBECONFIG" == "$HOME/.kube/config" ]]; then
    log "skipping the Ready check: KUBECONFIG is not the x402 one (direnv not loaded?)"
    return 0
  fi
  if ! kubectl config get-contexts "$KCTX" >/dev/null 2>&1; then
    log "skipping the Ready check: no $KCTX context. Run:"
    log "  gcloud container clusters get-credentials $CLUSTER --zone $ZONE"
    return 0
  fi
  local sel=cloud.google.com/gke-nodepool=system deadline=$((SECONDS + TIMEOUT))
  # The Node object only exists once the kubelet registers, so wait for it first.
  until [[ -n "$(kubectl --context "$KCTX" get nodes -l "$sel" -o name 2>/dev/null)" ]]; do
    ((SECONDS < deadline)) || die "system node never registered with the API server."
    log "  waiting for the system node to register"
    sleep 15
  done
  kubectl --context "$KCTX" wait --for=condition=Ready node -l "$sel" --timeout=300s
}

up() {
  preflight
  if [[ "$(target_size system)" == 0 ]]; then
    log "system: resizing to 1"
    gc container clusters resize "$CLUSTER" --zone "$ZONE" --node-pool system --num-nodes 1
  fi
  log "system: enabling autoscaling ($SYSTEM_MIN-$SYSTEM_MAX)"
  gc container clusters update "$CLUSTER" --zone "$ZONE" --node-pool system \
    --enable-autoscaling --min-nodes "$SYSTEM_MIN" --max-nodes "$SYSTEM_MAX"
  log "workload: enabling autoscaling ($WORKLOAD_MIN-$WORKLOAD_MAX)"
  gc container clusters update "$CLUSTER" --zone "$ZONE" --node-pool workload \
    --enable-autoscaling --min-nodes "$WORKLOAD_MIN" --max-nodes "$WORKLOAD_MAX"
  wait_for_size system 1 || die "system pool did not reach 1 node within ${TIMEOUT}s."
  wait_node_ready
  log "up. Workload nodes are added by the autoscaler when pods need them."
  status
}

case "${1:-}" in
  down) down ;;
  up) up ;;
  status) preflight && status ;;
  *) echo "usage: $0 down|up|status" >&2; exit 2 ;;
esac
