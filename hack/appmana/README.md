# kind and QEMU Windows CSI lab scripts

End-to-end checks of the Windows CSI driver on a kind cluster plus a QEMU
Windows worker: the kind and QEMU lab of the Calico fork
(`forks-calico-windows-ipv6`, its `docs/kind-qemu-calico-validation.md`).
Calico and kube-proxy-windows must already be healthy there; their preflight
is a precondition for these scripts. The digest-pinned Labcontainers lab in
`test/kubernetes_lab` (see [AGENTS.md](../../AGENTS.md)) replaces this lab for
release checks.

## Scripts

- `deploy-seaweedfs-kind.sh`: an all-in-one SeaweedFS pod
  (`weed server -filer -master.volumeSizeLimitMB=64 -volume.max=10`) on host
  networking on a kind node, so the filer HTTP and gRPC ports are reachable
  from the QEMU VM. Prints `FILER=<host:port>`. `--filer HOST:PORT` skips the
  deploy and only checks reachability from inside the cluster.
- `deploy-csi-driver-kind.sh`: StorageClass `seaweedfs-storage`, CSIDriver,
  RBAC, controller with provisioner, attacher and resizer sidecars, the Linux
  `seaweedfs-mount` and `seaweedfs-node` DaemonSets (kubelet root
  `/var/lib/kubelet`) and the Windows HostProcess DaemonSets (kubelet root
  `C:\var\lib\kubelet`; this script still installs WinFsp from an
  initContainer). Requires `--csi-image`, `--mount-image` and `--filer`.
- `check-kind-qemu-csi-ready.sh`: preflight. DaemonSets and controller Ready,
  the Windows node's `csinode` lists the driver, WinFsp.Launcher running, the
  mount supervisor and CSI plugin sockets present on the VM. Prints
  `key=True/False` lines; any False is a bootstrap failure, not a health
  result.
- `storage-health-check.sh`: RWX PVC bind, cross-OS write and read with
  SHA-256 equality both ways, concurrent cross-OS append integrity, unmount
  cleanup (no leftover reparse point or orphan `weed.exe`) and remount of
  earlier data. `--quick` runs PVC bind and the cross-OS write and read only;
  `--skip-cross-os` runs Linux only.
- `run-kind-qemu-csi-resilience.sh`: on a long-lived canary volume, deletes
  the node plugin pod, kills `seaweedfs-mount.exe` and restarts kubelet, each
  followed by `storage-health-check.sh --quick`.
- `run-kind-qemu-storage-e2e.sh`: forwarding rules and Calico preflight (from
  the Calico repository, overridable with `FORWARDING_SCRIPT` and
  `PREFLIGHT_SCRIPT`), SeaweedFS deploy, CSI deploy, CSI preflight and the
  health matrix. `RESILIENCE=true` adds the resilience drills.

Environment: `KUBECONFIG` (the lab kubeconfig), `LAB_CONTEXT` (the kind
context the scripts insist on), `WINDOWS_NODE` (the QEMU worker's node name)
and `CALICO_HACK_DIR` (the Calico fork's `hack/appmana`, by default a sibling
checkout). The deploy, preflight, health and resilience scripts refuse to run
unless the current context is `LAB_CONTEXT`.

## Usage

```bash
# 1. Bring up the kind and QEMU lab from the Calico fork.
# 2. Build candidate images through this repository's CI.
CSI_IMAGE=ghcr.io/appmana/seaweedfs-csi-driver:<tag> \
MOUNT_IMAGE=ghcr.io/appmana/seaweedfs-mount:<tag> \
hack/appmana/run-kind-qemu-storage-e2e.sh

RESILIENCE=true CSI_IMAGE=... MOUNT_IMAGE=... hack/appmana/run-kind-qemu-storage-e2e.sh

# Mocked script tests, no kind or QEMU needed:
hack/appmana/tests/run-appmana-script-tests.sh
```
