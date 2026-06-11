#!/bin/bash
# Run the full AppMana kind + QEMU SeaweedFS CSI storage e2e flow.
#
# Mirrors the calico fork's run-kind-qemu-health.sh: applies the kind/QEMU
# forwarding rules and the calico networking preflight from the sibling
# calico repo, then deploys SeaweedFS, deploys the CSI driver under test,
# runs the CSI preflight, and finally the storage health matrix.
# RESILIENCE=true appends the resilience drills.
#
# All sub-script paths are env-overridable: FORWARDING_SCRIPT,
# PREFLIGHT_SCRIPT (calico), DEPLOY_SEAWEEDFS_SCRIPT, DEPLOY_CSI_SCRIPT,
# CSI_PREFLIGHT_SCRIPT, HEALTH_CHECK_SCRIPT, RESILIENCE_SCRIPT.
#
# Usage: run-kind-qemu-storage-e2e.sh [LINUX_NODE [WINDOWS_NODE]]

set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)

export KUBECONFIG="${KUBECONFIG:-/tmp/appmana-calico-kind/kubeconfig}"

CALICO_HACK_DIR="${CALICO_HACK_DIR:-$SCRIPT_DIR/../../../forks-calico-windows-ipv6/hack/appmana}"

SEAWEEDFS_NAMESPACE="${SEAWEEDFS_NAMESPACE:-seaweedfs-test}"
SEAWEEDFS_IMAGE="${SEAWEEDFS_IMAGE:-chrislusf/seaweedfs:4.23_large_disk}"
SEAWEEDFS_FILER="${SEAWEEDFS_FILER:-}"
CSI_NAMESPACE="${CSI_NAMESPACE:-seaweedfs-csi}"
CSI_IMAGE="${CSI_IMAGE:-ghcr.io/appmana/seaweedfs-csi-driver:v1.4.12-appmana.post.1}"
MOUNT_IMAGE="${MOUNT_IMAGE:-ghcr.io/appmana/seaweedfs-mount:v1.4.12-appmana.post.1}"
TEST_NAMESPACE="${TEST_NAMESPACE:-seaweedfs-csi-test}"
STORAGE_CLASS="${STORAGE_CLASS:-seaweedfs-storage}"
LINUX_NODE="${LINUX_NODE:-appmana-calico-worker}"
WINDOWS_NODE="${WINDOWS_NODE:-appmana-000}"
WINDOWS_EXEC="${WINDOWS_EXEC:-hcsdiag}"
APPLY_FORWARDING="${APPLY_FORWARDING:-true}"
PREFLIGHT="${PREFLIGHT:-true}"
CSI_PREFLIGHT="${CSI_PREFLIGHT:-true}"
RESILIENCE="${RESILIENCE:-false}"

if [[ $# -ge 1 ]]; then
  LINUX_NODE="$1"
fi
if [[ $# -ge 2 ]]; then
  WINDOWS_NODE="$2"
fi

if [[ "$APPLY_FORWARDING" == "true" ]]; then
  "${FORWARDING_SCRIPT:-$CALICO_HACK_DIR/apply-kind-qemu-forwarding.sh}"
fi

if [[ "$PREFLIGHT" == "true" ]]; then
  "${PREFLIGHT_SCRIPT:-$CALICO_HACK_DIR/check-kind-qemu-calico-ready.sh}"
fi

deploy_args=(--namespace "$SEAWEEDFS_NAMESPACE" --image "$SEAWEEDFS_IMAGE")
if [[ -n "$SEAWEEDFS_FILER" ]]; then
  deploy_args+=(--filer "$SEAWEEDFS_FILER")
fi
deploy_out=$(
  "${DEPLOY_SEAWEEDFS_SCRIPT:-$SCRIPT_DIR/deploy-seaweedfs-kind.sh}" "${deploy_args[@]}" |
    tee /dev/stderr
)
FILER=$(grep '^FILER=' <<<"$deploy_out" | tail -n 1 || true)
FILER="${FILER#FILER=}"
if [[ -z "$FILER" ]]; then
  echo "ERROR: deploy-seaweedfs did not print FILER=<host:port>" >&2
  exit 1
fi

"${DEPLOY_CSI_SCRIPT:-$SCRIPT_DIR/deploy-csi-driver-kind.sh}" \
  --namespace "$CSI_NAMESPACE" \
  --csi-image "$CSI_IMAGE" \
  --mount-image "$MOUNT_IMAGE" \
  --filer "$FILER"

if [[ "$CSI_PREFLIGHT" == "true" ]]; then
  NAMESPACE="$CSI_NAMESPACE" WINDOWS_NODE="$WINDOWS_NODE" \
    "${CSI_PREFLIGHT_SCRIPT:-$SCRIPT_DIR/check-kind-qemu-csi-ready.sh}"
fi

health_args=(
  --namespace "$TEST_NAMESPACE"
  --storage-class "$STORAGE_CLASS"
  --linux-node "$LINUX_NODE"
  --windows-node "$WINDOWS_NODE"
  --windows-exec "$WINDOWS_EXEC"
)

if [[ "$RESILIENCE" == "true" ]]; then
  "${HEALTH_CHECK_SCRIPT:-$SCRIPT_DIR/storage-health-check.sh}" "${health_args[@]}"
  exec env \
    CSI_NAMESPACE="$CSI_NAMESPACE" \
    TEST_NAMESPACE="$TEST_NAMESPACE" \
    STORAGE_CLASS="$STORAGE_CLASS" \
    LINUX_NODE="$LINUX_NODE" \
    WINDOWS_NODE="$WINDOWS_NODE" \
    HEALTH_CHECK_SCRIPT="${HEALTH_CHECK_SCRIPT:-$SCRIPT_DIR/storage-health-check.sh}" \
    "${RESILIENCE_SCRIPT:-$SCRIPT_DIR/run-kind-qemu-csi-resilience.sh}"
else
  exec "${HEALTH_CHECK_SCRIPT:-$SCRIPT_DIR/storage-health-check.sh}" "${health_args[@]}"
fi
