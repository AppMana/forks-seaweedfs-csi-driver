#!/bin/bash
# Mocked tests for the AppMana kind/QEMU SeaweedFS CSI lab scripts.
#
# Mocks kubectl, ssh, iconv, base64, and sleep on PATH (same approach as the
# calico fork's hack/appmana/tests harness) so changes to command
# construction are caught without QEMU or kind. Every script is also passed
# through bash -n.

set -uo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd -- "$SCRIPT_DIR/../../.." && pwd)
HACK="$REPO_ROOT/hack/appmana"

TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

MOCKBIN="$TMPDIR/bin"
LOG="$TMPDIR/commands.log"
mkdir -p "$MOCKBIN"
: > "$LOG"

PASSED=0
FAILED=0

pass() {
  echo "ok: $*"
  PASSED=$((PASSED + 1))
}

fail() {
  echo "FAILED: $*" >&2
  FAILED=$((FAILED + 1))
}

assert_log_contains() {
  local pattern="$1"
  if grep -Fq -- "$pattern" "$LOG"; then
    pass "log contains: $pattern"
  else
    fail "log missing: $pattern"
    echo "--- command log ---" >&2
    cat "$LOG" >&2
  fi
}

assert_log_not_contains() {
  local pattern="$1"
  if grep -Fq -- "$pattern" "$LOG"; then
    fail "log unexpectedly contains: $pattern"
    echo "--- command log ---" >&2
    cat "$LOG" >&2
  else
    pass "log does not contain: $pattern"
  fi
}

assert_file_contains() {
  local file="$1" pattern="$2"
  if grep -Fq -- "$pattern" "$file"; then
    pass "$(basename "$file") contains: $pattern"
  else
    fail "$(basename "$file") missing: $pattern"
    echo "--- $file ---" >&2
    cat "$file" >&2
  fi
}

write_mock() {
  local name="$1"
  shift
  cat > "$MOCKBIN/$name"
  chmod +x "$MOCKBIN/$name"
}

# Stable fake content hashes; the linux-side and windows-side mocks return
# the same value so cross-OS hash comparisons pass unless mismatch is
# injected with APP_MOCK_HASH_MISMATCH=true.
HASH_LINUX_BLOB="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
HASH_WINDOWS_BLOB="bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
HASH_MISMATCH="cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

write_mock kubectl <<'EOF'
#!/bin/bash
set -euo pipefail
echo "kubectl $*" >> "$APP_MOCK_LOG"

args=("$@")
if [[ "${args[0]:-}" == "--kubeconfig" ]]; then
  args=("${args[@]:2}")
fi
if [[ "${args[0]:-}" == "-n" ]]; then
  args=("${args[@]:2}")
fi
joined=" ${args[*]} "

if [[ "${args[0]:-}" == "config" && "${args[1]:-}" == "current-context" ]]; then
  echo "${APP_MOCK_CONTEXT:-kind-appmana-calico}"
  exit 0
fi

if [[ "${args[0]:-}" == "create" && "${args[1]:-}" == "namespace" ]]; then
  echo 'apiVersion: v1'
  echo 'kind: Namespace'
  echo "metadata: {name: ${args[2]}}"
  exit 0
fi

if [[ "${args[0]:-}" == "apply" ]]; then
  # Capture the applied manifest into the log so content assertions work.
  cat >> "$APP_MOCK_LOG"
  echo "applied"
  exit 0
fi

if [[ "${args[0]:-}" == "delete" ]]; then
  exit 0
fi

if [[ "${args[0]:-}" == "wait" ]]; then
  echo "condition met"
  exit 0
fi

if [[ "${args[0]:-}" == "rollout" ]]; then
  echo "rollout ok"
  exit 0
fi

if [[ "${args[0]:-}" == "logs" ]]; then
  echo "filer-ok"
  exit 0
fi

if [[ "${args[0]:-}" == "run" ]]; then
  echo "pod/${args[1]} created"
  exit 0
fi

if [[ "${args[0]:-}" == "get" && "${args[1]:-}" == "node" ]]; then
  node="${args[2]}"
  if [[ "$joined" == *'@.type=="Ready"'* ]]; then
    echo -n "True"
    exit 0
  fi
  if [[ "$joined" == *"InternalIP"* ]]; then
    case "$node" in
      appmana-000) echo -n "10.2.0.180 2001:db8::180" ;;
      appmana-calico-worker) echo -n "172.21.0.3" ;;
      appmana-calico-worker2) echo -n "172.21.0.2" ;;
      *) echo -n "172.21.0.9" ;;
    esac
    exit 0
  fi
fi

if [[ "${args[0]:-}" == "get" && "${args[1]:-}" == "csinode" ]]; then
  if [[ "${APP_MOCK_CSINODE_EMPTY:-false}" == "true" ]]; then
    echo -n ""
  else
    echo -n "seaweedfs-csi-driver"
  fi
  exit 0
fi

if [[ "${args[0]:-}" == "get" && "${args[1]:-}" == "pvc" ]]; then
  echo -n "Bound"
  exit 0
fi

if [[ "${args[0]:-}" == "get" && "${args[1]:-}" == "pod" ]]; then
  pod="${args[2]}"
  if [[ "$joined" == *".status.phase"* ]]; then
    echo -n "Running"
    exit 0
  fi
  if [[ "$joined" == *".metadata.uid"* ]]; then
    echo -n "11111111-2222-3333-4444-555555555555"
    exit 0
  fi
  if [[ "$joined" == *"containerStatuses[0].containerID"* ]]; then
    if [[ "$pod" == *windows* || "$pod" == *canary* ]]; then
      echo -n "containerd://wincid"
    else
      echo -n "containerd://linuxcid"
    fi
    exit 0
  fi
fi

if [[ "${args[0]:-}" == "exec" ]]; then
  if [[ "$joined" == *"sha256sum /data/linux-blob.bin"* ]]; then
    echo "$APP_MOCK_HASH_LINUX_BLOB  /data/linux-blob.bin"
    exit 0
  fi
  if [[ "$joined" == *"sha256sum /data/windows-blob.bin"* ]]; then
    echo "$APP_MOCK_HASH_WINDOWS_BLOB  /data/windows-blob.bin"
    exit 0
  fi
  if [[ "$joined" == *"wc -l < /data/concurrent-windows.log"* ]]; then
    echo "60"
    exit 0
  fi
  if [[ "$joined" == *"wget"* ]]; then
    echo "ok"
    exit 0
  fi
  exit 0
fi

echo "unhandled kubectl $*" >&2
exit 1
EOF

write_mock ssh <<'EOF'
#!/bin/bash
echo "ssh $*" >> "$APP_MOCK_LOG"
joined="$*"

# CSI preflight host status block.
if [[ "$joined" == *"Get-Service WinFsp.Launcher"* ]]; then
  if [[ "${APP_MOCK_CSI_READY:-true}" == "true" ]]; then
    printf 'winfsp_launcher=True\r\nmount_sock=True\r\ncsi_sock=True\r\n'
  else
    printf 'winfsp_launcher=False\r\nmount_sock=False\r\ncsi_sock=False\r\n'
  fi
  exit 0
fi

# In-pod hash probes (EncodedCommand passes through the mocked iconv/base64
# as plain text, so the powershell source is visible in the args).
if [[ "$joined" == *"Get-FileHash C:\data\linux-blob.bin"* ]]; then
  if [[ "${APP_MOCK_HASH_MISMATCH:-false}" == "true" ]]; then
    echo "$APP_MOCK_HASH_MISMATCH_VALUE"
  else
    echo "$APP_MOCK_HASH_LINUX_BLOB"
  fi
  exit 0
fi
if [[ "$joined" == *"Get-FileHash C:\data\windows-blob.bin"* ]]; then
  echo "$APP_MOCK_HASH_WINDOWS_BLOB"
  exit 0
fi
if [[ "$joined" == *"concurrent-linux.log).Count"* ]]; then
  echo "60"
  exit 0
fi

# Host-level assertions for the unmount/cleanup test.
if [[ "$joined" == *"# weed-count-before-delete"* ]]; then
  echo "1"
  exit 0
fi
if [[ "$joined" == *"# weed-count-after-delete"* ]]; then
  echo "0"
  exit 0
fi
if [[ "$joined" == *"kubernetes.io~csi"* ]]; then
  echo "False"
  exit 0
fi

# Resilience drills.
if [[ "$joined" == *"Stop-Process -Name seaweedfs-mount"* ]]; then
  exit 0
fi
if [[ "$joined" == *"Restart-Service kubelet"* ]]; then
  exit 0
fi

# Everything else (marker reads/writes, appenders, canary IO) succeeds.
exit 0
EOF

write_mock iconv <<'EOF'
#!/bin/bash
cat
EOF

write_mock base64 <<'EOF'
#!/bin/bash
tr -d '\n'
EOF

write_mock sleep <<'EOF'
#!/bin/bash
exit 0
EOF

PATH="$MOCKBIN:$PATH"
export PATH APP_MOCK_LOG="$LOG" KUBECONFIG="$TMPDIR/kubeconfig"
export APP_MOCK_HASH_LINUX_BLOB="$HASH_LINUX_BLOB"
export APP_MOCK_HASH_WINDOWS_BLOB="$HASH_WINDOWS_BLOB"
export APP_MOCK_HASH_MISMATCH_VALUE="$HASH_MISMATCH"
touch "$KUBECONFIG"

SCRIPTS=(
  deploy-seaweedfs-kind.sh
  deploy-csi-driver-kind.sh
  check-kind-qemu-csi-ready.sh
  storage-health-check.sh
  run-kind-qemu-csi-resilience.sh
  run-kind-qemu-storage-e2e.sh
  tests/run-appmana-script-tests.sh
)
for s in "${SCRIPTS[@]}"; do
  if bash -n "$HACK/$s"; then
    pass "bash -n $s"
  else
    fail "bash -n $s"
  fi
done

echo ""
echo "--- deploy-seaweedfs-kind.sh (deploy path) ---"
: > "$LOG"
if bash "$HACK/deploy-seaweedfs-kind.sh" > "$TMPDIR/deploy-seaweedfs.out" 2>&1; then
  pass "deploy-seaweedfs-kind.sh exits 0"
else
  fail "deploy-seaweedfs-kind.sh exited nonzero"
  cat "$TMPDIR/deploy-seaweedfs.out" >&2
fi
assert_log_contains "image: chrislusf/seaweedfs:4.23_large_disk"
assert_log_contains "- -master.volumeSizeLimitMB=64"
assert_log_contains "- -volume.max=100"
assert_log_contains "hostNetwork: true"
assert_log_contains "containerPort: 18888"
assert_log_contains "kubectl wait -n seaweedfs-test --for=condition=Ready pod/seaweedfs --timeout=300s"
assert_log_contains "kubectl wait -n seaweedfs-test --for=jsonpath={.status.phase}=Succeeded pod/swfs-filer-probe --timeout=240s"
assert_log_contains "nc -z -w 3 172.21.0.2 18888"
assert_file_contains "$TMPDIR/deploy-seaweedfs.out" "FILER=172.21.0.2:8888"

echo ""
echo "--- deploy-seaweedfs-kind.sh (--filer skip path) ---"
: > "$LOG"
if bash "$HACK/deploy-seaweedfs-kind.sh" --filer 10.2.0.55:8888 > "$TMPDIR/deploy-seaweedfs-skip.out" 2>&1; then
  pass "deploy-seaweedfs-kind.sh --filer exits 0"
else
  fail "deploy-seaweedfs-kind.sh --filer exited nonzero"
  cat "$TMPDIR/deploy-seaweedfs-skip.out" >&2
fi
assert_log_not_contains "image: chrislusf/seaweedfs:4.23_large_disk"
assert_log_contains "nc -z -w 3 10.2.0.55 18888"
assert_file_contains "$TMPDIR/deploy-seaweedfs-skip.out" "FILER=10.2.0.55:8888"

echo ""
echo "--- context guard refuses non-lab contexts ---"
: > "$LOG"
if APP_MOCK_CONTEXT="prod-cluster" bash "$HACK/deploy-seaweedfs-kind.sh" > "$TMPDIR/guard.out" 2>&1; then
  fail "deploy-seaweedfs-kind.sh ran against a non-lab context"
else
  pass "deploy-seaweedfs-kind.sh refused a non-lab context"
fi
assert_file_contains "$TMPDIR/guard.out" "refusing to run"
assert_file_contains "$TMPDIR/guard.out" "kind-appmana-calico"
: > "$LOG"
if APP_MOCK_CONTEXT="prod-cluster" bash "$HACK/storage-health-check.sh" > "$TMPDIR/guard-health.out" 2>&1; then
  fail "storage-health-check.sh ran against a non-lab context"
else
  pass "storage-health-check.sh refused a non-lab context"
fi

echo ""
echo "--- deploy-csi-driver-kind.sh ---"
: > "$LOG"
if bash "$HACK/deploy-csi-driver-kind.sh" \
  --csi-image ghcr.io/appmana/seaweedfs-csi-driver:test \
  --mount-image ghcr.io/appmana/seaweedfs-mount:test \
  --filer 172.21.0.2:8888 > "$TMPDIR/deploy-csi.out" 2>&1; then
  pass "deploy-csi-driver-kind.sh exits 0"
else
  fail "deploy-csi-driver-kind.sh exited nonzero"
  cat "$TMPDIR/deploy-csi.out" >&2
fi
assert_log_contains "provisioner: seaweedfs-csi-driver"
assert_log_contains "name: seaweedfs-storage"
assert_log_contains "kind: CSIDriver"
assert_log_contains "- --components=controller"
assert_log_contains "- --components=node"
assert_log_contains "image: ghcr.io/appmana/seaweedfs-csi-driver:test"
assert_log_contains "image: ghcr.io/appmana/seaweedfs-mount:test"
assert_log_contains 'value: "172.21.0.2:8888"'
assert_log_contains "kubernetes.io/os: windows"
assert_log_contains "hostProcess: true"
assert_log_contains "winfsp.msi"
assert_log_contains 'value: unix://C:\var\lib\kubelet\plugins\seaweedfs-csi-driver\csi.sock'
assert_log_contains 'value: C:\\var\\lib\\kubelet\\plugins\\seaweedfs-csi-driver\\csi.sock'
assert_log_contains 'value: C:\\var\\lib\\kubelet\\plugins_registry\\'
assert_log_contains "path: /var/lib/kubelet/pods"
assert_log_contains "path: /var/lib/kubelet/plugins/seaweedfs-csi-driver"
assert_log_not_contains "appmana-026"
assert_log_contains "kubectl rollout status -n seaweedfs-csi deployment/seaweedfs-controller --timeout=10m"
assert_log_contains "kubectl rollout status -n seaweedfs-csi ds/seaweedfs-mount --timeout=10m"
assert_log_contains "kubectl rollout status -n seaweedfs-csi ds/seaweedfs-node --timeout=10m"
assert_log_contains "kubectl rollout status -n seaweedfs-csi ds/seaweedfs-mount-windows --timeout=20m"
assert_log_contains "kubectl rollout status -n seaweedfs-csi ds/seaweedfs-node-windows --timeout=20m"

echo ""
echo "--- check-kind-qemu-csi-ready.sh (ready) ---"
: > "$LOG"
if bash "$HACK/check-kind-qemu-csi-ready.sh" > "$TMPDIR/csi-ready.out" 2>&1; then
  pass "check-kind-qemu-csi-ready.sh exits 0 when ready"
else
  fail "check-kind-qemu-csi-ready.sh exited nonzero when ready"
  cat "$TMPDIR/csi-ready.out" >&2
fi
assert_file_contains "$TMPDIR/csi-ready.out" "controller_ready=True"
assert_file_contains "$TMPDIR/csi-ready.out" "windows_node_ready=True"
assert_file_contains "$TMPDIR/csi-ready.out" "csinode_driver=True"
assert_file_contains "$TMPDIR/csi-ready.out" "winfsp_launcher=True"
assert_file_contains "$TMPDIR/csi-ready.out" "mount_sock=True"
assert_file_contains "$TMPDIR/csi-ready.out" "csi_sock=True"
assert_log_contains "kubectl --kubeconfig $KUBECONFIG -n seaweedfs-csi wait --for=condition=Ready pod -l app=seaweedfs-node-windows --timeout=30s"
assert_log_contains "kubectl --kubeconfig $KUBECONFIG get csinode appmana-000"
assert_log_contains "ssh -o StrictHostKeyChecking=no -o ConnectTimeout=5 administrator@10.2.0.180"

echo ""
echo "--- check-kind-qemu-csi-ready.sh (not ready) ---"
: > "$LOG"
if APP_MOCK_CSI_READY=false bash "$HACK/check-kind-qemu-csi-ready.sh" > "$TMPDIR/csi-not-ready.out" 2>&1; then
  fail "check-kind-qemu-csi-ready.sh passed with missing host artifacts"
  cat "$TMPDIR/csi-not-ready.out" >&2
else
  pass "check-kind-qemu-csi-ready.sh failed with missing host artifacts"
fi
assert_file_contains "$TMPDIR/csi-not-ready.out" "winfsp_launcher=False"
assert_file_contains "$TMPDIR/csi-not-ready.out" "ERROR: SeaweedFS CSI preflight failed"
assert_file_contains "$TMPDIR/csi-not-ready.out" "This is a bootstrap failure, not a health-matrix result."

: > "$LOG"
if APP_MOCK_CSINODE_EMPTY=true bash "$HACK/check-kind-qemu-csi-ready.sh" > "$TMPDIR/csi-no-csinode.out" 2>&1; then
  fail "check-kind-qemu-csi-ready.sh passed with no csinode driver entry"
else
  pass "check-kind-qemu-csi-ready.sh failed with no csinode driver entry"
fi
assert_file_contains "$TMPDIR/csi-no-csinode.out" "csinode_driver=False"

echo ""
echo "--- storage-health-check.sh (full matrix) ---"
: > "$LOG"
if RUN_ID=mock bash "$HACK/storage-health-check.sh" > "$TMPDIR/health-full.out" 2>&1; then
  pass "storage-health-check.sh exits 0"
else
  fail "storage-health-check.sh exited nonzero"
  cat "$TMPDIR/health-full.out" >&2
fi
assert_file_contains "$TMPDIR/health-full.out" "Total: 12  Pass: 12  Fail: 0"
assert_file_contains "$TMPDIR/health-full.out" "ALL TESTS PASSED"
assert_file_contains "$TMPDIR/health-full.out" "RWX PVC swfs-hc-pvc Binds via seaweedfs-storage: PASS"
assert_file_contains "$TMPDIR/health-full.out" "windows pod sha256(linux-blob) matches linux writer: PASS"
assert_file_contains "$TMPDIR/health-full.out" "linux pod sha256(windows-blob) matches windows writer: PASS"
assert_file_contains "$TMPDIR/health-full.out" "windows pod delete leaves no orphan weed.exe (before=1 after=0): PASS"
assert_file_contains "$TMPDIR/health-full.out" "new windows pod on appmana-000 reads prior data after remount: PASS"
assert_log_contains "ReadWriteMany"
assert_log_contains "storageClassName: seaweedfs-storage"
assert_log_contains '"kubernetes.io/os": "windows"'
assert_log_contains '"kubernetes.io/os": "linux"'
assert_log_contains '"mountPath": "C:\\data"'
assert_log_contains "hcsdiag exec wincid powershell -NoProfile -EncodedCommand"
assert_log_contains "ssh -o StrictHostKeyChecking=no -o ConnectTimeout=5 administrator@10.2.0.180"
assert_log_contains "kubernetes.io~csi"

echo ""
echo "--- storage-health-check.sh (--quick) ---"
: > "$LOG"
if RUN_ID=mock bash "$HACK/storage-health-check.sh" --quick > "$TMPDIR/health-quick.out" 2>&1; then
  pass "storage-health-check.sh --quick exits 0"
else
  fail "storage-health-check.sh --quick exited nonzero"
  cat "$TMPDIR/health-quick.out" >&2
fi
assert_file_contains "$TMPDIR/health-quick.out" "Total: 7  Pass: 7  Fail: 0"
assert_file_contains "$TMPDIR/health-quick.out" "ALL TESTS PASSED"

echo ""
echo "--- storage-health-check.sh (--skip-cross-os) ---"
: > "$LOG"
if RUN_ID=mock bash "$HACK/storage-health-check.sh" --skip-cross-os > "$TMPDIR/health-linux.out" 2>&1; then
  pass "storage-health-check.sh --skip-cross-os exits 0"
else
  fail "storage-health-check.sh --skip-cross-os exited nonzero"
  cat "$TMPDIR/health-linux.out" >&2
fi
assert_file_contains "$TMPDIR/health-linux.out" "Total: 3  Pass: 3  Fail: 0"
assert_log_not_contains "hcsdiag"

echo ""
echo "--- storage-health-check.sh (hash mismatch fails) ---"
: > "$LOG"
if RUN_ID=mock APP_MOCK_HASH_MISMATCH=true bash "$HACK/storage-health-check.sh" --quick > "$TMPDIR/health-mismatch.out" 2>&1; then
  fail "storage-health-check.sh passed despite a cross-OS hash mismatch"
  cat "$TMPDIR/health-mismatch.out" >&2
else
  pass "storage-health-check.sh failed on cross-OS hash mismatch"
fi
assert_file_contains "$TMPDIR/health-mismatch.out" "windows pod sha256(linux-blob) matches linux writer: FAIL"
assert_file_contains "$TMPDIR/health-mismatch.out" "SOME TESTS FAILED"

echo ""
echo "--- run-kind-qemu-csi-resilience.sh ---"
HEALTH_STUB="$TMPDIR/mock-health.sh"
cat > "$HEALTH_STUB" <<'EOF'
#!/bin/bash
echo "health $*" >> "$APP_MOCK_LOG"
exit 0
EOF
chmod +x "$HEALTH_STUB"
: > "$LOG"
if HEALTH_CHECK_SCRIPT="$HEALTH_STUB" bash "$HACK/run-kind-qemu-csi-resilience.sh" > "$TMPDIR/resilience.out" 2>&1; then
  pass "run-kind-qemu-csi-resilience.sh exits 0"
else
  fail "run-kind-qemu-csi-resilience.sh exited nonzero"
  cat "$TMPDIR/resilience.out" >&2
fi
assert_file_contains "$TMPDIR/resilience.out" "Drill 1 PASSED"
assert_file_contains "$TMPDIR/resilience.out" "Drill 2 PASSED"
assert_file_contains "$TMPDIR/resilience.out" "Drill 3 PASSED"
assert_file_contains "$TMPDIR/resilience.out" "ALL RESILIENCE DRILLS PASSED"
assert_log_contains "kubectl delete pod -n seaweedfs-csi -l app=seaweedfs-node-windows --wait=false"
assert_log_contains "kubectl rollout status -n seaweedfs-csi ds/seaweedfs-node-windows --timeout=15m"
assert_log_contains "kubectl rollout status -n seaweedfs-csi ds/seaweedfs-mount-windows --timeout=15m"
assert_log_contains "Stop-Process -Name seaweedfs-mount -Force"
assert_log_contains "Restart-Service kubelet -Force"
assert_log_contains "hcsdiag exec wincid powershell -NoProfile -EncodedCommand"
health_runs=$(grep -c '^health .*--quick' "$LOG" || true)
if [[ "$health_runs" == "3" ]]; then
  pass "resilience ran the quick health matrix after each of the 3 drills"
else
  fail "expected 3 quick health runs, got $health_runs"
  cat "$LOG" >&2
fi

echo ""
echo "--- run-kind-qemu-storage-e2e.sh (orchestrator) ---"
FORWARDING_STUB="$TMPDIR/mock-forwarding.sh"
PREFLIGHT_STUB="$TMPDIR/mock-preflight.sh"
CSI_PREFLIGHT_STUB="$TMPDIR/mock-csi-preflight.sh"
DEPLOY_SEAWEEDFS_STUB="$TMPDIR/mock-deploy-seaweedfs.sh"
DEPLOY_CSI_STUB="$TMPDIR/mock-deploy-csi.sh"
RESILIENCE_STUB="$TMPDIR/mock-resilience.sh"
cat > "$FORWARDING_STUB" <<'EOF'
#!/bin/bash
echo "forwarding" >> "$APP_MOCK_LOG"
EOF
cat > "$PREFLIGHT_STUB" <<'EOF'
#!/bin/bash
echo "calico-preflight" >> "$APP_MOCK_LOG"
EOF
cat > "$CSI_PREFLIGHT_STUB" <<'EOF'
#!/bin/bash
echo "csi-preflight NAMESPACE=$NAMESPACE WINDOWS_NODE=$WINDOWS_NODE" >> "$APP_MOCK_LOG"
EOF
cat > "$DEPLOY_SEAWEEDFS_STUB" <<'EOF'
#!/bin/bash
echo "deploy-seaweedfs $*" >> "$APP_MOCK_LOG"
echo "FILER=172.21.0.2:8888"
EOF
cat > "$DEPLOY_CSI_STUB" <<'EOF'
#!/bin/bash
echo "deploy-csi $*" >> "$APP_MOCK_LOG"
EOF
cat > "$RESILIENCE_STUB" <<'EOF'
#!/bin/bash
echo "resilience" >> "$APP_MOCK_LOG"
EOF
chmod +x "$FORWARDING_STUB" "$PREFLIGHT_STUB" "$CSI_PREFLIGHT_STUB" \
  "$DEPLOY_SEAWEEDFS_STUB" "$DEPLOY_CSI_STUB" "$RESILIENCE_STUB"

: > "$LOG"
if FORWARDING_SCRIPT="$FORWARDING_STUB" PREFLIGHT_SCRIPT="$PREFLIGHT_STUB" \
  CSI_PREFLIGHT_SCRIPT="$CSI_PREFLIGHT_STUB" \
  DEPLOY_SEAWEEDFS_SCRIPT="$DEPLOY_SEAWEEDFS_STUB" \
  DEPLOY_CSI_SCRIPT="$DEPLOY_CSI_STUB" \
  HEALTH_CHECK_SCRIPT="$HEALTH_STUB" \
  bash "$HACK/run-kind-qemu-storage-e2e.sh" > "$TMPDIR/e2e.out" 2>&1; then
  pass "run-kind-qemu-storage-e2e.sh exits 0"
else
  fail "run-kind-qemu-storage-e2e.sh exited nonzero"
  cat "$TMPDIR/e2e.out" >&2
fi
assert_log_contains "forwarding"
assert_log_contains "calico-preflight"
assert_log_contains "deploy-seaweedfs --namespace seaweedfs-test --image chrislusf/seaweedfs:4.23_large_disk"
assert_log_contains "deploy-csi --namespace seaweedfs-csi --csi-image ghcr.io/appmana/seaweedfs-csi-driver:v1.4.12-appmana.post.2 --mount-image ghcr.io/appmana/seaweedfs-mount:v1.4.12-appmana.post.2 --filer 172.21.0.2:8888"
assert_log_contains "csi-preflight NAMESPACE=seaweedfs-csi WINDOWS_NODE=appmana-000"
assert_log_contains "health --namespace seaweedfs-csi-test --storage-class seaweedfs-storage --linux-node appmana-calico-worker --windows-node appmana-000 --windows-exec hcsdiag"

: > "$LOG"
if RESILIENCE=true FORWARDING_SCRIPT="$FORWARDING_STUB" PREFLIGHT_SCRIPT="$PREFLIGHT_STUB" \
  CSI_PREFLIGHT_SCRIPT="$CSI_PREFLIGHT_STUB" \
  DEPLOY_SEAWEEDFS_SCRIPT="$DEPLOY_SEAWEEDFS_STUB" \
  DEPLOY_CSI_SCRIPT="$DEPLOY_CSI_STUB" \
  HEALTH_CHECK_SCRIPT="$HEALTH_STUB" \
  RESILIENCE_SCRIPT="$RESILIENCE_STUB" \
  bash "$HACK/run-kind-qemu-storage-e2e.sh" > "$TMPDIR/e2e-resilience.out" 2>&1; then
  pass "run-kind-qemu-storage-e2e.sh RESILIENCE=true exits 0"
else
  fail "run-kind-qemu-storage-e2e.sh RESILIENCE=true exited nonzero"
  cat "$TMPDIR/e2e-resilience.out" >&2
fi
assert_log_contains "health --namespace seaweedfs-csi-test"
assert_log_contains "resilience"

echo ""
echo "=== Harness Results ==="
echo "Checks passed: $PASSED  failed: $FAILED"
if [[ $FAILED -gt 0 ]]; then
  echo "APPMANA SCRIPT TESTS FAILED"
  exit 1
fi
echo "AppMana seaweedfs-csi script tests passed."
