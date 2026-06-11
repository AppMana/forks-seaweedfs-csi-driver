# AppMana kind + QEMU SeaweedFS CSI lab scripts

Local e2e verification for the Windows port of the SeaweedFS CSI driver
(branch `appmana/windows-v1.4.12`). The lab is the same kind cluster plus
QEMU Windows worker used by the calico fork; bring it up per
`../../../forks-calico-windows-ipv6/docs/kind-qemu-calico-validation.md`
(kind cluster `appmana-calico`, apiserver `10.2.0.55:6443`, Windows worker
`appmana-000` at `10.2.0.180`). Calico and kube-proxy-windows must already
be healthy there; their preflight is a precondition for ours.

## Scripts

- `deploy-seaweedfs-kind.sh` - all-in-one SeaweedFS pod
  (`weed server -filer -master.volumeSizeLimitMB=64 -volume.max=10`,
  hostNetwork on a kind node so the filer HTTP and gRPC ports are reachable
  from the QEMU VM through the existing forwarding rules). Prints
  `FILER=<host:port>`. `--filer HOST:PORT` skips the deploy and only
  verifies reachability from inside the cluster.
- `deploy-csi-driver-kind.sh` - StorageClass `seaweedfs-storage`, CSIDriver,
  RBAC, controller (provisioner/attacher/resizer sidecars), linux
  `seaweedfs-mount`/`seaweedfs-node` DaemonSets (kubelet root
  `/var/lib/kubelet`), and the Windows HostProcess DaemonSets (kubelet root
  `C:\var\lib\kubelet`, WinFsp installed by the mount image's
  initContainer). Requires `--csi-image`, `--mount-image`, `--filer`.
- `check-kind-qemu-csi-ready.sh` - preflight: DaemonSets + controller
  Ready, `csinode appmana-000` lists the driver, WinFsp.Launcher running,
  mount supervisor and CSI plugin sockets present on the VM. Emits
  `key=True/False` lines; any False is a bootstrap failure, not a
  health-matrix result.
- `storage-health-check.sh` - the matrix: RWX PVC bind, cross-OS
  write/read with sha256 equality in both directions, concurrent cross-OS
  append integrity, unmount cleanup (no leftover reparse point, no orphan
  `weed.exe`), and remount of prior data. `--quick` runs only PVC bind plus
  the cross-OS write/read tests; `--skip-cross-os` runs linux-only.
- `run-kind-qemu-csi-resilience.sh` - sequenced drills against a long-lived
  canary volume: node-plugin pod delete, `seaweedfs-mount.exe` supervisor
  kill, kubelet restart; each followed by `storage-health-check.sh --quick`.
- `run-kind-qemu-storage-e2e.sh` - orchestrator: forwarding rules + calico
  preflight (from the calico repo, env-overridable via `FORWARDING_SCRIPT`
  / `PREFLIGHT_SCRIPT`), deploy SeaweedFS, deploy CSI driver, CSI
  preflight, health matrix. `RESILIENCE=true` appends the drills.

## Normal loop

```bash
# 1. Bring up the kind + QEMU lab (see the calico validation doc above).
# 2. Build/push candidate images via the fork CI (one manifest-list tag
#    serves linux and windows ltsc2022).
# 3. Run the full flow against the candidate tag:
CSI_IMAGE=ghcr.io/appmana/seaweedfs-csi-driver:<tag> \
MOUNT_IMAGE=ghcr.io/appmana/seaweedfs-mount:<tag> \
hack/appmana/run-kind-qemu-storage-e2e.sh

# 4. Resilience drills on top:
RESILIENCE=true CSI_IMAGE=... MOUNT_IMAGE=... hack/appmana/run-kind-qemu-storage-e2e.sh

# 5. Mocked script tests (no kind/QEMU needed):
hack/appmana/tests/run-appmana-script-tests.sh
```

## Safety

These scripts must never target the production cluster. They default to
`KUBECONFIG=/tmp/appmana-calico-kind/kubeconfig`, and the deploy, preflight,
health, and resilience scripts refuse to run unless
`kubectl config current-context` is the kind lab context
`kind-appmana-calico`.
