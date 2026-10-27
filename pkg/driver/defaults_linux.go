//go:build linux

package driver

// DefaultCacheDir is the node plugin's cache base when --cacheDir is not set.
const DefaultCacheDir = "/var/cache/seaweedfs"

// DefaultStagingScanDir is kubelet's CSI plugins directory under its default
// root (--root-dir=/var/lib/kubelet). Distributions with another kubelet root,
// such as k0s (/var/lib/k0s/kubelet), set --stagingScanDir.
const DefaultStagingScanDir = "/var/lib/kubelet/plugins/kubernetes.io/csi"
