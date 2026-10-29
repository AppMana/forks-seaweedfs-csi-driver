package driver

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/glog"
)

// quarantineDirName is created directly under the kubelet root, beside pods/:
// kubelet treats every directory under pods/ as a pod and may delete it.
const quarantineDirName = "seaweedfs-quarantine"

// unpublishTarget removes a publish target. A target that is not mounted but
// still holds files (something wrote there while the volume was absent) makes
// cleanup fail on every kubelet retry and keeps the pod terminating forever;
// when a kubelet root is configured it is moved aside intact instead.
func (ns *NodeServer) unpublishTarget(targetPath string, cleanup func(string) error) error {
	err := cleanup(targetPath)
	if err == nil {
		return nil
	}
	if moved := ns.quarantineStaleTarget(targetPath, targetPath); moved != "" {
		glog.Warningf("publish target %s was not mounted but held files (%v); moved it to %s", targetPath, err, moved)
		resetStatfsProbe(targetPath)
		return nil
	}
	return err
}

// quarantineStaleTarget renames the unmounted, nonempty target into the
// quarantine directory and returns its new path, or "" when it does not apply.
// targetPath is the host path kubelet passed; visiblePath is where this
// process sees it. The rename goes through KubeletRootMount, the container
// mount of KubeletRootDir: pods/ and plugins/ are separate bind mounts, and a
// rename between them fails even on one filesystem.
func (ns *NodeServer) quarantineStaleTarget(targetPath, visiblePath string) string {
	root, mount := ns.Driver.KubeletRootDir, ns.Driver.KubeletRootMount
	if root == "" || mount == "" {
		return ""
	}
	rel, err := filepath.Rel(filepath.Join(root, "pods"), targetPath)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return ""
	}
	if mounted, err := isPathMounted(visiblePath); err != nil || mounted {
		return ""
	}
	entries, err := os.ReadDir(visiblePath)
	if err != nil || len(entries) == 0 {
		return ""
	}
	// <uid>/volumes/kubernetes.io~csi/<pvc>/mount names the pod and the volume.
	parts := strings.Split(rel, string(filepath.Separator))
	name := strings.Join(parts[:1], "")
	if len(parts) >= 4 {
		name += "-" + parts[3]
	}
	dir := filepath.Join(mount, quarantineDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		glog.Warningf("quarantine %s: %v", targetPath, err)
		return ""
	}
	dest := filepath.Join(dir, fmt.Sprintf("%s-%s", time.Now().UTC().Format("20060102T150405Z"), name))
	if err := os.Rename(filepath.Join(mount, "pods", rel), dest); err != nil {
		glog.Warningf("quarantine %s: %v", targetPath, err)
		return ""
	}
	return dest
}
