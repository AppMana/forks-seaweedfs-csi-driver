#!/usr/bin/env bash
set -euo pipefail

# Uses the native Kubernetes fixture; never consults the caller's kubeconfig.
# All paths and offline input hashes are explicit so CI cannot silently skip.
: "${CALICO_LAB_MODULE:?path to Calico hack/appmana/lab with the native workload hook}"
: "${CSI_LAB_ARTIFACTS:?persistent output directory for this run}"
: "${LABCONTAINERS_STATE_DIR:?absolute persistent daemon state directory for this fresh lab}"
: "${LABCONTAINERS_CALICO_MEDIA:?offline LCQUAL ISO including CSI images and mount-smoke.ps1}"
: "${LABCONTAINERS_CALICO_MEDIA_SHA256:?SHA256 of the offline ISO}"
: "${LABCONTAINERS_VM_IMAGE:?Linux image with matching guest helper}"
: "${LABCONTAINERS_WINDOWS_IMAGE:?Windows image with matching guest helper}"
: "${LABCONTAINERS_LABD:?daemon matching the fixture SDK and guest helper}"

# Keep the fixture matrix and the CSI network oracle on the same explicit CNI.
# This closed set also makes embedding the selection in JSON unambiguous.
export LABCONTAINERS_KUBERNETES_CNI=${LABCONTAINERS_KUBERNETES_CNI:-calico-vxlan}
case "$LABCONTAINERS_KUBERNETES_CNI" in
  calico-vxlan|calico-bgp) ;;
  *) echo 'CSI qualification requires calico-vxlan or calico-bgp' >&2; exit 1 ;;
esac

csi_repo=$(git -C "$(dirname "$0")" rev-parse --show-toplevel)
bash "$csi_repo/test/kubernetes_lab/verify-runtime.sh"
bash "$csi_repo/test/kubernetes_lab/verify-media.sh" "$LABCONTAINERS_CALICO_MEDIA"
mkdir -p "$CSI_LAB_ARTIFACTS"
export GOWORK=off
export LABCONTAINERS_KUBERNETES_WORKLOAD="$CSI_LAB_ARTIFACTS/kubernetes-workload"
(cd "$csi_repo" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go test -c -o "$LABCONTAINERS_KUBERNETES_WORKLOAD" ./test/kubernetes_lab)
LABCONTAINERS_KUBERNETES_WORKLOAD_SHA256=$(sha256sum "$LABCONTAINERS_KUBERNETES_WORKLOAD" | cut -d ' ' -f 1)
export LABCONTAINERS_KUBERNETES_WORKLOAD_SHA256
export LABCONTAINERS_KUBERNETES_WORKLOAD_ARGS="[\"-test.v\",\"-test.run=^TestCSIStockWinFsp$\",\"-test.timeout=35m\",\"-csi-live\",\"-csi-cni=$LABCONTAINERS_KUBERNETES_CNI\"]"
export LABCONTAINERS_KUBERNETES_WORKLOAD_SUCCESS=CSI_QUALIFICATION_COMPLETE
cd "$CALICO_LAB_MODULE"
go test -v -count=1 -run '^TestLiveK0sWindowsNetwork$' -timeout=90m . 2>&1 | tee "$CSI_LAB_ARTIFACTS/live.log"
# go test returns success on skipped tests; require evidence of the consumer.
grep -q 'CSI_QUALIFICATION_COMPLETE' "$CSI_LAB_ARTIFACTS/live.log"
