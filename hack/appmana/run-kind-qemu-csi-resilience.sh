#!/bin/bash
# Sequenced SeaweedFS CSI resilience drills for the kind+QEMU lab.
#
# A long-lived Windows canary pod with a mounted RWX volume is created
# first; each drill asserts canary IO via ssh + hcsdiag (independent of the
# kubelet exec path and of the CSI node plugin), then re-runs the storage
# health matrix in --quick mode:
#   (i)   delete the seaweedfs-node-windows pod: an existing pod's mounted
#         volume must keep serving IO while the plugin restarts
#   (ii)  kill the seaweedfs-mount.exe supervisor on the VM: the DaemonSet
#         restarts it and the node plugin health monitor re-stages volumes
#   (iii) Restart-Service kubelet: node returns Ready, mounts stay intact

set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)

export KUBECONFIG="${KUBECONFIG:-/tmp/appmana-calico-kind/kubeconfig}"
LAB_CONTEXT="${LAB_CONTEXT:-kind-appmana-calico}"

CSI_NAMESPACE="${CSI_NAMESPACE:-seaweedfs-csi}"
TEST_NAMESPACE="${TEST_NAMESPACE:-seaweedfs-csi-test}"
CANARY_NAMESPACE="${CANARY_NAMESPACE:-seaweedfs-csi-resilience}"
STORAGE_CLASS="${STORAGE_CLASS:-seaweedfs-storage}"
LINUX_NODE="${LINUX_NODE:-kind-worker}"
WINDOWS_NODE="${WINDOWS_NODE:-appmana-000}"
SSH_USER="${SSH_USER:-administrator}"
WIN_IMAGE="${WIN_IMAGE:-mcr.microsoft.com/windows/servercore:ltsc2022}"
HEALTH_CHECK_SCRIPT="${HEALTH_CHECK_SCRIPT:-$SCRIPT_DIR/storage-health-check.sh}"

CANARY_PVC="swfs-resilience-pvc"
CANARY_POD="swfs-resilience-canary-windows"

current_context=$(kubectl config current-context 2>/dev/null || true)
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
    kubectl get node "$WINDOWS_NODE" \
      -o jsonpath='{range .status.addresses[?(@.type=="InternalIP")]}{.address}{" "}{end}' |
      first_ipv4
  )
fi
if [[ -z "$WINDOWS_HOST" ]]; then
  echo "ERROR: could not determine IPv4 InternalIP for Windows node $WINDOWS_NODE" >&2
  exit 1
fi

cleanup() {
  kubectl delete pod "$CANARY_POD" -n "$CANARY_NAMESPACE" --force --grace-period=0 >/dev/null 2>&1 || true
  kubectl delete pvc "$CANARY_PVC" -n "$CANARY_NAMESPACE" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

windows_host_ps() {
  local script="$1"
  ssh -o StrictHostKeyChecking=no -o ConnectTimeout=5 "$SSH_USER@$WINDOWS_HOST" \
    "powershell -NoProfile -Command \"$script\""
}

# Write + read back a marker inside the canary pod via hcsdiag (does not
# depend on the kubelet exec path or the CSI node plugin being up).
canary_io() {
  local tag="$1" cid encoded script
  cid=$(kubectl get pod "$CANARY_POD" -n "$CANARY_NAMESPACE" -o jsonpath='{.status.containerStatuses[0].containerID}' 2>/dev/null)
  cid="${cid#containerd://}"
  if [[ -z "$cid" ]]; then
    return 1
  fi
  script="Set-Content -Path C:\data\canary-$tag.txt -Value 'canary-$tag'; if ((Get-Content C:\data\canary-$tag.txt) -match 'canary-$tag') { exit 0 } else { exit 1 }"
  encoded=$(printf '%s' "$script" | iconv -f UTF-8 -t UTF-16LE | base64 -w0)
  ssh -o StrictHostKeyChecking=no -o ConnectTimeout=5 "$SSH_USER@$WINDOWS_HOST" \
    "hcsdiag exec $cid powershell -NoProfile -EncodedCommand $encoded"
}

run_health() {
  "$HEALTH_CHECK_SCRIPT" \
    --namespace "$TEST_NAMESPACE" \
    --storage-class "$STORAGE_CLASS" \
    --linux-node "$LINUX_NODE" \
    --windows-node "$WINDOWS_NODE" \
    --windows-exec hcsdiag \
    --quick
}

echo "=== Resilience setup: canary volume + pod on $WINDOWS_NODE ==="
kubectl create namespace "$CANARY_NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -

kubectl apply -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: $CANARY_PVC
  namespace: $CANARY_NAMESPACE
spec:
  accessModes:
    - ReadWriteMany
  storageClassName: $STORAGE_CLASS
  resources:
    requests:
      storage: 1Gi
EOF

bound=false
deadline=$(( $(date +%s) + 120 ))
while (( $(date +%s) < deadline )); do
  phase=$(kubectl get pvc "$CANARY_PVC" -n "$CANARY_NAMESPACE" -o jsonpath='{.status.phase}' 2>/dev/null)
  if [[ "$phase" == "Bound" ]]; then
    bound=true
    break
  fi
  sleep 2
done
if [[ "$bound" != "true" ]]; then
  echo "ERROR: canary PVC $CANARY_PVC did not Bind" >&2
  exit 1
fi

kubectl run "$CANARY_POD" --image="$WIN_IMAGE" --restart=Never -n "$CANARY_NAMESPACE" \
  --overrides="{
  \"spec\": {
    \"nodeSelector\": {\"kubernetes.io/hostname\": \"$WINDOWS_NODE\", \"kubernetes.io/os\": \"windows\"},
    \"tolerations\": [{\"operator\": \"Exists\"}],
    \"volumes\": [{\"name\": \"data\", \"persistentVolumeClaim\": {\"claimName\": \"$CANARY_PVC\"}}],
    \"containers\": [{
      \"name\": \"test\",
      \"image\": \"$WIN_IMAGE\",
      \"command\": [\"cmd\", \"/c\", \"ping -t localhost\"],
      \"volumeMounts\": [{\"name\": \"data\", \"mountPath\": \"C:\\\\data\"}]
    }]
  }
}"

running=false
deadline=$(( $(date +%s) + 900 ))
while (( $(date +%s) < deadline )); do
  phase=$(kubectl get pod "$CANARY_POD" -n "$CANARY_NAMESPACE" -o jsonpath='{.status.phase}' 2>/dev/null)
  if [[ "$phase" == "Running" ]]; then
    running=true
    break
  fi
  if [[ "$phase" == "Failed" ]]; then
    break
  fi
  sleep 5
done
if [[ "$running" != "true" ]]; then
  echo "ERROR: canary pod $CANARY_POD did not reach Running" >&2
  exit 1
fi

if ! canary_io setup; then
  echo "ERROR: canary volume IO failed before any drill; lab is not in a testable state" >&2
  exit 1
fi
echo "Canary volume serving IO."

echo ""
echo "=== Drill 1: seaweedfs-node-windows restart; mounted volume keeps serving IO ==="
kubectl delete pod -n "$CSI_NAMESPACE" -l app=seaweedfs-node-windows --wait=false
if ! canary_io drill1-during-restart; then
  echo "ERROR: drill 1 failed: mounted volume stopped serving IO while seaweedfs-node-windows restarted" >&2
  exit 1
fi
kubectl rollout status -n "$CSI_NAMESPACE" ds/seaweedfs-node-windows --timeout=15m
run_health
echo "Drill 1 PASSED"

echo ""
echo "=== Drill 2: kill seaweedfs-mount.exe supervisor; volumes recover ==="
windows_host_ps "Stop-Process -Name seaweedfs-mount -Force"
sleep 5
kubectl rollout status -n "$CSI_NAMESPACE" ds/seaweedfs-mount-windows --timeout=15m
# The supervisor restart tears down WinFsp mounts; the node plugin health
# monitor re-stages them. Poll canary IO until it recovers.
recovered=false
for _ in $(seq 1 60); do
  if canary_io drill2-after-supervisor-restart >/dev/null 2>&1; then
    recovered=true
    break
  fi
  sleep 10
done
if [[ "$recovered" != "true" ]]; then
  echo "ERROR: drill 2 failed: canary volume did not recover after the mount supervisor restart" >&2
  exit 1
fi
run_health
echo "Drill 2 PASSED"

echo ""
echo "=== Drill 3: kubelet restart; node Ready again, mounts intact ==="
windows_host_ps "Restart-Service kubelet -Force"
sleep 10
ready=false
for _ in $(seq 1 30); do
  cond=$(kubectl get node "$WINDOWS_NODE" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)
  if [[ "$cond" == "True" ]]; then
    ready=true
    break
  fi
  sleep 10
done
if [[ "$ready" != "true" ]]; then
  echo "ERROR: drill 3 failed: node $WINDOWS_NODE did not return to Ready after kubelet restart" >&2
  exit 1
fi
if ! canary_io drill3-after-kubelet-restart; then
  echo "ERROR: drill 3 failed: canary volume IO broken after kubelet restart" >&2
  exit 1
fi
run_health
echo "Drill 3 PASSED"

echo ""
echo "ALL RESILIENCE DRILLS PASSED"
