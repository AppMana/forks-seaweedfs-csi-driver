#!/usr/bin/env bash
set -euo pipefail
[[ $# == 1 ]] || { echo 'usage: verify-media.sh LCQUAL.iso' >&2; exit 2; }
# Linux reads Rock Ridge; Windows reads Joliet. Checking only the Linux tree
# misses ISO rewrites that silently turn windows-*.tar into WINDOWS_*.TAR.
listing=$(isoinfo -J -f -i "$1")
for input in k0s k0s.exe mount-smoke.ps1 linux-csi.tar linux-registrar.tar linux-provisioner.tar linux-attacher.tar linux-resizer.tar windows-csi.tar windows-registrar.tar windows-pause.tar windows-workload.tar; do
 if ! grep -Fxq "/$input" <<<"$listing"; then
  echo "Windows-visible offline input missing: $input" >&2
  exit 1
 fi
done
