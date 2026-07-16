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

**Large volumes:** the bundled `weed.exe`/`weed` binaries are the large-disk
build (`-tags 5BytesOffset`, 5-byte needle offsets for 8TB volume files),
matching clusters that run the upstream `*_large_disk` images.

Images (multi-OS manifest lists, `linux/amd64` + `windows/amd64` ltsc2022):

```
ghcr.io/appmana/seaweedfs-csi-driver:v1.4.12-appmana.post.5
ghcr.io/appmana/seaweedfs-mount:v1.4.12-appmana.post.5
```

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
