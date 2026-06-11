//go:build windows

package driver

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/glog"
)

// On Windows the staging path is a WinFsp directory mount point, which is
// implemented as a reparse point, and the publish path is a directory
// symlink (also a reparse point) onto the staging path. There is no
// k8s.io/mount-utils mount table to consult; mount state is derived from
// the file attributes of the paths themselves.

// isReparsePointInfo reports whether fi describes a reparse point
// (WinFsp directory mount points and symlinks are both reparse points).
func isReparsePointInfo(fi os.FileInfo) bool {
	if attrs, ok := fi.Sys().(*syscall.Win32FileAttributeData); ok {
		return attrs.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0
	}
	return fi.Mode()&os.ModeSymlink != 0
}

// isReparsePoint reports whether path exists and is a reparse point.
func isReparsePoint(path string) bool {
	fi, err := os.Lstat(path)
	if err != nil {
		return false
	}
	return isReparsePointInfo(fi)
}

// removeMountArtifact removes a reparse point, symlink, or empty
// directory at path with os.Remove (RemoveDirectory under the hood,
// which removes a directory reparse point WITHOUT recursing into the
// mounted filesystem). It tolerates the path not existing.
func removeMountArtifact(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// isStagingPathHealthy checks if the staging path has a healthy WinFsp
// mount: the path must be a reparse point and must be readable through
// the mounted filesystem.
func isStagingPathHealthy(stagingPath string) bool {
	if !isReparsePoint(stagingPath) {
		glog.V(4).Infof("staging path %s is not a mount point", stagingPath)
		return false
	}

	// Try to read the directory to verify the WinFsp filesystem is
	// responsive. A dangling reparse point (weed.exe killed) fails here.
	if _, err := os.ReadDir(stagingPath); err != nil {
		glog.Warningf("staging path %s is not readable (WinFsp mount may be dead): %v", stagingPath, err)
		return false
	}

	glog.V(4).Infof("staging path %s is healthy", stagingPath)
	return true
}

// cleanupCorruptedStagingPath removes the dangling reparse point left
// behind by a dead weed.exe. os.Remove never recurses, so it cannot
// propagate deletes through a live mount.
func cleanupCorruptedStagingPath(stagingPath string) error {
	if err := removeMountArtifact(stagingPath); err != nil {
		glog.Warningf("failed to cleanup corrupted mount point %s: %v", stagingPath, err)
		return err
	}
	glog.Infof("successfully cleaned up corrupted staging path %s", stagingPath)
	return nil
}

// cleanupStaleStagingPath cleans up a stale staging mount point: a
// dangling reparse point or a leftover empty directory. It never
// removes recursively, so a live WinFsp mount can not have its data
// deleted through this path (os.Remove of a non-empty mounted directory
// fails, and removing the reparse point itself only detaches it).
func cleanupStaleStagingPath(stagingPath string) error {
	glog.Infof("cleaning up stale staging path %s", stagingPath)
	if err := removeMountArtifact(stagingPath); err != nil {
		glog.Warningf("failed to remove staging path %s: %v", stagingPath, err)
		return err
	}
	glog.Infof("successfully cleaned up staging path %s", stagingPath)
	return nil
}

// waitForMount polls until the path becomes a reparse point, i.e. the
// WinFsp mount has been created by weed.exe. The path not existing yet
// (weed.exe removes the pre-created directory before mounting) or still
// being a plain directory both mean "keep polling".
func waitForMount(path string, timeout time.Duration) error {
	var elapsed time.Duration
	var interval = 10 * time.Millisecond
	for {
		if isReparsePoint(path) {
			return nil
		}
		time.Sleep(interval)
		elapsed = elapsed + interval
		if elapsed >= timeout {
			return errors.New("timeout waiting for mount")
		}
	}
}

// checkMount reports whether targetPath is already "mounted", i.e. is a
// reparse point (symlink or WinFsp mount point). Unlike the Linux
// implementation it must NOT create a missing directory: staging mounts
// WinFsp directly at the path (which must not pre-exist) and publish
// replaces the path with a symlink.
func checkMount(targetPath string) (bool, error) {
	fi, err := os.Lstat(targetPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if isReparsePointInfo(fi) {
		return true, nil
	}
	return false, nil
}

// defaultBindMount implements BindMountFn on Windows: kubelet's
// pre-created (empty) target directory is removed and replaced with a
// directory symlink onto the staging path. Windows has no bind mounts
// for arbitrary directories without involving extra filter drivers.
func defaultBindMount(source, target string, readOnly bool) error {
	if readOnly {
		glog.Warningf("read-only publish of %s: on Windows per-publish read-only is enforced at the FUSE level (weed mount -readOnly), not by the symlink", target)
	}
	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing pre-created publish target %s: %w", target, err)
	}
	return os.Symlink(source, target)
}

// unmountVolume removes the reparse point or symlink at path. It
// tolerates the path not existing.
func unmountVolume(path string) error {
	return removeMountArtifact(path)
}

// cleanupMountPoint removes the symlink (publish path) or dangling
// reparse point at path. It tolerates the path not existing.
func cleanupMountPoint(path string) error {
	return removeMountArtifact(path)
}

// isCorruptedMount reports whether err indicates a corrupted mount.
// Windows has no ENOTCONN-style corrupted-FUSE errno surfaced through
// file operations; dead WinFsp mounts are detected as dangling reparse
// points instead.
func isCorruptedMount(err error) bool {
	return false
}

// isPathMounted reports whether path is currently a LIVE mount point:
// a reparse point whose target filesystem still responds. A dangling
// reparse point left behind by a killed weed.exe (WinFsp directory
// mount or staging symlink to a dead UNC share) is not a live mount;
// treating it as one would block health-monitor recovery forever.
// Cleanup on Windows is os.Remove (never recursive), so no data can be
// deleted through a live mount either way.
func isPathMounted(path string) (bool, error) {
	if !isReparsePoint(path) {
		return false, nil
	}
	if _, err := os.ReadDir(path); err != nil {
		return false, nil
	}
	return true, nil
}
