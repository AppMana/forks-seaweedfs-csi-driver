#!/usr/bin/env bash
# Focused real-kernel recovery regression; no cluster, VM, or FUSE required.
# Usage: bash test/run-kernel-recovery.sh ABSOLUTE_ARTIFACT_DIR LOCAL_LINUX_IMAGE
# Image must already exist locally. It needs no Go compiler or network access.
set -euo pipefail
artifact_dir=${1:?provide an absolute artifact directory}
runtime_image=${2:?provide a locally available Linux image}
case "$artifact_dir" in /*) ;; *) exit 2 ;; esac
repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
mkdir -p "$artifact_dir"
cd "$repo"
CGO_ENABLED=0 go test -c -o "$artifact_dir/kernel-recovery.test" ./pkg/driver
# Docker supplies a private mount namespace. Only the test binary is exposed
# read-only; all test mounts and files are confined to disposable /tmp.
docker run --rm --pull=never --network none --cap-add SYS_ADMIN \
  --security-opt apparmor=unconfined --security-opt seccomp=unconfined \
  --read-only --tmpfs /tmp:rw,size=32m \
  -e CSI_ISOLATED_KERNEL_MOUNT_TEST=1 \
  --mount "type=bind,src=$artifact_dir/kernel-recovery.test,dst=/kernel-recovery.test,readonly" \
  --entrypoint /kernel-recovery.test "$runtime_image" \
  -test.run '^TestManagerRestartKernelMountRecovery$' -test.v \
  2>&1 | tee "$artifact_dir/kernel-recovery.log"
