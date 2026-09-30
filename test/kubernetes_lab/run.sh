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
export LABCONTAINERS_KUBERNETES_WORKLOAD_ARGS="[\"-test.v\",\"-test.run=^TestCSIStockWinFsp$\",\"-test.timeout=35m\",\"-csi-live\",\"-csi-cni=$LABCONTAINERS_KUBERNETES_CNI\"]"
# Optional production-layout lane: retain the lab images only for test tools.
# Require a complete digest-pinned set before starting any VM.
split_count=0
for role in driver mount; do
 for platform in linux windows; do
  key="CSI_${role^^}_${platform^^}_IMAGE"
  value=${!key:-}
  if [[ -n "$value" ]]; then
   [[ "$value" =~ ^[a-zA-Z0-9./:_-]+@sha256:[0-9a-f]{64}$ ]] || { echo "$key must be digest-pinned" >&2; exit 1; }
   LABCONTAINERS_KUBERNETES_WORKLOAD_ARGS="${LABCONTAINERS_KUBERNETES_WORKLOAD_ARGS%]},\"-csi-$role-$platform-image=$value\"]"
   split_count=$((split_count+1))
  fi
 done
done
[[ "$split_count" == 0 || "$split_count" == 4 ]] || { echo 'Provide all four CSI split image references' >&2; exit 1; }
export LABCONTAINERS_KUBERNETES_WORKLOAD_SUCCESS=CSI_QUALIFICATION_COMPLETE
# stage_split_media.py produces the final-image + patched-MSI bundle. Explicit
# selection cannot silently fall back to the stock-driver qualification lane.
if [[ -n "${CSI_CANDIDATE_BUNDLE:-}" ]]; then
 [[ "$split_count" == 0 ]] || { echo 'Candidate bundle already pins all four images; do not override them' >&2; exit 1; }
 candidate_spec=$(python3 "$csi_repo/test/kubernetes_lab/stage_split_media.py" --launch-bundle "$CSI_CANDIDATE_BUNDLE" --cni "$LABCONTAINERS_KUBERNETES_CNI")
 LABCONTAINERS_KUBERNETES_WORKLOAD_ARGS=$(jq -ce '.args' <<<"$candidate_spec")
 LABCONTAINERS_KUBERNETES_WORKLOAD_SUCCESS=$(jq -er '.success' <<<"$candidate_spec")
fi
fixture_timeout=90m
if [[ -n "${LABCONTAINERS_KUBERNETES_CRASH_VERIFY:-}" ]]; then
 [[ "$LABCONTAINERS_KUBERNETES_CRASH_VERIFY" == 1 && -n "${CSI_CANDIDATE_BUNDLE:-}" ]] || { echo 'Crash readback requires explicit 1 and the candidate bundle' >&2; exit 1; }
 LABCONTAINERS_KUBERNETES_WORKLOAD_ARGS=$(jq -ce '. + ["-csi-emit-crash-plan"]' <<<"$LABCONTAINERS_KUBERNETES_WORKLOAD_ARGS")
 fixture_timeout=130m
fi
printf '%s\n' "$LABCONTAINERS_KUBERNETES_WORKLOAD_ARGS" > "$CSI_LAB_ARTIFACTS/launched-workload-args.json"
(cd "$csi_repo" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go test -c -o "$LABCONTAINERS_KUBERNETES_WORKLOAD" ./test/kubernetes_lab)
LABCONTAINERS_KUBERNETES_WORKLOAD_SHA256=$(sha256sum "$LABCONTAINERS_KUBERNETES_WORKLOAD" | cut -d ' ' -f 1)
export LABCONTAINERS_KUBERNETES_WORKLOAD_SHA256
cd "$CALICO_LAB_MODULE"
go test -v -count=1 -run '^TestLiveK0sWindowsNetwork$' -timeout="$fixture_timeout" . 2>&1 | tee "$CSI_LAB_ARTIFACTS/live.log"
# go test returns success on skipped tests; require evidence of the consumer.
grep -Fq "$LABCONTAINERS_KUBERNETES_WORKLOAD_SUCCESS" "$CSI_LAB_ARTIFACTS/live.log"
if [[ "${LABCONTAINERS_KUBERNETES_CRASH_VERIFY:-}" == 1 ]]; then
 grep -Fxq 'RETAINED_KUBERNETES_CRASH_CONSUMER_COMPLETE' "$CSI_LAB_ARTIFACTS/live.log"
fi
