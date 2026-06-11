#!/bin/bash
# SeaweedFS CSI cross-OS storage health matrix for the kind+QEMU lab.
#
# Tests (each counted PASS/FAIL):
#   (a) RWX PVC creates and Binds
#   (b) linux pod writes a marker + 1MiB random blob, computes sha256
#   (c) windows pod reads both; hash equality (2 checks)
#   (d) windows pod writes marker + blob; linux reads both (3 checks)
#   (e) concurrent 60s cross-OS append to separate files, then cross-OS
#       line-count integrity (2 checks; skipped with --quick)
#   (f) windows pod delete leaves no CSI reparse point under the kubelet
#       pods dir and no orphan weed.exe (2 checks; skipped with --quick)
#   (g) a new pod on the same node reads prior data, i.e. remount works
#       (skipped with --quick)
#
# Totals: full cross-OS run = 12, --quick = 7, --skip-cross-os = 3,
# --quick --skip-cross-os = 2.
#
# Windows-origin probes default to --windows-exec hcsdiag: the Windows
# kubelet exec path can fail with tls errors in the QEMU lab, so probes get
# the container ID from pod status, ssh to the Windows node, and run
# powershell via hcsdiag exec (same mechanism as the calico fork's
# ipv6-health-check.sh).
#
# Usage:
#   storage-health-check.sh [--namespace NS] [--storage-class SC]
#     [--linux-node NODE] [--windows-node NODE] [--windows-exec kubectl|hcsdiag]
#     [--linux-image IMG] [--win-image IMG] [--quick] [--skip-cross-os]

set -uo pipefail

export KUBECONFIG="${KUBECONFIG:-/tmp/appmana-calico-kind/kubeconfig}"
LAB_CONTEXT="${LAB_CONTEXT:-kind-appmana-calico}"

NAMESPACE="seaweedfs-csi-test"
STORAGE_CLASS="seaweedfs-storage"
LINUX_NODE="appmana-calico-worker"
WINDOWS_NODE="appmana-000"
WINDOWS_EXEC="hcsdiag"
WINDOWS_SSH_USER="${WINDOWS_SSH_USER:-administrator}"
LINUX_IMAGE="${LINUX_IMAGE:-busybox:1.36}"
WIN_IMAGE="${WIN_IMAGE:-mcr.microsoft.com/windows/servercore:ltsc2022}"
RUN_ID="${RUN_ID:-$(date +%s)-$$}"
CONCURRENT_SECONDS="${CONCURRENT_SECONDS:-60}"
WINDOWS_KUBELET_PODS_DIR="${WINDOWS_KUBELET_PODS_DIR:-C:\\var\\lib\\kubelet\\pods}"
QUICK=false
SKIP_CROSS_OS=false

while [[ $# -gt 0 ]]; do
  case $1 in
    --namespace) NAMESPACE="$2"; shift 2 ;;
    --storage-class) STORAGE_CLASS="$2"; shift 2 ;;
    --linux-node) LINUX_NODE="$2"; shift 2 ;;
    --windows-node) WINDOWS_NODE="$2"; shift 2 ;;
    --windows-exec) WINDOWS_EXEC="$2"; shift 2 ;;
    --linux-image) LINUX_IMAGE="$2"; shift 2 ;;
    --win-image) WIN_IMAGE="$2"; shift 2 ;;
    --quick) QUICK=true; shift ;;
    --skip-cross-os) SKIP_CROSS_OS=true; shift ;;
    *) echo "Usage: $0 [--namespace NS] [--storage-class SC] [--linux-node NODE] [--windows-node NODE] [--windows-exec kubectl|hcsdiag] [--linux-image IMG] [--win-image IMG] [--quick] [--skip-cross-os]" >&2; exit 1 ;;
  esac
done

if [[ "$WINDOWS_EXEC" != "kubectl" && "$WINDOWS_EXEC" != "hcsdiag" ]]; then
  echo "ERROR: --windows-exec must be kubectl or hcsdiag" >&2
  exit 1
fi

current_context=$(kubectl config current-context 2>/dev/null || true)
if [[ "$current_context" != "$LAB_CONTEXT" ]]; then
  echo "ERROR: refusing to run: kubectl context is '${current_context:-<none>}', expected the kind lab context '$LAB_CONTEXT'." >&2
  echo "These scripts must never target the production cluster." >&2
  exit 1
fi

PVC_NAME="swfs-hc-pvc"
LINUX_POD="swfs-hc-linux"
WINDOWS_POD="swfs-hc-windows"
WIN_MOUNT_JSON='C:\\data'
WINDOWS_NODE_IP=""

PASS=0
FAIL=0
TOTAL=0

record() {
  local desc="$1" rc="$2"
  TOTAL=$((TOTAL + 1))
  if [[ "$rc" -eq 0 ]]; then
    echo "$desc: PASS"
    PASS=$((PASS + 1))
  else
    echo "$desc: FAIL"
    FAIL=$((FAIL + 1))
  fi
}

summary() {
  echo ""
  echo "=== Results ==="
  echo "Total: $TOTAL  Pass: $PASS  Fail: $FAIL"
  if [[ $FAIL -eq 0 && $TOTAL -gt 0 ]]; then
    echo "ALL TESTS PASSED"
  else
    echo "SOME TESTS FAILED"
  fi
}

CLEANUP_PODS=()
cleanup() {
  echo ""
  echo "Cleaning up..."
  local podname
  for podname in "${CLEANUP_PODS[@]}"; do
    kubectl delete pod "$podname" -n "$NAMESPACE" --force --grace-period=0 >/dev/null 2>&1 || true
  done
  kubectl delete pvc "$PVC_NAME" -n "$NAMESPACE" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

first_ipv4() {
  tr ' ' '\n' | awk '/^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$/ { print; exit }'
}

linux_pod_overrides() {
  local node="$1"
  cat <<EOF
{
  "spec": {
    "nodeSelector": {"kubernetes.io/hostname": "$node", "kubernetes.io/os": "linux"},
    "volumes": [{"name": "data", "persistentVolumeClaim": {"claimName": "$PVC_NAME"}}],
    "containers": [{
      "name": "test",
      "image": "$LINUX_IMAGE",
      "command": ["sh", "-c", "while true; do sleep 3600; done"],
      "volumeMounts": [{"name": "data", "mountPath": "/data"}]
    }]
  }
}
EOF
}

windows_pod_overrides() {
  local node="$1"
  cat <<EOF
{
  "spec": {
    "nodeSelector": {"kubernetes.io/hostname": "$node", "kubernetes.io/os": "windows"},
    "tolerations": [{"operator": "Exists"}],
    "volumes": [{"name": "data", "persistentVolumeClaim": {"claimName": "$PVC_NAME"}}],
    "containers": [{
      "name": "test",
      "image": "$WIN_IMAGE",
      "command": ["cmd", "/c", "ping -t localhost"],
      "volumeMounts": [{"name": "data", "mountPath": "$WIN_MOUNT_JSON"}]
    }]
  }
}
EOF
}

create_pod() {
  local podname="$1" os="$2" node="$3" image overrides
  if [[ "$os" == "windows" ]]; then
    image="$WIN_IMAGE"
    overrides=$(windows_pod_overrides "$node")
  else
    image="$LINUX_IMAGE"
    overrides=$(linux_pod_overrides "$node")
  fi
  CLEANUP_PODS+=("$podname")
  kubectl run "$podname" --image="$image" --restart=Never -n "$NAMESPACE" \
    --overrides="$overrides"
}

wait_pod_running() {
  local podname="$1" timeout="$2" start phase
  start=$(date +%s)
  while true; do
    phase=$(kubectl get pod "$podname" -n "$NAMESPACE" -o jsonpath='{.status.phase}' 2>/dev/null)
    if [[ "$phase" == "Running" ]]; then
      return 0
    fi
    if [[ "$phase" == "Failed" || "$phase" == "Succeeded" ]]; then
      return 1
    fi
    if (( $(date +%s) - start >= timeout )); then
      return 1
    fi
    sleep 5
  done
}

wait_pvc_bound() {
  local timeout="$1" start phase
  start=$(date +%s)
  while true; do
    phase=$(kubectl get pvc "$PVC_NAME" -n "$NAMESPACE" -o jsonpath='{.status.phase}' 2>/dev/null)
    if [[ "$phase" == "Bound" ]]; then
      return 0
    fi
    if (( $(date +%s) - start >= timeout )); then
      return 1
    fi
    sleep 2
  done
}

pod_cid() {
  local podname="$1" cid
  cid=$(kubectl get pod "$podname" -n "$NAMESPACE" -o jsonpath='{.status.containerStatuses[0].containerID}' 2>/dev/null)
  printf '%s' "${cid#containerd://}"
}

# Run powershell inside a Windows pod. With hcsdiag, the probe goes over ssh
# to the node and into the container via hcsdiag exec.
windows_pod_ps() {
  local podname="$1" script="$2" cid encoded
  if [[ "$WINDOWS_EXEC" == "hcsdiag" ]]; then
    cid=$(pod_cid "$podname")
    if [[ -z "$cid" || -z "$WINDOWS_NODE_IP" ]]; then
      return 1
    fi
    encoded=$(printf '%s' "$script" | iconv -f UTF-8 -t UTF-16LE | base64 -w0)
    ssh -o StrictHostKeyChecking=no -o ConnectTimeout=5 \
      "$WINDOWS_SSH_USER@$WINDOWS_NODE_IP" \
      "hcsdiag exec $cid powershell -NoProfile -EncodedCommand $encoded"
  else
    kubectl exec "$podname" -n "$NAMESPACE" -- powershell -NoProfile -Command "$script"
  fi
}

windows_pod_ps_quiet() {
  windows_pod_ps "$@" >/dev/null 2>&1
}

windows_pod_ps_out() {
  windows_pod_ps "$@" 2>/dev/null | tr -d '\r' | tail -n 1
}

# Run powershell on the Windows host (node-level assertions, not in-pod).
windows_host_ps() {
  local script="$1"
  ssh -o StrictHostKeyChecking=no -o ConnectTimeout=5 \
    "$WINDOWS_SSH_USER@$WINDOWS_NODE_IP" \
    "powershell -NoProfile -Command \"$script\"" 2>/dev/null | tr -d '\r'
}

echo ""
echo "=== SeaweedFS CSI Storage Health Check ==="
echo "Namespace:     $NAMESPACE"
echo "StorageClass:  $STORAGE_CLASS"
echo "Linux node:    $LINUX_NODE"
if [[ "$SKIP_CROSS_OS" == "true" ]]; then
  echo "Windows node:  (skipped, --skip-cross-os)"
else
  echo "Windows node:  $WINDOWS_NODE (exec via $WINDOWS_EXEC)"
fi
echo "Run id:        $RUN_ID"
echo ""

if [[ "$SKIP_CROSS_OS" != "true" ]]; then
  node_ips=$(kubectl get node "$WINDOWS_NODE" -o jsonpath='{range .status.addresses[?(@.type=="InternalIP")]}{.address}{" "}{end}' 2>/dev/null)
  WINDOWS_NODE_IP=$(first_ipv4 <<<"$node_ips")
  if [[ "$WINDOWS_EXEC" == "hcsdiag" && -z "$WINDOWS_NODE_IP" ]]; then
    echo "ERROR: could not determine IPv4 InternalIP for Windows node $WINDOWS_NODE" >&2
    exit 1
  fi
fi

kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f - >/dev/null

echo "=== Provisioning ==="
kubectl apply -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: $PVC_NAME
  namespace: $NAMESPACE
spec:
  accessModes:
    - ReadWriteMany
  storageClassName: $STORAGE_CLASS
  resources:
    requests:
      storage: 1Gi
EOF
wait_pvc_bound 120
record "RWX PVC $PVC_NAME Binds via $STORAGE_CLASS" $?
if [[ $FAIL -gt 0 ]]; then
  echo "ERROR: PVC did not Bind; nothing else can run" >&2
  summary
  exit 1
fi

create_pod "$LINUX_POD" linux "$LINUX_NODE"
if [[ "$SKIP_CROSS_OS" != "true" ]]; then
  create_pod "$WINDOWS_POD" windows "$WINDOWS_NODE"
fi

if ! wait_pod_running "$LINUX_POD" 300; then
  echo "ERROR: linux pod $LINUX_POD did not reach Running" >&2
  summary
  exit 1
fi
if [[ "$SKIP_CROSS_OS" != "true" ]] && ! wait_pod_running "$WINDOWS_POD" 900; then
  echo "ERROR: windows pod $WINDOWS_POD did not reach Running" >&2
  summary
  exit 1
fi

echo ""
echo "=== Cross-OS Write/Read ==="
linux_hash=$(
  kubectl exec "$LINUX_POD" -n "$NAMESPACE" -- sh -c \
    "echo marker-linux-$RUN_ID > /data/marker-linux.txt && dd if=/dev/urandom of=/data/linux-blob.bin bs=1024 count=1024 2>/dev/null && sync && sha256sum /data/linux-blob.bin" 2>/dev/null |
    awk '{print $1}' | tail -n 1
)
[[ "$linux_hash" =~ ^[0-9a-f]{64}$ ]]
record "linux pod writes marker + 1MiB blob on $LINUX_NODE" $?

if [[ "$SKIP_CROSS_OS" != "true" ]]; then
  windows_pod_ps_quiet "$WINDOWS_POD" \
    "if ((Get-Content C:\data\marker-linux.txt -ErrorAction SilentlyContinue) -match 'marker-linux-$RUN_ID') { exit 0 } else { exit 1 }"
  record "windows pod reads linux marker" $?

  win_read_hash=$(windows_pod_ps_out "$WINDOWS_POD" \
    "(Get-FileHash C:\data\linux-blob.bin -Algorithm SHA256).Hash.ToLower()")
  [[ -n "$linux_hash" && "$win_read_hash" == "$linux_hash" ]]
  record "windows pod sha256(linux-blob) matches linux writer" $?

  win_write_hash=$(windows_pod_ps_out "$WINDOWS_POD" \
    "Set-Content -Path C:\data\marker-windows.txt -Value 'marker-windows-$RUN_ID'; \$bytes = New-Object byte[] 1048576; (New-Object Random).NextBytes(\$bytes); [IO.File]::WriteAllBytes('C:\data\windows-blob.bin', \$bytes); (Get-FileHash C:\data\windows-blob.bin -Algorithm SHA256).Hash.ToLower()")
  [[ "$win_write_hash" =~ ^[0-9a-f]{64}$ ]]
  record "windows pod writes marker + 1MiB blob on $WINDOWS_NODE" $?

  kubectl exec "$LINUX_POD" -n "$NAMESPACE" -- \
    grep -q "marker-windows-$RUN_ID" /data/marker-windows.txt >/dev/null 2>&1
  record "linux pod reads windows marker" $?

  linux_read_hash=$(
    kubectl exec "$LINUX_POD" -n "$NAMESPACE" -- sh -c "sha256sum /data/windows-blob.bin" 2>/dev/null |
      awk '{print $1}' | tail -n 1
  )
  [[ -n "$win_write_hash" && "$linux_read_hash" == "$win_write_hash" ]]
  record "linux pod sha256(windows-blob) matches windows writer" $?
fi

if [[ "$QUICK" != "true" && "$SKIP_CROSS_OS" != "true" ]]; then
  echo ""
  echo "=== Concurrent Cross-OS Append (${CONCURRENT_SECONDS}s) ==="
  kubectl exec "$LINUX_POD" -n "$NAMESPACE" -- sh -c \
    "i=0; while [ \$i -lt $CONCURRENT_SECONDS ]; do i=\$((i+1)); echo linux-\$i >> /data/concurrent-linux.log; sleep 1; done" >/dev/null 2>&1 &
  linux_writer_pid=$!
  windows_pod_ps_quiet "$WINDOWS_POD" \
    "1..$CONCURRENT_SECONDS | ForEach-Object { Add-Content -Path C:\data\concurrent-windows.log -Value \"windows-\$_\"; Start-Sleep -Seconds 1 }" &
  windows_writer_pid=$!
  wait "$linux_writer_pid"
  linux_writer_rc=$?
  wait "$windows_writer_pid"
  windows_writer_rc=$?

  win_count=$(windows_pod_ps_out "$WINDOWS_POD" "(Get-Content C:\data\concurrent-linux.log).Count")
  [[ $linux_writer_rc -eq 0 && "$win_count" == "$CONCURRENT_SECONDS" ]]
  record "concurrent append: windows reads $CONCURRENT_SECONDS lines from linux log" $?

  linux_count=$(kubectl exec "$LINUX_POD" -n "$NAMESPACE" -- sh -c "wc -l < /data/concurrent-windows.log" 2>/dev/null | tr -d '[:space:]')
  [[ $windows_writer_rc -eq 0 && "$linux_count" == "$CONCURRENT_SECONDS" ]]
  record "concurrent append: linux reads $CONCURRENT_SECONDS lines from windows log" $?
fi

if [[ "$QUICK" != "true" && "$SKIP_CROSS_OS" != "true" ]]; then
  echo ""
  echo "=== Unmount / Cleanup on Pod Delete ==="
  pod_uid=$(kubectl get pod "$WINDOWS_POD" -n "$NAMESPACE" -o jsonpath='{.metadata.uid}' 2>/dev/null)
  weed_before=$(windows_host_ps "(Get-Process weed -ErrorAction SilentlyContinue | Measure-Object).Count # weed-count-before-delete" | tail -n 1 | tr -d '[:space:]')

  kubectl delete pod "$WINDOWS_POD" -n "$NAMESPACE" --wait=true --timeout=300s >/dev/null 2>&1
  delete_rc=$?

  reparse_gone=1
  for _ in $(seq 1 12); do
    leftover=$(windows_host_ps "Test-Path '${WINDOWS_KUBELET_PODS_DIR}\\$pod_uid\\volumes\\kubernetes.io~csi'" | tail -n 1 | tr -d '[:space:]')
    if [[ "$leftover" == "False" ]]; then
      reparse_gone=0
      break
    fi
    sleep 10
  done
  [[ $delete_rc -eq 0 && $reparse_gone -eq 0 ]]
  record "windows pod delete leaves no CSI reparse point under kubelet pods dir" $?

  orphan_rc=1
  weed_after=""
  for _ in $(seq 1 6); do
    weed_after=$(windows_host_ps "(Get-Process weed -ErrorAction SilentlyContinue | Measure-Object).Count # weed-count-after-delete" | tail -n 1 | tr -d '[:space:]')
    if [[ "$weed_before" =~ ^[0-9]+$ && "$weed_after" =~ ^[0-9]+$ ]] && (( weed_after < weed_before )); then
      orphan_rc=0
      break
    fi
    sleep 5
  done
  record "windows pod delete leaves no orphan weed.exe (before=${weed_before:-?} after=${weed_after:-?})" $orphan_rc
fi

if [[ "$QUICK" != "true" ]]; then
  echo ""
  echo "=== Remount ==="
  if [[ "$SKIP_CROSS_OS" != "true" ]]; then
    REMOUNT_POD="swfs-hc-windows-remount"
    create_pod "$REMOUNT_POD" windows "$WINDOWS_NODE"
    if wait_pod_running "$REMOUNT_POD" 900; then
      windows_pod_ps_quiet "$REMOUNT_POD" \
        "if ((Get-Content C:\data\marker-linux.txt -ErrorAction SilentlyContinue) -match 'marker-linux-$RUN_ID') { exit 0 } else { exit 1 }"
      record "new windows pod on $WINDOWS_NODE reads prior data after remount" $?
    else
      record "new windows pod on $WINDOWS_NODE reads prior data after remount" 1
    fi
  else
    REMOUNT_POD="swfs-hc-linux-remount"
    create_pod "$REMOUNT_POD" linux "$LINUX_NODE"
    if wait_pod_running "$REMOUNT_POD" 300; then
      kubectl exec "$REMOUNT_POD" -n "$NAMESPACE" -- \
        grep -q "marker-linux-$RUN_ID" /data/marker-linux.txt >/dev/null 2>&1
      record "new linux pod on $LINUX_NODE reads prior data after remount" $?
    else
      record "new linux pod on $LINUX_NODE reads prior data after remount" 1
    fi
  fi
fi

summary
if [[ $FAIL -eq 0 && $TOTAL -gt 0 ]]; then
  exit 0
else
  exit 1
fi
