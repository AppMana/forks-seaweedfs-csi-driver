#!/bin/bash
# Deploy the SeaweedFS CSI driver under test into the kind+QEMU lab.
#
# Renders StorageClass / CSIDriver / RBAC / controller / linux DaemonSets /
# Windows HostProcess DaemonSets from heredoc templates. The shape follows
# deploy/kubernetes/seaweedfs-csi.yaml (controller) and the appmana-cluster
# csi-driver{,-windows}.yaml DaemonSets, except:
#   - kubelet roots are the upstream kind defaults (/var/lib/kubelet and
#     C:\var\lib\kubelet), not the production k0s paths,
#   - the Windows DaemonSets have no hostname pin (the lab Windows node is
#     selected by kubernetes.io/os alone),
#   - images are parameterized; one manifest-list tag serves both OSes
#     (the fork CI publishes linux + windows ltsc2022 under one tag),
#   - lab cache is 1024 MB and liveness sidecars are omitted.
#
# Usage:
#   deploy-csi-driver-kind.sh --csi-image IMG --mount-image IMG --filer HOST:PORT
#                             [--namespace NS] [--kubelet-root-linux DIR]
#                             [--kubelet-root-windows DIR] [--image-pull-secret NAME]

set -euo pipefail

export KUBECONFIG="${KUBECONFIG:-/tmp/appmana-calico-kind/kubeconfig}"
LAB_CONTEXT="${LAB_CONTEXT:-kind-appmana-calico}"

NAMESPACE="seaweedfs-csi"
CSI_IMAGE=""
MOUNT_IMAGE=""
FILER=""
KUBELET_ROOT_LINUX="/var/lib/kubelet"
KUBELET_ROOT_WINDOWS='C:\var\lib\kubelet'
IMAGE_PULL_SECRET="${IMAGE_PULL_SECRET:-}"
CACHE_CAPACITY_MB="${CACHE_CAPACITY_MB:-1024}"

usage() {
  echo "Usage: $0 --csi-image IMG --mount-image IMG --filer HOST:PORT [--namespace NS] [--kubelet-root-linux DIR] [--kubelet-root-windows DIR] [--image-pull-secret NAME]" >&2
  exit 1
}

while [[ $# -gt 0 ]]; do
  case $1 in
    --csi-image) CSI_IMAGE="$2"; shift 2 ;;
    --mount-image) MOUNT_IMAGE="$2"; shift 2 ;;
    --filer) FILER="$2"; shift 2 ;;
    --namespace) NAMESPACE="$2"; shift 2 ;;
    --kubelet-root-linux) KUBELET_ROOT_LINUX="$2"; shift 2 ;;
    --kubelet-root-windows) KUBELET_ROOT_WINDOWS="$2"; shift 2 ;;
    --image-pull-secret) IMAGE_PULL_SECRET="$2"; shift 2 ;;
    *) usage ;;
  esac
done

[[ -n "$CSI_IMAGE" && -n "$MOUNT_IMAGE" && -n "$FILER" ]] || usage

current_context=$(kubectl config current-context 2>/dev/null || true)
if [[ "$current_context" != "$LAB_CONTEXT" ]]; then
  echo "ERROR: refusing to run: kubectl context is '${current_context:-<none>}', expected the kind lab context '$LAB_CONTEXT'." >&2
  echo "These scripts must never target the production cluster." >&2
  exit 1
fi

# Windows paths: single-backslash form for PowerShell / unix:// endpoints,
# escaped form for the registrar env values (mirrors the production YAML).
win_root="$KUBELET_ROOT_WINDOWS"
win_plugin_dir="${win_root}\\plugins\\seaweedfs-csi-driver"
win_csi_sock="${win_plugin_dir}\\csi.sock"
win_root_esc="${win_root//\\/\\\\}"
win_csi_sock_esc="${win_csi_sock//\\/\\\\}"
win_registry_esc="${win_root_esc}\\\\plugins_registry\\\\"

pull_secrets_block=""
if [[ -n "$IMAGE_PULL_SECRET" ]]; then
  pull_secrets_block=$'      imagePullSecrets:\n        - name: '"$IMAGE_PULL_SECRET"
fi

kubectl apply -f - <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: $NAMESPACE
---
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: seaweedfs-storage
provisioner: seaweedfs-csi-driver
allowVolumeExpansion: true
volumeBindingMode: Immediate
---
apiVersion: storage.k8s.io/v1
kind: CSIDriver
metadata:
  name: seaweedfs-csi-driver
spec:
  attachRequired: true
  podInfoOnMount: true
  volumeLifecycleModes:
    - Persistent
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: seaweedfs-controller-sa
  namespace: $NAMESPACE
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: seaweedfs-node-sa
  namespace: $NAMESPACE
---
kind: ClusterRole
apiVersion: rbac.authorization.k8s.io/v1
metadata:
  name: seaweedfs-provisioner-role
rules:
  - apiGroups: [""]
    resources: ["secrets"]
    verbs: ["get", "list"]
  - apiGroups: [""]
    resources: ["persistentvolumes"]
    verbs: ["get", "list", "watch", "create", "delete"]
  - apiGroups: [""]
    resources: ["persistentvolumeclaims"]
    verbs: ["get", "list", "watch", "update"]
  - apiGroups: [""]
    resources: ["persistentvolumeclaims/status"]
    verbs: ["get", "list", "watch", "update", "patch"]
  - apiGroups: ["storage.k8s.io"]
    resources: ["storageclasses"]
    verbs: ["get", "list", "watch"]
  - apiGroups: [""]
    resources: ["events"]
    verbs: ["list", "watch", "create", "update", "patch"]
  - apiGroups: ["snapshot.storage.k8s.io"]
    resources: ["volumesnapshots", "volumesnapshotcontents"]
    verbs: ["get", "list"]
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "list", "watch"]
---
kind: ClusterRole
apiVersion: rbac.authorization.k8s.io/v1
metadata:
  name: seaweedfs-attacher-role
rules:
  - apiGroups: [""]
    resources: ["persistentvolumes"]
    verbs: ["get", "list", "watch", "update", "patch"]
  - apiGroups: [""]
    resources: ["nodes"]
    verbs: ["get", "list", "watch"]
  - apiGroups: ["csi.storage.k8s.io"]
    resources: ["csinodeinfos"]
    verbs: ["get", "list", "watch"]
  - apiGroups: ["storage.k8s.io"]
    resources: ["volumeattachments", "volumeattachments/status"]
    verbs: ["get", "list", "watch", "update", "patch"]
---
kind: ClusterRole
apiVersion: rbac.authorization.k8s.io/v1
metadata:
  name: seaweedfs-driver-registrar-node-role
rules:
  - apiGroups: [""]
    resources: ["events"]
    verbs: ["get", "list", "watch", "create", "update", "patch"]
  - apiGroups: [""]
    resources: ["nodes"]
    verbs: ["get", "list", "watch"]
  - apiGroups: [""]
    resources: ["persistentvolumes"]
    verbs: ["get", "list", "watch"]
---
kind: ClusterRoleBinding
apiVersion: rbac.authorization.k8s.io/v1
metadata:
  name: seaweedfs-provisioner-binding
subjects:
  - kind: ServiceAccount
    name: seaweedfs-controller-sa
    namespace: $NAMESPACE
roleRef:
  kind: ClusterRole
  name: seaweedfs-provisioner-role
  apiGroup: rbac.authorization.k8s.io
---
kind: ClusterRoleBinding
apiVersion: rbac.authorization.k8s.io/v1
metadata:
  name: seaweedfs-attacher-binding
subjects:
  - kind: ServiceAccount
    name: seaweedfs-controller-sa
    namespace: $NAMESPACE
roleRef:
  kind: ClusterRole
  name: seaweedfs-attacher-role
  apiGroup: rbac.authorization.k8s.io
---
kind: ClusterRoleBinding
apiVersion: rbac.authorization.k8s.io/v1
metadata:
  name: seaweedfs-driver-registrar-node-binding
subjects:
  - kind: ServiceAccount
    name: seaweedfs-node-sa
    namespace: $NAMESPACE
roleRef:
  kind: ClusterRole
  name: seaweedfs-driver-registrar-node-role
  apiGroup: rbac.authorization.k8s.io
---
kind: Role
apiVersion: rbac.authorization.k8s.io/v1
metadata:
  name: seaweedfs-leader-election-controller-role
  namespace: $NAMESPACE
rules:
  - apiGroups: ["coordination.k8s.io"]
    resources: ["leases"]
    verbs: ["get", "watch", "list", "delete", "update", "create"]
---
kind: RoleBinding
apiVersion: rbac.authorization.k8s.io/v1
metadata:
  name: seaweedfs-leader-election-controller-binding
  namespace: $NAMESPACE
subjects:
  - kind: ServiceAccount
    name: seaweedfs-controller-sa
    namespace: $NAMESPACE
roleRef:
  kind: Role
  name: seaweedfs-leader-election-controller-role
  apiGroup: rbac.authorization.k8s.io
---
kind: Deployment
apiVersion: apps/v1
metadata:
  name: seaweedfs-controller
  namespace: $NAMESPACE
spec:
  replicas: 1
  selector:
    matchLabels:
      app: seaweedfs-controller
  template:
    metadata:
      labels:
        app: seaweedfs-controller
    spec:
      priorityClassName: system-cluster-critical
      serviceAccountName: seaweedfs-controller-sa
      nodeSelector:
        kubernetes.io/os: linux
$pull_secrets_block
      containers:
        - name: seaweedfs-csi-plugin
          image: $CSI_IMAGE
          imagePullPolicy: IfNotPresent
          args:
            - --endpoint=\$(CSI_ENDPOINT)
            - --filer=\$(SEAWEEDFS_FILER)
            - --driverName=\$(DRIVER_NAME)
            - --components=controller
            - --attacher=true
          env:
            - name: CSI_ENDPOINT
              value: unix:///var/lib/csi/sockets/pluginproxy/csi.sock
            - name: SEAWEEDFS_FILER
              value: "$FILER"
            - name: DRIVER_NAME
              value: seaweedfs-csi-driver
          volumeMounts:
            - name: socket-dir
              mountPath: /var/lib/csi/sockets/pluginproxy/
        - name: csi-provisioner
          image: registry.k8s.io/sig-storage/csi-provisioner:v3.5.0
          imagePullPolicy: IfNotPresent
          args:
            - --csi-address=\$(ADDRESS)
            - --leader-election
            - --leader-election-namespace=$NAMESPACE
          env:
            - name: ADDRESS
              value: /var/lib/csi/sockets/pluginproxy/csi.sock
          volumeMounts:
            - name: socket-dir
              mountPath: /var/lib/csi/sockets/pluginproxy/
        - name: csi-attacher
          image: registry.k8s.io/sig-storage/csi-attacher:v4.3.0
          imagePullPolicy: IfNotPresent
          args:
            - --csi-address=\$(ADDRESS)
            - --leader-election
            - --leader-election-namespace=$NAMESPACE
          env:
            - name: ADDRESS
              value: /var/lib/csi/sockets/pluginproxy/csi.sock
          volumeMounts:
            - name: socket-dir
              mountPath: /var/lib/csi/sockets/pluginproxy/
        - name: csi-resizer
          image: registry.k8s.io/sig-storage/csi-resizer:v1.8.0
          imagePullPolicy: IfNotPresent
          args:
            - --csi-address=\$(ADDRESS)
            - --leader-election
            - --leader-election-namespace=$NAMESPACE
          env:
            - name: ADDRESS
              value: /var/lib/csi/sockets/pluginproxy/csi.sock
          volumeMounts:
            - name: socket-dir
              mountPath: /var/lib/csi/sockets/pluginproxy/
      volumes:
        - name: socket-dir
          emptyDir: {}
---
kind: DaemonSet
apiVersion: apps/v1
metadata:
  name: seaweedfs-mount
  namespace: $NAMESPACE
spec:
  selector:
    matchLabels:
      app: seaweedfs-mount
  template:
    metadata:
      labels:
        app: seaweedfs-mount
    spec:
      nodeSelector:
        kubernetes.io/os: linux
      hostNetwork: true
      dnsPolicy: ClusterFirstWithHostNet
      priorityClassName: system-node-critical
      serviceAccountName: seaweedfs-node-sa
$pull_secrets_block
      containers:
        - name: seaweedfs-mount
          image: $MOUNT_IMAGE
          imagePullPolicy: IfNotPresent
          args:
            - --endpoint=\$(MOUNT_ENDPOINT)
          env:
            - name: MOUNT_ENDPOINT
              value: unix:///var/lib/seaweedfs-mount/seaweedfs-mount.sock
          securityContext:
            allowPrivilegeEscalation: true
            capabilities:
              add:
                - SYS_ADMIN
            privileged: true
          volumeMounts:
            - name: plugins-dir
              mountPath: $KUBELET_ROOT_LINUX/plugins
              mountPropagation: Bidirectional
            - name: pods-mount-dir
              mountPath: $KUBELET_ROOT_LINUX/pods
              mountPropagation: Bidirectional
            - name: device-dir
              mountPath: /dev
            - name: seaweedfs-cache
              mountPath: /var/cache/seaweedfs
            - name: mount-socket-dir
              mountPath: /var/lib/seaweedfs-mount
      volumes:
        - name: plugins-dir
          hostPath:
            path: $KUBELET_ROOT_LINUX/plugins
            type: Directory
        - name: pods-mount-dir
          hostPath:
            path: $KUBELET_ROOT_LINUX/pods
            type: Directory
        - name: device-dir
          hostPath:
            path: /dev
        - name: seaweedfs-cache
          hostPath:
            path: /var/cache/seaweedfs
            type: DirectoryOrCreate
        - name: mount-socket-dir
          hostPath:
            path: /var/lib/seaweedfs-mount
            type: DirectoryOrCreate
  updateStrategy:
    type: RollingUpdate
    rollingUpdate:
      maxUnavailable: 1
---
kind: DaemonSet
apiVersion: apps/v1
metadata:
  name: seaweedfs-node
  namespace: $NAMESPACE
spec:
  selector:
    matchLabels:
      app: seaweedfs-node
  template:
    metadata:
      labels:
        app: seaweedfs-node
    spec:
      nodeSelector:
        kubernetes.io/os: linux
      hostNetwork: true
      hostPID: true
      dnsPolicy: ClusterFirstWithHostNet
      priorityClassName: system-node-critical
      serviceAccountName: seaweedfs-node-sa
$pull_secrets_block
      containers:
        - name: csi-seaweedfs-plugin
          image: $CSI_IMAGE
          imagePullPolicy: IfNotPresent
          args:
            - --endpoint=\$(CSI_ENDPOINT)
            - --filer=\$(SEAWEEDFS_FILER)
            - --nodeid=\$(NODE_ID)
            - --driverName=\$(DRIVER_NAME)
            - --mountEndpoint=\$(MOUNT_ENDPOINT)
            - --cacheDir=/var/cache/seaweedfs
            - --cacheCapacityMB=$CACHE_CAPACITY_MB
            - --components=node
          env:
            - name: CSI_ENDPOINT
              value: unix:///csi/csi.sock
            - name: SEAWEEDFS_FILER
              value: "$FILER"
            - name: NODE_ID
              valueFrom:
                fieldRef:
                  fieldPath: spec.nodeName
            - name: DRIVER_NAME
              value: seaweedfs-csi-driver
            - name: MOUNT_ENDPOINT
              value: unix:///var/lib/seaweedfs-mount/seaweedfs-mount.sock
          securityContext:
            allowPrivilegeEscalation: true
            capabilities:
              add:
                - SYS_ADMIN
            privileged: true
          volumeMounts:
            - name: plugin-dir
              mountPath: /csi
            - name: plugins-dir
              mountPath: $KUBELET_ROOT_LINUX/plugins
              mountPropagation: Bidirectional
            - name: pods-mount-dir
              mountPath: $KUBELET_ROOT_LINUX/pods
              mountPropagation: Bidirectional
            - name: device-dir
              mountPath: /dev
            - name: seaweedfs-cache
              mountPath: /var/cache/seaweedfs
            - name: mount-socket-dir
              mountPath: /var/lib/seaweedfs-mount
        - name: driver-registrar
          image: registry.k8s.io/sig-storage/csi-node-driver-registrar:v2.8.0
          imagePullPolicy: IfNotPresent
          args:
            - --csi-address=\$(ADDRESS)
            - --kubelet-registration-path=\$(DRIVER_REG_SOCK_PATH)
          env:
            - name: ADDRESS
              value: /csi/csi.sock
            - name: DRIVER_REG_SOCK_PATH
              value: $KUBELET_ROOT_LINUX/plugins/seaweedfs-csi-driver/csi.sock
            - name: KUBE_NODE_NAME
              valueFrom:
                fieldRef:
                  fieldPath: spec.nodeName
          volumeMounts:
            - name: plugin-dir
              mountPath: /csi/
            - name: registration-dir
              mountPath: /registration/
      volumes:
        - name: registration-dir
          hostPath:
            path: $KUBELET_ROOT_LINUX/plugins_registry
            type: DirectoryOrCreate
        - name: plugin-dir
          hostPath:
            path: $KUBELET_ROOT_LINUX/plugins/seaweedfs-csi-driver
            type: DirectoryOrCreate
        - name: plugins-dir
          hostPath:
            path: $KUBELET_ROOT_LINUX/plugins
            type: Directory
        - name: pods-mount-dir
          hostPath:
            path: $KUBELET_ROOT_LINUX/pods
            type: Directory
        - name: device-dir
          hostPath:
            path: /dev
        - name: seaweedfs-cache
          hostPath:
            path: /var/cache/seaweedfs
            type: DirectoryOrCreate
        - name: mount-socket-dir
          hostPath:
            path: /var/lib/seaweedfs-mount
            type: DirectoryOrCreate
  updateStrategy:
    type: RollingUpdate
    rollingUpdate:
      maxUnavailable: 1
---
kind: DaemonSet
apiVersion: apps/v1
metadata:
  name: seaweedfs-mount-windows
  namespace: $NAMESPACE
spec:
  selector:
    matchLabels:
      app: seaweedfs-mount-windows
  template:
    metadata:
      labels:
        app: seaweedfs-mount-windows
    spec:
      nodeSelector:
        kubernetes.io/os: windows
      tolerations:
        - operator: Exists
          effect: NoSchedule
        - key: CriticalAddonsOnly
          operator: Exists
      securityContext:
        windowsOptions:
          hostProcess: true
          runAsUserName: "NT AUTHORITY\\\\SYSTEM"
      hostNetwork: true
      dnsPolicy: ClusterFirstWithHostNet
      priorityClassName: system-node-critical
      serviceAccountName: seaweedfs-node-sa
$pull_secrets_block
      initContainers:
        - name: install-winfsp
          image: $MOUNT_IMAGE
          command:
            - powershell.exe
            - -NoProfile
            - -ExecutionPolicy
            - Bypass
            - -Command
            - >-
              if (-not (Test-Path 'HKLM:\SOFTWARE\WOW6432Node\WinFsp')) {
              Start-Process msiexec -Wait -ArgumentList '/i',"\$env:CONTAINER_SANDBOX_MOUNT_POINT\winfsp.msi",'/qn','INSTALLLEVEL=1000' } ;
              New-Item -ItemType Directory -Force -Path C:\var\lib\seaweedfs-mount, C:\var\cache\seaweedfs | Out-Null
      containers:
        - name: seaweedfs-mount
          image: $MOUNT_IMAGE
          imagePullPolicy: IfNotPresent
          command:
            - "\$env:CONTAINER_SANDBOX_MOUNT_POINT/seaweedfs-mount.exe"
          args:
            - --endpoint=\$(MOUNT_ENDPOINT)
          env:
            - name: MOUNT_ENDPOINT
              value: unix://C:\var\lib\seaweedfs-mount\seaweedfs-mount.sock
  updateStrategy:
    type: RollingUpdate
    rollingUpdate:
      # A mount supervisor restart tears down every WinFsp mount on the
      # node (same semantics as the Linux mount DaemonSet).
      maxUnavailable: 1
---
kind: DaemonSet
apiVersion: apps/v1
metadata:
  name: seaweedfs-node-windows
  namespace: $NAMESPACE
spec:
  selector:
    matchLabels:
      app: seaweedfs-node-windows
  template:
    metadata:
      labels:
        app: seaweedfs-node-windows
    spec:
      nodeSelector:
        kubernetes.io/os: windows
      tolerations:
        - operator: Exists
          effect: NoSchedule
        - key: CriticalAddonsOnly
          operator: Exists
      securityContext:
        windowsOptions:
          hostProcess: true
          runAsUserName: "NT AUTHORITY\\\\SYSTEM"
      hostNetwork: true
      dnsPolicy: ClusterFirstWithHostNet
      priorityClassName: system-node-critical
      serviceAccountName: seaweedfs-node-sa
$pull_secrets_block
      initContainers:
        - name: init
          image: $CSI_IMAGE
          command:
            - powershell.exe
            - -NoProfile
            - -Command
            - New-Item -ItemType Directory -Force -Path $win_plugin_dir | Out-Null
      containers:
        - name: csi-seaweedfs-plugin
          image: $CSI_IMAGE
          imagePullPolicy: IfNotPresent
          command:
            - "\$env:CONTAINER_SANDBOX_MOUNT_POINT/seaweedfs-csi-driver.exe"
          args:
            - --endpoint=\$(CSI_ENDPOINT)
            - --filer=\$(SEAWEEDFS_FILER)
            - --nodeid=\$(NODE_ID)
            - --driverName=\$(DRIVER_NAME)
            - --mountEndpoint=\$(MOUNT_ENDPOINT)
            - --cacheDir=C:\var\cache\seaweedfs
            - --cacheCapacityMB=$CACHE_CAPACITY_MB
            - --components=node
          env:
            - name: CSI_ENDPOINT
              value: unix://$win_csi_sock
            - name: SEAWEEDFS_FILER
              value: "$FILER"
            - name: NODE_ID
              valueFrom:
                fieldRef:
                  fieldPath: spec.nodeName
            - name: DRIVER_NAME
              value: seaweedfs-csi-driver
            - name: MOUNT_ENDPOINT
              value: unix://C:\var\lib\seaweedfs-mount\seaweedfs-mount.sock
        - name: driver-registrar
          image: registry.k8s.io/sig-storage/csi-node-driver-registrar:v2.16.0
          imagePullPolicy: IfNotPresent
          command:
            - csi-node-driver-registrar.exe
          args:
            - --csi-address=\$(CSI_ENDPOINT)
            - --kubelet-registration-path=\$(DRIVER_REG_SOCK_PATH)
            - --plugin-registration-path=\$(PLUGIN_REG_DIR)
            - --v=2
          env:
            - name: CSI_ENDPOINT
              value: unix://$win_csi_sock
            - name: DRIVER_REG_SOCK_PATH
              value: $win_csi_sock_esc
            - name: PLUGIN_REG_DIR
              value: $win_registry_esc
            - name: KUBE_NODE_NAME
              valueFrom:
                fieldRef:
                  fieldPath: spec.nodeName
  updateStrategy:
    type: RollingUpdate
    rollingUpdate:
      maxUnavailable: 1
EOF

kubectl rollout status -n "$NAMESPACE" deployment/seaweedfs-controller --timeout=10m
kubectl rollout status -n "$NAMESPACE" ds/seaweedfs-mount --timeout=10m
kubectl rollout status -n "$NAMESPACE" ds/seaweedfs-node --timeout=10m
# Windows image pulls (servercore base layers) are slow on first boot.
kubectl rollout status -n "$NAMESPACE" ds/seaweedfs-mount-windows --timeout=20m
kubectl rollout status -n "$NAMESPACE" ds/seaweedfs-node-windows --timeout=20m

echo ""
echo "SeaweedFS CSI driver deployed to namespace $NAMESPACE (filer $FILER)."
