# SeaweedFS CSI driver fork: Windows nodes and recoverable mounts

Fork of [seaweedfs/seaweedfs-csi-driver](https://github.com/seaweedfs/seaweedfs-csi-driver)
(upstream base v1.4.34; the default branch keeps its original name,
`appmana/windows-v1.4.12`). The same driver and StorageClass serve Linux and **Windows**
Kubernetes nodes, including cross-OS ReadWriteMany, and node mounts survive
plugin and mount-daemon restarts. The `weed` binary inside the mount image
comes from [forks-seaweedfs](https://github.com/AppMana/forks-seaweedfs),
whose README describes the mount's own fixes, flags and benchmarks.

## What the fork changes

- **Windows nodes**: HostProcess node plugin and mount supervisor running
  WinFsp `weed.exe mount` in network-FS mode (details below).
- **Restart recovery**: the node plugin rediscovers staged volumes from
  kubelet's `vol_data.json` (`--stagingScanDir`, on by default), including pod publish paths
  of statically provisioned volumes (matched by volume handle), and its health
  monitor remounts dead FUSE mounts and re-binds them inside running
  containers without deleting pods. Stage and Publish succeed only on a
  healthy mount. A slow probe is not death: a mount counts as hung only when
  three consecutive checks time out and one probe has stayed unanswered for
  longer than the mount's 60 s request bound; it is then stopped through the
  mount service and re-staged. `NodeGetVolumeStats` reports usage and a
  `VolumeCondition`, abnormal while a mount is dead or hung.
- **Cache directories**: at startup and then hourly the node plugin removes
  cache directories whose volume is neither staged (by the plugin or, per its
  staging directory, by kubelet), being staged, nor served by a running mount,
  instead of emptying the whole cache base under live mounts. A sweep that
  would wait for a stage in progress skips its round. The shared chunk cache
  `<cacheDir>/chunks` is always kept. Starting a volume's mount removes its
  other instance cache directories (weed names them after its version) that
  no process holds open.
- **Platform defaults**: cache base and kubelet staging directory default to
  the platform's paths, the node plugin creates its endpoint directory, and
  the Windows mount service installs the pinned WinFsp itself, so the
  DaemonSets need no init containers or path flags.
- **Write buffer cap**: `--writeBufferSizeMB` caps every mount's dirty data,
  so `close()` and `fsync()` drain within the mount's request deadline.
- **Per-volume mount options** from StorageClass parameters or static PV
  `volumeAttributes` (table below), including WinFsp cache tuning and
  `readerCacheMode`.
- **Unpublish quarantine**: with `--kubeletRootDir` and `--kubeletRootMount`
  (a mount of the whole kubelet root into the plugin, HostToContainer), an
  unpublish whose target is not mounted but holds files renames it intact to
  `<kubelet root>/seaweedfs-quarantine/<time>-<pod uid>-<pvc>` and succeeds.
  Without both flags such an unpublish fails and keeps the data.
- Volume buckets are created so that their empty directories are kept.

## Configuration

Node plugin flags set the default for every mount on the node:

| Flag | Default | Effect |
| --- | --- | --- |
| `--cacheDir` | `/var/cache/seaweedfs`, `C:\var\cache\seaweedfs` | Base of per-volume cache directories, `<cacheDir>/<sha256 of volume id>`, and of the node's shared chunk cache `<cacheDir>/chunks`. |
| `--cacheCapacityMB` | 0 | Each mount's on-disk read cache (`weed mount -cacheCapacityMB`); `0` disables it. |
| `--writeBufferSizeMB` | 0 (no cap) | Each mount's dirty-data cap, memory plus swap. |
| `--concurrentWriters`, `--concurrentReaders` | 128 | Upload and chunk-fetch concurrency per mount. |
| `--cacheMetaTtlSec` | 60 | Metadata cache TTL. |
| `--stagingScanDir` | `/var/lib/kubelet/plugins/kubernetes.io/csi`, `C:\var\lib\kubelet\plugins\kubernetes.io\csi` | Kubelet `plugins/kubernetes.io/csi` directory scanned for staged volumes at startup; the cache sweep also keeps a cache while kubelet's staging directory for it exists. Set it when kubelet has another root (k0s: `/var/lib/k0s/kubelet/...`); empty disables both. |

A StorageClass parameter, or a static PV's `volumeAttributes`, overrides the
node default for that volume: `cacheCapacityMB`, `wholeCacheCapacityMB`,
`readerCacheSizeMB`, `writeBufferSizeMB`, `cacheMetaTtlSec`, `concurrentReaders`, `concurrentWriters`, `readerCacheMode`,
`winfspOptions`, `replication`, `collection`, `diskType`, `dataCenter`,
`dataLocality`, `ttl`, `chunkSizeLimitMB`, `volumeServerAccess`,
`readRetryTime`, `writebackCache`, `metadataFlushSeconds`, `dlm`, `uidMap`,
`gidMap` and `path`. Unknown keys are logged and
ignored. `sharedChunkCacheDir` is node-local and always `<cacheDir>/chunks`:
a StorageClass or PV value is ignored, and a VolumeAttributesClass cannot set
it. A dynamically provisioned PV records its StorageClass parameters when
it is created, and PV volume attributes are immutable: changing a
StorageClass affects only volumes provisioned afterwards.

Request deadlines are the mount's own (`weed mount` defaults): namespace
requests are answered within 25 s and every other FUSE request within 60 s,
the kernel aborts a connection unanswered for 90 s (Linux 6.14+), and chunk
uploads and filer calls are bounded. A mount's disk use is at most its
`cacheCapacityMB` share of the node's chunk cache plus `writeBufferSizeMB` of
swap; the chunk cache as a whole is bounded by `wholeCacheCapacityMB`. Size
them against the node's root disk and the number of volumes it mounts.

Measured with the fork's mount: a 4 GiB write with a 512 MiB cap on a 1 GbE
node takes 80.4 s to write and 8.8 s to `close()` (46 MB/s); 256 MiB at
16 MiB/s closes in 15.7 s uncapped and 1.35 s with a 32 MiB cap.

## Images and releases

`build-images.yml` runs on pushes and pull requests to the default branch and
`merge/**`: vet and race unit tests, native Windows tests (which install the
pinned WinFsp MSI through the mount service's installer on the hosted
runner), then Linux and Windows (ltsc2022) images of
`ghcr.io/appmana/seaweedfs-csi-driver` and `ghcr.io/appmana/seaweedfs-mount`,
published as multi-platform manifests tagged
`candidate-<sha>-<run>-<attempt>`. The bundled `weed`/`weed.exe` is a static
large-disk build (`5BytesOffset`, 8 TB volume files) of the
[forks-seaweedfs](https://github.com/AppMana/forks-seaweedfs) commit the
workflow pins, matching servers that run `*_large_disk` images.

A release is a candidate's exact images under a readable tag,
`v<upstream base>-appmana.post.<n>`: the upstream release the fork last merged
(`git describe --tags --exclude '*appmana*'`) and a counter that restarts at 1
for each new upstream base. [RELEASES.md](RELEASES.md) lists what each release
contains and its digests. To release a candidate built from a commit on the
default branch:

1. Add its `## v…-appmana.post.<n>` section to RELEASES.md, naming the
   candidate's source commit, and push it to the default branch.
2. Run **Promote a CI candidate to a release** (`promote-release.yml`) with the
   release and candidate tags. It adds the release tag to the candidate's
   manifest list and each per-platform image (`-linux-amd64`,
   `-windows-ltsc2022`) with `crane tag`, refuses to move an existing release
   tag, tags the source commit in git and prints the pins.

Deployments pin `repository:<release tag>[-<platform>]@sha256:<digest>`: the
tag names the release and the digest keeps the pin immutable. Linux and
Windows DaemonSets pin the per-platform images. Never rebuild a released
image or move a release tag.

## Windows nodes

- The node plugin and the mount supervisor run as **HostProcess** DaemonSets
  (Server 2022, containerd). There is no csi-proxy: file and process
  operations are direct Win32 calls.
- The supervisor runs one WinFsp `weed.exe mount` per volume in network-FS
  mode (`WEED_WINFSP_VOLUME_PREFIX`, default `\seaweedfs`; `local` turns it
  off): the volume is served as a UNC path, the only form Windows containers
  can consume ([winfsp#498](https://github.com/winfsp/winfsp/issues/498)).
  Publish is a symlink, as in the SMB and Azure File CSI drivers.
- `weed.exe` children run in a kill-on-close job object and are stopped with
  a console ctrl event for a clean unmount. Staged volumes are rediscovered
  from kubelet's `vol_data.json` after plugin restarts (`--stagingScanDir`)
  and restaged by the health monitor.
- Publishes record where pods mount each volume (`-winfspMountRoots`) so
  absolute symlinks resolve inside the volume.

The Windows mount image pins upstream WinFsp **2.1.25156** with
`WINFSP_MSI_URL` and `WINFSP_MSI_SHA256` in
`cmd/seaweedfs-mount/Dockerfile.Windows`, and ships the user-mode DLL released
by [AppMana/forks-winfsp-fixes](https://github.com/AppMana/forks-winfsp-fixes)
(`WINFSP_DLL_URL`, `WINFSP_DLL_SHA256`) beside `weed.exe`, against the stock
signed driver. Before it accepts mounts, `seaweedfs-mount.exe` checks
`HKLM\SOFTWARE\WOW6432Node\WinFsp`. When WinFsp is missing it copies
`winfsp.msi` from beside the executable to `%SystemRoot%\Temp` (the Windows
Installer service cannot read the HostProcess sandbox mount, exit 1619), runs
`msiexec /i … /qn INSTALLLEVEL=1000 /l*v %SystemRoot%\Temp\seaweedfs-winfsp-install.log`
and fails startup on a nonzero exit, naming the log. It then requires the
installed version (the WinFsp `DisplayVersion` under the Windows Installer
uninstall keys) to be 2.1.25156, the driver the DLL is built for, and fails
startup on any other. It never upgrades or replaces an installed WinFsp, so a
DaemonSet rollout cannot upgrade the driver. `--winfspMSI` names another MSI;
`--winfspVersion=` (empty) accepts any installed version, for the lab's
kernel driver lane only. Keep test signing disabled on cluster hosts.

## Deploying the Windows DaemonSets

Deploy the upstream controller, StorageClass and Linux DaemonSets as usual
(`deploy/kubernetes/seaweedfs-csi.yaml`), then add the two Windows
DaemonSets. Minimal example (adjust the filer address and images; kubelet's
root is assumed to be `C:\var\lib\kubelet`, otherwise set
`--stagingScanDir`):

```yaml
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: seaweedfs-mount-windows
  namespace: seaweedfs
spec:
  selector: { matchLabels: { app: seaweedfs-mount-windows } }
  template:
    metadata:
      labels: { app: seaweedfs-mount-windows }
    spec:
      nodeSelector: { kubernetes.io/os: windows }
      tolerations: [{ operator: Exists, effect: NoSchedule }]
      securityContext:
        windowsOptions: { hostProcess: true, runAsUserName: "NT AUTHORITY\\SYSTEM" }
      hostNetwork: true
      priorityClassName: system-node-critical
      serviceAccountName: seaweedfs-node-sa
      containers:
        - name: seaweedfs-mount
          image: ghcr.io/appmana/seaweedfs-mount:<release>-windows-ltsc2022@sha256:<digest>
          command: ["$env:CONTAINER_SANDBOX_MOUNT_POINT/seaweedfs-mount.exe"]
          args: ["--endpoint=unix://C:\\var\\lib\\seaweedfs-mount\\seaweedfs-mount.sock"]
---
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: seaweedfs-node-windows
  namespace: seaweedfs
spec:
  selector: { matchLabels: { app: seaweedfs-node-windows } }
  template:
    metadata:
      labels: { app: seaweedfs-node-windows }
    spec:
      nodeSelector: { kubernetes.io/os: windows }
      tolerations: [{ operator: Exists, effect: NoSchedule }]
      securityContext:
        windowsOptions: { hostProcess: true, runAsUserName: "NT AUTHORITY\\SYSTEM" }
      hostNetwork: true
      priorityClassName: system-node-critical
      serviceAccountName: seaweedfs-node-sa
      containers:
        - name: csi-seaweedfs-plugin
          image: ghcr.io/appmana/seaweedfs-csi-driver:<release>-windows-ltsc2022@sha256:<digest>
          command: ["$env:CONTAINER_SANDBOX_MOUNT_POINT/seaweedfs-csi-driver.exe"]
          args:
            - --endpoint=unix://C:\var\lib\kubelet\plugins\seaweedfs-csi-driver\csi.sock
            # use the FQDN: weed.exe runs as a host process and short
            # service names do not resolve on Windows hosts
            - --filer=seaweedfs-filer.seaweedfs.svc.cluster.local:8888
            - --nodeid=$(NODE_ID)
            - --driverName=seaweedfs-csi-driver
            - --mountEndpoint=unix://C:\var\lib\seaweedfs-mount\seaweedfs-mount.sock
            - --cacheCapacityMB=51200
            - --components=node
          env:
            - name: NODE_ID
              valueFrom: { fieldRef: { fieldPath: spec.nodeName } }
        - name: driver-registrar
          image: registry.k8s.io/sig-storage/csi-node-driver-registrar:v2.16.0
          command: ["csi-node-driver-registrar.exe"]
          args:
            - --csi-address=unix://C:\var\lib\kubelet\plugins\seaweedfs-csi-driver\csi.sock
            - --kubelet-registration-path=C:\\var\\lib\\kubelet\\plugins\\seaweedfs-csi-driver\\csi.sock
            - --plugin-registration-path=C:\\var\\lib\\kubelet\\plugins_registry\\
```

Tuning: WinFsp mounts cache file data in the NT cache manager by default
(`FileInfoTimeout=-1`), with other clients' changes invalidating it through
filer events; see forks-seaweedfs `WINDOWS_PORT.md`.
`SEAWEEDFS_WINFSP_OPTIONS` on the mount DaemonSet passes extra WinFsp `-o`
options to every mount, and a volume's `winfspOptions` parameter to one.

Windows mounts are case-insensitive by default. Set
`SEAWEEDFS_WINFSP_CASE_SENSITIVE=true` on the mount DaemonSet only when a
workload requires the filer namespace's case-sensitive behavior.

[AGENTS.md](AGENTS.md) covers building, testing and the Kubernetes lab;
`hack/appmana/` holds the kind and QEMU Windows e2e scripts.

Upstream README: https://github.com/seaweedfs/seaweedfs-csi-driver
