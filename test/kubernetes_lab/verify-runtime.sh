#!/usr/bin/env bash
set -euo pipefail

: "${CALICO_LAB_MODULE:?native fixture module required}"
: "${LABCONTAINERS_LABD:?explicit daemon required}"
: "${LABCONTAINERS_VM_IMAGE:?explicit Linux VM image required}"
: "${LABCONTAINERS_WINDOWS_IMAGE:?explicit Windows VM image required}"

sdk=$(cd "$CALICO_LAB_MODULE" && GOWORK=off go list -m -f '{{if .Replace}}REPLACED{{else}}{{.Version}}{{end}}' github.com/appmana/labcontainers)
[[ "$sdk" =~ -([0-9a-f]{12})$ ]] || { echo 'SDK must have an immutable, unreplaced commit pin' >&2; exit 1; }
sdk_commit=${BASH_REMATCH[1]}
build=$(go version -m "$LABCONTAINERS_LABD")
daemon_module=$(awk '$1=="mod" && $2=="github.com/appmana/labcontainers" {print $3}' <<<"$build")
revision=$(awk '$1=="build" && $2 ~ /^vcs.revision=/ {sub(/^vcs.revision=/,"",$2); print $2}' <<<"$build")
clean=$(awk '$1=="build" && $2=="vcs.modified=false" {print "yes"}' <<<"$build")
[[ "$daemon_module" == "$sdk" && "$revision" =~ ^[0-9a-f]{40}$ && "${revision:0:12}" == "$sdk_commit" && "$clean" == yes ]] || {
 echo 'Daemon build must match the pinned SDK and have clean source provenance' >&2; exit 1;
}

helper_hash=''
for image in "$LABCONTAINERS_VM_IMAGE" "$LABCONTAINERS_WINDOWS_IMAGE"; do
 labels=$(docker image inspect --format '{{ index .Config.Labels "appmana.labcontainers.revision" }} {{ index .Config.Labels "appmana.labcontainers.guest-helper-sha256" }}' "$image")
 read -r image_revision image_hash extra <<<"$labels"
 [[ "$image_revision" == "$revision" && "$image_hash" =~ ^[0-9a-f]{64}$ && -z "$extra" ]] || {
  echo "VM image $image does not attest the matching guest helper" >&2; exit 1;
 }
 [[ -z "$helper_hash" || "$helper_hash" == "$image_hash" ]] || { echo 'VM guest-helper hashes differ' >&2; exit 1; }
 helper_hash=$image_hash
done
printf 'CSI_RUNTIME_PINS_VERIFIED sdk=%s revision=%s helper=%s\n' "$sdk" "$revision" "$helper_hash"
