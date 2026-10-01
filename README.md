# forks-seaweedfs-csi-driver — SeaweedFS CSI with Windows node support

AppMana fork of [seaweedfs/seaweedfs-csi-driver](https://github.com/seaweedfs/seaweedfs-csi-driver)
(v1.4.12). It makes `seaweedfs-storage` PersistentVolumes mount on **Windows**
Kubernetes nodes: the same StorageClass and driver serve Linux and Windows
pods, including cross-OS ReadWriteMany.

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
- WinFsp is installed on the host idempotently by an initContainer from the
  MSI shipped in the mount image.

The Windows mount image currently pins upstream WinFsp **2.1.25156** using
`WINFSP_MSI_URL` and `WINFSP_MSI_SHA256` in
`cmd/seaweedfs-mount/Dockerfile.Windows`. WinFsp's DLL and kernel driver are
host-installed components in the default image. The optional `dll-only` target
instead places a hash-pinned DLL beside `weed.exe` in the mount HostProcess
image; it retains the official MSI and does not replace the host kernel driver.
The bootstrap shown below only checks for an existing registry key: it does
**not** verify or upgrade an existing installation when the image changes.
Do not use a DaemonSet image rollout as a driver-upgrade mechanism.

[AppMana/forks-winfsp-fixes](https://github.com/AppMana/forks-winfsp-fixes)
contains the candidate fixes and isolated native-VM qualification harness.
Its test-signed outputs are not production MSI replacements. A fork rollout
requires a production-signed package, explicit MSI URL/checksum pins, and a
real-VM installation/restart test with the actual CSI network/UNC mount mode
and mixed-OS pod workloads. This qualification belongs in the isolated
Labcontainers Kubernetes harness, not on drained production nodes.
DLL-only substitution cannot deploy a kernel
fix. Keep test signing disabled on cluster hosts.

**Large volumes:** the bundled `weed.exe`/`weed` binaries are the large-disk
build (`-tags 5BytesOffset`, 5-byte needle offsets for 8TB volume files),
matching clusters that run the upstream `*_large_disk` images.

Images (multi-OS manifest lists, `linux/amd64` + `windows/amd64` ltsc2022):

```
ghcr.io/appmana/seaweedfs-csi-driver:v1.4.12-appmana.post.5
ghcr.io/appmana/seaweedfs-mount:v1.4.12-appmana.post.5
```

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
DaemonSets. Minimal example (adjust the filer address and images):

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
      initContainers:
        - name: install-winfsp
          image: ghcr.io/appmana/seaweedfs-mount:v1.4.12-appmana.post.5
          command: ["powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command"]
          args:
            - >-
              if (-not (Test-Path 'HKLM:\SOFTWARE\WOW6432Node\WinFsp')) {
              Copy-Item "$env:CONTAINER_SANDBOX_MOUNT_POINT\winfsp.msi" 'C:\Windows\Temp\winfsp.msi' -Force ;
              $p = Start-Process msiexec -Wait -PassThru -ArgumentList '/i','C:\Windows\Temp\winfsp.msi','/qn','INSTALLLEVEL=1000' ;
              if ($p.ExitCode -ne 0) { exit 1 } } ;
              New-Item -ItemType Directory -Force -Path C:\var\lib\seaweedfs-mount, C:\var\cache\seaweedfs | Out-Null
      containers:
        - name: seaweedfs-mount
          image: ghcr.io/appmana/seaweedfs-mount:v1.4.12-appmana.post.5
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
          image: ghcr.io/appmana/seaweedfs-csi-driver:v1.4.12-appmana.post.5
          command: ["$env:CONTAINER_SANDBOX_MOUNT_POINT/seaweedfs-csi-driver.exe"]
          args:
            - --endpoint=unix://C:\var\lib\kubelet\plugins\seaweedfs-csi-driver\csi.sock
            # use the FQDN: weed.exe runs as a host process and short
            # service names do not resolve on Windows hosts
            - --filer=seaweedfs-filer.seaweedfs.svc.cluster.local:8888
            - --nodeid=$(NODE_ID)
            - --driverName=seaweedfs-csi-driver
            - --mountEndpoint=unix://C:\var\lib\seaweedfs-mount\seaweedfs-mount.sock
            - --cacheDir=C:\var\cache\seaweedfs
            - --cacheCapacityMB=51200
            - --stagingScanDir=C:\var\lib\kubelet\plugins\kubernetes.io\csi
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
