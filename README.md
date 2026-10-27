# SeaweedFS CSI driver fork: Windows nodes and recoverable mounts

Fork of [seaweedfs/seaweedfs-csi-driver](https://github.com/seaweedfs/seaweedfs-csi-driver)
(v1.4.12). The same driver and StorageClass serve Linux and **Windows**
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
node default for that volume: `cacheCapacityMB`, `writeBufferSizeMB`,
`cacheMetaTtlSec`, `concurrentReaders`, `concurrentWriters`, `readerCacheMode`,
`winfspOptions`, `replication`, `collection`, `diskType`, `dataCenter`,
`dataLocality`, `ttl`, `chunkSizeLimitMB`, `volumeServerAccess`,
`readRetryTime`, `uidMap`, `gidMap` and `path`. Unknown keys are logged and
ignored. `sharedChunkCacheDir` is node-local and always `<cacheDir>/chunks`:
a StorageClass or PV value is ignored, and a VolumeAttributesClass cannot set
it. A dynamically provisioned PV records its StorageClass parameters when
it is created, and PV volume attributes are immutable: changing a
StorageClass affects only volumes provisioned afterwards.

Request deadlines are the mount's own (`weed mount` defaults): every FUSE
request is answered within 60 s, the kernel aborts a connection unanswered
for 90 s (Linux 6.14+), and chunk uploads and filer calls are bounded. A
mount's disk use is at most its `cacheCapacityMB` plus `writeBufferSizeMB`
in its current cache directory; size them against the node's root disk and
the number of volumes it mounts.

Measured with the fork's mount: a 4 GiB write with a 512 MiB cap on a 1 GbE
node takes 80.4 s to write and 8.8 s to `close()` (46 MB/s); 256 MiB at
16 MiB/s closes in 15.7 s uncapped and 1.35 s with a 32 MiB cap.

## Images and CI

`build-images.yml` runs on pushes and pull requests to the default branch and
`merge/**`: race unit tests, native Windows tests (which install the pinned
WinFsp MSI on the hosted runner through the mount service's installer), then Linux and Windows
(ltsc2022) images for `ghcr.io/appmana/seaweedfs-csi-driver` and
`ghcr.io/appmana/seaweedfs-mount`, published as multi-platform manifests
tagged `candidate-<sha>-<run>-<attempt>`. `SEAWEEDFS_COMMIT` (Linux) and
`SEAWEEDFS_WINDOWS_COMMIT` select the forks-seaweedfs source; the Linux
mount Dockerfiles must carry the same pin, and
`test/kubernetes_lab/build_pins_test.go` requires that, a static
(`CGO_ENABLED=0`) large-disk (`5BytesOffset`) build and a stamped
`weed version`. Deploy candidates by digest after qualification; never
rebuild a deployed image or move a release tag.

### Releases

A release is a qualified candidate's exact images under a readable tag,
`v<upstream base>-appmana.post.<n>`: the upstream release the fork last
merged (`git describe --tags`, v1.4.34 since `merge/upstream-csi-master`) and
a counter that restarts at 1 for each new upstream base. Earlier lines:
`v1.4.12-appmana.post.1` to `.post.20`, `v1.4.26-appmana.post.1` and `.post.2`.
[RELEASES.md](RELEASES.md) lists what each release contains and its digests.

To release a candidate built from a commit on the default branch:

1. Add its `## v…-appmana.post.<n>` section to RELEASES.md, naming the
   candidate's source commit, and push it to the default branch.
2. Run the **Promote a CI candidate to a release** workflow
   (`promote-release.yml`) with the release and candidate tags. It adds the
   release tag to the candidate's manifest list and to each per-platform
   image (`-linux-amd64`, `-windows-ltsc2022`) with `crane tag`, refuses to
   move an existing release tag, tags the source commit in git, and prints
   the pins.

Deployments pin `repository:<release tag>[-<platform>]@sha256:<digest>`: the
tag says which release runs, the digest keeps the pin immutable (the runtime
pulls by digest). Linux and Windows DaemonSets pin the per-platform images.

The bundled `weed`/`weed.exe` binaries are the large-disk build (5-byte
needle offsets for 8 TB volume files), matching servers that run
`*_large_disk` images.

## Qualification

```sh
go test -race -count=1 ./...                      # unit, as CI runs it
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...   # Windows cross-compile
test/kubernetes_lab/run.sh                        # mixed Linux and Windows Labcontainers cluster
```

The Kubernetes lab qualifies digest-pinned images in disposable VMs:
`CSI_QUALIFICATION_MODE=recovery` runs the restart, remount, reboot and
crash-recovery scenarios, the default `full` mode the complete native Windows
suite (see below). The mount binary's own gates (unit, `test/fuse_integration`,
`test/storage_lab` VM gates) run in forks-seaweedfs.

## Branches

`appmana/windows-v1.4.12` is the default branch and the source of deployed
images; it advances by fast-forward or merge from `merge/**` branches once
their images are qualified. `merge/upstream-csi-master` carries the next
upstream merge.

## Windows nodes

How it works on Windows:

- The node plugin and the mount supervisor run as **HostProcess** DaemonSets
  (Server 2022, containerd). No csi-proxy: file and process operations are
  direct Win32 calls.
- The supervisor spawns one `weed.exe mount` (from
  [AppMana/forks-seaweedfs](https://github.com/AppMana/forks-seaweedfs),
  WinFsp-backed) per volume in **network-FS mode**: the volume is served as a
  UNC path, which is the only form Windows containers can consume
  ([winfsp#498](https://github.com/winfsp/winfsp/issues/498)). Publish is a
  symlink, like the SMB/Azure-File CSI drivers.
- weed.exe children are held in a kill-on-close job object and stopped with a
  console ctrl event for clean unmounts; staged volumes are rediscovered from
  kubelet's `vol_data.json` after plugin restarts (`--stagingScanDir`) and
  re-staged by the health monitor.
- The mount service installs WinFsp on the host when it starts, from the MSI
  shipped in the mount image (below).

The Windows mount image pins upstream WinFsp **2.1.25156** using
`WINFSP_MSI_URL` and `WINFSP_MSI_SHA256` in
`cmd/seaweedfs-mount/Dockerfile.Windows`, and ships the released user-mode DLL
beside `weed.exe`. Before it accepts mounts, `seaweedfs-mount.exe` checks
`HKLM\SOFTWARE\WOW6432Node\WinFsp`; when WinFsp is missing it copies
`winfsp.msi` from beside the executable to `%SystemRoot%\Temp` (the Windows
Installer service cannot read the HostProcess sandbox mount, exit 1619), runs
`msiexec /i … /qn INSTALLLEVEL=1000 /l*v %SystemRoot%\Temp\seaweedfs-winfsp-install.log`,
and fails startup on a non-zero exit, naming the log. It then requires the
installed version (the WinFsp entry's `DisplayVersion` under the Windows
Installer uninstall keys) to be 2.1.25156, the driver the shipped DLL is built
for, and fails startup on any other version. It never upgrades or replaces an
installed WinFsp: do not use a DaemonSet image rollout as a driver-upgrade
mechanism. `--winfspMSI` names another MSI; `--winfspVersion=` (empty) accepts
any installed version, for the lab's kernel-driver candidate lane only.

[AppMana/forks-winfsp-fixes](https://github.com/AppMana/forks-winfsp-fixes)
contains the candidate fixes and isolated native-VM qualification harness.
Its test-signed outputs are not production MSI replacements. A fork rollout
requires a production-signed package, explicit MSI URL/checksum pins, and a
real-VM installation/restart test with the actual CSI network/UNC mount mode
and mixed-OS pod workloads. This qualification belongs in the isolated
Labcontainers Kubernetes harness, not on drained production nodes.
DLL-only substitution cannot deploy a kernel
fix. Keep test signing disabled on cluster hosts.

To package already-tested executables without rebuilding them, run on the
Linux lab host:

```sh
python3 test/kubernetes_lab/package_images.py \
  --inputs /absolute/path/to/qualified-binaries \
  --sha256-file /absolute/path/to/inputs.sha256 \
  --results-root /absolute/path/to/retained-results --builder lin-multi
```

The checksum file uses `sha256sum` format with plain filenames. Required
inputs are `weed[.exe]`, `seaweedfs-mount[.exe]`,
`seaweedfs-csi-driver[.exe]`, and `winfsp.msi`. The existing Dockerfiles retain
their default source-build path; `PAYLOAD_STAGE=prebuilt` requires matching
executable checksum build arguments. The command builds four images in
parallel, checks the packaged bytes/platform/entrypoint, smoke-tests Linux
startup, and retains OCI archives, digests and logs. It never pushes images,
starts VMs or upgrades a host WinFsp installation. These packaging checks do
not substitute for the real-VM workload evidence or production driver signing.

For a driver-only fix, add `--component csi-driver`: only the Linux/Windows
driver executables and their checksums are required, and only those two images
are built. Reuse the already-qualified mount archives unchanged. The selector
can be repeated; its default remains both components. `--winfsp-dll` requires
the mount component.

For the DLL-only variant, add `--winfsp-dll` and include `winfsp-x64.dll` with
its checksum in the input manifest. This mode requires the pinned official
MSI. Verify the running mount process loads the image-local DLL and that the
host SYS remains Microsoft-signed with test signing disabled. Absolute-symlink
fixes that require the fork's kernel driver are not delivered by this option.

For fresh mixed-OS recovery qualification, the existing
`test/kubernetes_lab/run.sh` accepts `CSI_QUALIFICATION_MODE=recovery` together
with all four digest-pinned `CSI_DRIVER_{LINUX,WINDOWS}_IMAGE` and
`CSI_MOUNT_{LINUX,WINDOWS}_IMAGE` inputs. It retains Git LFS, native persistence,
mixed I/O, remount and Windows reboot checks, but emits the distinct
`CSI_RECOVERY_QUALIFICATION_COMPLETE` marker: it is not a full native
conformance pass. The default `full` mode still runs the full native suite.
`LABCONTAINERS_KUBERNETES_CRASH_VERIFY=1` additionally verifies the original
dataset around VM power loss with these image pins, without a kernel-candidate
MSI requirement.

For DLL-only qualification, set both `CSI_WINFSP_DLL_SHA256` and
`CSI_NATIVE_TEST_SHA256`. Put the matching native executable at
`/winfsp-csi-candidate.test.exe` on the hash-pinned qualification ISO. A fresh
fixture copies it to the host tools share only after verifying its hash; a
retained mismatched executable is rejected, not replaced. The running stock
SYS signature/hash and disabled test-signing policy remain mandatory. Do not
combine these settings with `CSI_CANDIDATE_BUNDLE` (the lab kernel-driver lane).

The build workflow publishes only
`candidate-<source-sha>-<run-id>-<run-attempt>` tags. These are source-build
candidates, not qualified releases or the prebuilt DLL-only images. For release,
promote the exact runtime-qualified OCI digests without rebuilding, use a new
release tag, and pin those digests in deployment manifests. Never republish an
existing deployed tag with newly built bytes.

To publish an already-packaged OCI archive without rebuilding it:

```sh
python3 test/kubernetes_lab/publish_candidate.py \
  --archive /absolute/path/to/mount-linux.tar \
  --record /absolute/path/to/mount-linux-result.json \
  --component mount --results-root /absolute/path/to/retained-results --publish
```

Omit `--publish` for local verification only. The packaging record must match
the archive checksum, manifest/config digests, platform, entrypoint and payload
hashes. Publication uses `ghcr.io/appmana/seaweedfs-<component>:candidate-<digest>`
and verifies the registry digest afterward. It cannot select a stable release
tag; a successful publication receipt is not a runtime-test waiver. Registry
credentials come from the normal `crane` credential configuration.

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

Tuning: set `SEAWEEDFS_WINFSP_OPTIONS=FileInfoTimeout=-1` on the mount
DaemonSet for **read-mostly** volumes (model caches etc.) to enable kernel
data caching — a large small-read speedup, but unsafe for volumes that see
delete-then-recreate patterns (see `forks-seaweedfs/WINDOWS_PORT.md`).

Windows mounts are case-insensitive by default. Set
`SEAWEEDFS_WINFSP_CASE_SENSITIVE=true` on the mount DaemonSet only when a
workload requires the filer namespace's case-sensitive behavior.

`hack/appmana/` contains the kind + QEMU-Windows e2e harness (cross-OS RWX
matrix and resilience drills) and the CI mount benchmark.

Upstream README: https://github.com/seaweedfs/seaweedfs-csi-driver
