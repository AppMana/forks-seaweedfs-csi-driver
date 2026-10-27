//go:build windows

package driver

// DefaultCacheDir is the node plugin's cache base when --cacheDir is not set.
const DefaultCacheDir = `C:\var\cache\seaweedfs`

// DefaultStagingScanDir is kubelet's CSI plugins directory under its default
// Windows root (C:\var\lib\kubelet).
const DefaultStagingScanDir = `C:\var\lib\kubelet\plugins\kubernetes.io\csi`
