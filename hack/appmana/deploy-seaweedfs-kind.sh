#!/bin/bash
# Deploy a single all-in-one SeaweedFS server into the kind lab cluster.
#
# Exposure choice: the pod runs with hostNetwork pinned to one kind node, so
# the filer listens directly on the kind node container IP (172.21.0.x). The
# Windows QEMU node (10.2.0.180) reaches 172.21.0.0/16 through the host
# forwarding rules applied by apply-kind-qemu-forwarding.sh in the calico
# fork (DOCKER-USER accepts plus the Windows static routes via the host br0
# address), so its hostNetwork mount daemons can dial the filer without any
# extra plumbing. hostNetwork also keeps the weed gRPC port convention
# intact (gRPC = HTTP + 10000: 8888->18888, 9333->19333, 8080->18080),
# which a NodePort Service cannot express inside 30000-32767.
#
# Usage:
#   deploy-seaweedfs-kind.sh [--namespace NS] [--image IMAGE] [--single-node]
#                            [--filer HOST:PORT]
#
# --single-node (default) deploys the all-in-one pod. --filer skips the
# deploy and only verifies the given filer is reachable from inside the
# cluster (HTTP and gRPC = port + 10000).
#
# Prints "FILER=<host>:<port>" as the final line for the next stage.

set -euo pipefail

export KUBECONFIG="${KUBECONFIG:-/tmp/appmana-calico-kind/kubeconfig}"
LAB_CONTEXT="${LAB_CONTEXT:-kind-appmana-calico}"

NAMESPACE="seaweedfs-test"
IMAGE="chrislusf/seaweedfs:4.23_large_disk"
SEAWEEDFS_NODE="${SEAWEEDFS_NODE:-appmana-calico-worker2}"
PROBE_IMAGE="${PROBE_IMAGE:-busybox:1.36}"
FILER=""

while [[ $# -gt 0 ]]; do
  case $1 in
    --namespace) NAMESPACE="$2"; shift 2 ;;
    --image) IMAGE="$2"; shift 2 ;;
    --single-node) shift ;;
    --filer) FILER="$2"; shift 2 ;;
    *) echo "Usage: $0 [--namespace NS] [--image IMAGE] [--single-node] [--filer HOST:PORT]" >&2; exit 1 ;;
  esac
done

current_context=$(kubectl config current-context 2>/dev/null || true)
if [[ "$current_context" != "$LAB_CONTEXT" ]]; then
  echo "ERROR: refusing to run: kubectl context is '${current_context:-<none>}', expected the kind lab context '$LAB_CONTEXT'." >&2
  echo "These scripts must never target the production cluster." >&2
  exit 1
fi

first_ipv4() {
  tr ' ' '\n' | awk '/^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$/ { print; exit }'
}

kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -

probe_filer() {
  local filer_host="$1" filer_port="$2" grpc_port probe_cmd overrides
  grpc_port=$((filer_port + 10000))
  probe_cmd="for i in \$(seq 1 60); do wget -q -O /dev/null http://$filer_host:$filer_port/ && nc -z -w 3 $filer_host $grpc_port && echo filer-ok && exit 0; sleep 2; done; echo filer-unreachable; exit 1"
  overrides=$(cat <<EOF
{
  "spec": {
    "restartPolicy": "Never",
    "containers": [{
      "name": "probe",
      "image": "$PROBE_IMAGE",
      "command": ["sh", "-c", "$probe_cmd"]
    }]
  }
}
EOF
)
  kubectl delete pod swfs-filer-probe -n "$NAMESPACE" --ignore-not-found >/dev/null 2>&1 || true
  kubectl run swfs-filer-probe --image="$PROBE_IMAGE" --restart=Never -n "$NAMESPACE" \
    --overrides="$overrides"
  if ! kubectl wait -n "$NAMESPACE" --for=jsonpath='{.status.phase}'=Succeeded \
    pod/swfs-filer-probe --timeout=240s >/dev/null 2>&1; then
    echo "ERROR: filer probe pod did not succeed; $filer_host:$filer_port (gRPC $grpc_port) is not reachable from inside the cluster" >&2
    kubectl logs -n "$NAMESPACE" swfs-filer-probe 2>/dev/null || true
    exit 1
  fi
  if ! kubectl logs -n "$NAMESPACE" swfs-filer-probe | grep -Fq filer-ok; then
    echo "ERROR: filer probe pod succeeded but did not report filer-ok" >&2
    exit 1
  fi
  kubectl delete pod swfs-filer-probe -n "$NAMESPACE" --ignore-not-found >/dev/null 2>&1 || true
  echo "Filer $filer_host:$filer_port reachable from inside the cluster (HTTP $filer_port, gRPC $grpc_port)."
}

if [[ -n "$FILER" ]]; then
  filer_host="${FILER%:*}"
  filer_port="${FILER##*:}"
  echo "Skipping SeaweedFS deploy; verifying provided filer $FILER."
  probe_filer "$filer_host" "$filer_port"
  echo "FILER=$FILER"
  exit 0
fi

kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: seaweedfs
  namespace: $NAMESPACE
  labels:
    app: seaweedfs
spec:
  hostNetwork: true
  dnsPolicy: ClusterFirstWithHostNet
  nodeSelector:
    kubernetes.io/hostname: $SEAWEEDFS_NODE
    kubernetes.io/os: linux
  containers:
    - name: seaweedfs
      image: $IMAGE
      imagePullPolicy: IfNotPresent
      args:
        - server
        - -filer
        - -master.volumeSizeLimitMB=64
        # Every CSI volume is its own collection and grabs ~7 volume
        # slots on first write; 10 slots exhaust after a single run and
        # subsequent writes silently produce zero-byte files. 100 slots
        # (max ~6.4GB) survive repeated runs.
        - -volume.max=100
      ports:
        - containerPort: 9333
        - containerPort: 19333
        - containerPort: 8080
        - containerPort: 18080
        - containerPort: 8888
        - containerPort: 18888
      readinessProbe:
        httpGet:
          path: /
          port: 8888
        initialDelaySeconds: 5
        periodSeconds: 5
---
apiVersion: v1
kind: Service
metadata:
  name: seaweedfs
  namespace: $NAMESPACE
spec:
  selector:
    app: seaweedfs
  ports:
    - name: filer-http
      port: 8888
      targetPort: 8888
    - name: filer-grpc
      port: 18888
      targetPort: 18888
    - name: master-http
      port: 9333
      targetPort: 9333
    - name: master-grpc
      port: 19333
      targetPort: 19333
    - name: volume-http
      port: 8080
      targetPort: 8080
    - name: volume-grpc
      port: 18080
      targetPort: 18080
EOF

kubectl wait -n "$NAMESPACE" --for=condition=Ready pod/seaweedfs --timeout=300s

filer_host=$(
  kubectl get node "$SEAWEEDFS_NODE" \
    -o jsonpath='{range .status.addresses[?(@.type=="InternalIP")]}{.address}{" "}{end}' |
    first_ipv4
)
if [[ -z "$filer_host" ]]; then
  echo "ERROR: could not determine IPv4 InternalIP for node $SEAWEEDFS_NODE" >&2
  exit 1
fi

probe_filer "$filer_host" 8888

echo ""
echo "SeaweedFS all-in-one ready on $SEAWEEDFS_NODE (host network)."
echo "Filer HTTP:  $filer_host:8888"
echo "Filer gRPC:  $filer_host:18888"
echo "FILER=$filer_host:8888"
