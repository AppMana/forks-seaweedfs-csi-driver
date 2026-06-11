#!/bin/bash
# Fail fast unless the kind/QEMU SeaweedFS CSI baseline is actually ready.
#
# Preconditions: the calico fork's check-kind-qemu-calico-ready.sh preflight
# must already pass (networking bootstrap); this script only checks the
# storage stack on top of it.

set -euo pipefail

KUBECONFIG="${KUBECONFIG:-/tmp/appmana-calico-kind/kubeconfig}"
LAB_CONTEXT="${LAB_CONTEXT:-kind-appmana-calico}"
NAMESPACE="${NAMESPACE:-seaweedfs-csi}"
WINDOWS_NODE="${WINDOWS_NODE:-appmana-000}"
SSH_USER="${SSH_USER:-administrator}"
READY_TIMEOUT="${READY_TIMEOUT:-30s}"

current_context=$(kubectl --kubeconfig "$KUBECONFIG" config current-context 2>/dev/null || true)
if [[ "$current_context" != "$LAB_CONTEXT" ]]; then
  echo "ERROR: refusing to run: kubectl context is '${current_context:-<none>}', expected the kind lab context '$LAB_CONTEXT'." >&2
  echo "These scripts must never target the production cluster." >&2
  exit 1
fi

first_ipv4() {
  tr ' ' '\n' | awk '/^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$/ { print; exit }'
}

WINDOWS_HOST="${WINDOWS_HOST:-}"
if [[ -z "$WINDOWS_HOST" ]]; then
  WINDOWS_HOST=$(
    kubectl --kubeconfig "$KUBECONFIG" get node "$WINDOWS_NODE" \
      -o jsonpath='{range .status.addresses[?(@.type=="InternalIP")]}{.address}{" "}{end}' |
      first_ipv4
  )
fi

if [[ -z "$WINDOWS_HOST" ]]; then
  echo "ERROR: could not determine IPv4 InternalIP for Windows node $WINDOWS_NODE" >&2
  exit 1
fi

missing=()

check_ready() {
  local key="$1" selector="$2" kind="$3"
  if kubectl --kubeconfig "$KUBECONFIG" -n "$NAMESPACE" wait \
    --for=condition=Ready pod -l "$selector" --timeout="$READY_TIMEOUT" >/dev/null 2>&1; then
    echo "$key=True"
  else
    echo "$key=False"
    missing+=("$kind")
  fi
}

check_ready controller_ready app=seaweedfs-controller "seaweedfs-controller pods Ready"
check_ready linux_mount_ready app=seaweedfs-mount "seaweedfs-mount pods Ready"
check_ready linux_node_ready app=seaweedfs-node "seaweedfs-node pods Ready"
check_ready windows_mount_ready app=seaweedfs-mount-windows "seaweedfs-mount-windows pods Ready"
check_ready windows_node_ready app=seaweedfs-node-windows "seaweedfs-node-windows pods Ready"

drivers=$(
  kubectl --kubeconfig "$KUBECONFIG" get csinode "$WINDOWS_NODE" \
    -o jsonpath='{.spec.drivers[*].name}' 2>/dev/null || true
)
if [[ " $drivers " == *" seaweedfs-csi-driver "* ]]; then
  echo "csinode_driver=True"
else
  echo "csinode_driver=False"
  missing+=("csinode $WINDOWS_NODE lists seaweedfs-csi-driver")
fi

status=$(
  ssh -o StrictHostKeyChecking=no -o ConnectTimeout=5 "$SSH_USER@$WINDOWS_HOST" \
    'powershell -NoProfile -Command "
      $ErrorActionPreference = \"SilentlyContinue\"
      $launcher = (Get-Service WinFsp.Launcher).Status -eq \"Running\"
      $mountsock = Test-Path C:\var\lib\seaweedfs-mount\seaweedfs-mount.sock
      $csisock = Test-Path C:\var\lib\kubelet\plugins\seaweedfs-csi-driver\csi.sock
      \"winfsp_launcher=$launcher\"
      \"mount_sock=$mountsock\"
      \"csi_sock=$csisock\"
    "' | tr -d '\r' || true
)

echo "$status"

grep -Fq 'winfsp_launcher=True' <<<"$status" || missing+=("WinFsp.Launcher service running")
grep -Fq 'mount_sock=True' <<<"$status" || missing+=("C:\\var\\lib\\seaweedfs-mount\\seaweedfs-mount.sock")
grep -Fq 'csi_sock=True' <<<"$status" || missing+=("C:\\var\\lib\\kubelet\\plugins\\seaweedfs-csi-driver\\csi.sock")

if (( ${#missing[@]} > 0 )); then
  printf 'ERROR: SeaweedFS CSI preflight failed; missing: %s\n' "${missing[*]}" >&2
  printf 'This is a bootstrap failure, not a health-matrix result.\n' >&2
  exit 1
fi
