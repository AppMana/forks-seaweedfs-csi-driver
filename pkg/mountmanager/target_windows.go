//go:build windows

package mountmanager

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/glog"
)

// isReparsePoint reports whether path exists and is a reparse point.
// WinFsp directory mount points and symlinks are both reparse points.
func isReparsePoint(path string) bool {
	fi, err := os.Lstat(path)
	if err != nil {
		return false
	}
	if attrs, ok := fi.Sys().(*syscall.Win32FileAttributeData); ok {
		return attrs.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0
	}
	return fi.Mode()&os.ModeSymlink != 0
}

// ensureTargetClean makes sure targetPath is ready for a fresh weed
// mount. WinFsp directory mount points must NOT pre-exist, so kubelet's
// pre-created (empty) globalmount directory, a dangling reparse point
// from a killed weed.exe, or a leftover symlink are removed with
// os.Remove (RemoveDirectory — never recursive). The directory is NOT
// re-created: weed.exe creates the mount point itself.
func ensureTargetClean(targetPath string) error {
	if _, err := os.Lstat(targetPath); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("inspecting target %s: %w", targetPath, err)
	}
	if err := os.Remove(targetPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("target %s exists and cannot be removed for WinFsp mount: %w", targetPath, err)
	}
	return nil
}

// cleanupDeadMountPoint cleans up the mount point left behind by a weed
// mount process that has exited. After a clean unmount WinFsp removes
// the reparse point itself; after a crash a dangling reparse point may
// remain, which os.Remove deletes without recursing.
func cleanupDeadMountPoint(target string) {
	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		glog.Warningf("failed to remove dead mount point %s: %v", target, err)
	}
}

// waitForMount polls until the target becomes a reparse point, i.e. the
// WinFsp mount has been created by weed.exe, or the timeout elapses.
// The path not existing yet or still being a plain directory both mean
// "keep polling".
func waitForMount(path string, timeout time.Duration) error {
	var elapsed time.Duration
	interval := 10 * time.Millisecond

	for {
		if isReparsePoint(path) {
			return nil
		}

		time.Sleep(interval)
		elapsed += interval
		if elapsed >= timeout {
			return errors.New("timeout waiting for mount")
		}
	}
}
