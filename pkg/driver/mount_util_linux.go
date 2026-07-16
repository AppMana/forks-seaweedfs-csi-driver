//go:build linux

package driver

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/glog"
	"golang.org/x/sys/unix"
	"k8s.io/mount-utils"
)

var mountutil = mount.New("")

func detachDeadMountPoint(path string) error {
	err := unix.Unmount(path, unix.MNT_DETACH)
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOENT) {
		return nil
	}
	return err
}

// isStagingPathHealthy checks if the staging path has a healthy FUSE mount.
// It returns true if the path is mounted and accessible, false otherwise.
func isStagingPathHealthy(stagingPath string) bool {
	// Check if path exists
	info, err := os.Stat(stagingPath)
	if err != nil {
		if os.IsNotExist(err) {
			glog.V(4).Infof("staging path %s does not exist", stagingPath)
			return false
		}
		// "Transport endpoint is not connected" or similar FUSE errors
		if mount.IsCorruptedMnt(err) {
			glog.Warningf("staging path %s has corrupted mount: %v", stagingPath, err)
			return false
		}
		glog.V(4).Infof("staging path %s stat error: %v", stagingPath, err)
		return false
	}

	// Check if it's a directory
	if !info.IsDir() {
		glog.Warningf("staging path %s is not a directory", stagingPath)
		return false
	}

	// Check if it's a mount point
	isMnt, err := mountutil.IsMountPoint(stagingPath)
	if err != nil {
		if mount.IsCorruptedMnt(err) {
			glog.Warningf("staging path %s has corrupted mount point: %v", stagingPath, err)
			return false
		}
		glog.V(4).Infof("staging path %s mount point check error: %v", stagingPath, err)
		return false
	}

	if !isMnt {
		glog.V(4).Infof("staging path %s is not a mount point", stagingPath)
		return false
	}

	// Try to read the directory to verify FUSE is responsive
	_, err = os.ReadDir(stagingPath)
	if err != nil {
		glog.Warningf("staging path %s is not readable (FUSE may be dead): %v", stagingPath, err)
		return false
	}

	glog.V(4).Infof("staging path %s is healthy", stagingPath)
	return true
}

// cleanupCorruptedStagingPath force-cleans a staging path whose FUSE
// daemon is already dead (ENOTCONN / IsCorruptedMnt). Safe because the
// kernel will reject reads/writes through a corrupted mount, so cleanup
// cannot propagate deletes through a live FUSE.
func cleanupCorruptedStagingPath(stagingPath string) error {
	if err := mount.CleanupMountPoint(stagingPath, mountutil, true); err != nil {
		glog.Warningf("failed to cleanup corrupted mount point %s: %v", stagingPath, err)
		return err
	}
	glog.Infof("successfully cleaned up corrupted staging path %s", stagingPath)
	return nil
}

// cleanupStaleStagingPath cleans up a stale or corrupted staging mount point.
// It attempts to unmount and remove the directory.
//
// Safety invariant: this function MUST NOT call os.RemoveAll on a path that
// is still a live mount point. If the staging path is still a working FUSE
// mount, RemoveAll would walk into the mount and recursively unlink user
// data through it. Callers (health monitor recovery, NodeStage/NodePublish
// re-stage) must treat a refusal as a hard failure rather than re-staging
// over an undeleted mount.
func cleanupStaleStagingPath(stagingPath string) error {
	glog.Infof("cleaning up stale staging path %s", stagingPath)

	// Surface unmount errors. A failed unmount almost always means the
	// FUSE mount is still alive (EBUSY because pods or bind mounts still
	// pin it), and silently dropping the error is what lets the
	// post-unmount RemoveAll below recurse into a live mount.
	unmountErr := mountutil.Unmount(stagingPath)
	if unmountErr != nil {
		glog.Warningf("unmount staging path %s failed: %v", stagingPath, unmountErr)
	}

	// Use Lstat so a leftover dangling symlink at stagingPath is still
	// discoverable (and removable) instead of being mis-classified by
	// stat-following ENOENT.
	_, statErr := os.Lstat(stagingPath)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			glog.Infof("successfully cleaned up staging path %s", stagingPath)
			return nil
		}
		if mount.IsCorruptedMnt(statErr) {
			return cleanupCorruptedStagingPath(stagingPath)
		}
		glog.Warningf("stat on staging path %s failed during cleanup: %v", stagingPath, statErr)
		return statErr
	}

	// Re-check whether the path is still a mount point AFTER unmount.
	// If it is, the unmount failed (or completed only as a lazy detach
	// while the kernel still routes I/O to the FUSE daemon) and
	// RemoveAll would walk a live FUSE mount.
	isMnt, mntErr := mountutil.IsMountPoint(stagingPath)
	if mntErr != nil {
		if mount.IsCorruptedMnt(mntErr) {
			return cleanupCorruptedStagingPath(stagingPath)
		}
		return fmt.Errorf("check mount point %s after unmount: %w", stagingPath, mntErr)
	}
	if isMnt {
		return fmt.Errorf("refuse to remove staging path %s: still a mount point after unmount (unmount err: %v); not deleting through a live FUSE", stagingPath, unmountErr)
	}

	if err := os.RemoveAll(stagingPath); err != nil {
		glog.Warningf("failed to remove staging path %s: %v", stagingPath, err)
		return err
	}

	glog.Infof("successfully cleaned up staging path %s", stagingPath)
	return nil
}

func waitForMount(path string, timeout time.Duration) error {
	var elapsed time.Duration
	var interval = 10 * time.Millisecond
	for {
		notMount, err := mountutil.IsLikelyNotMountPoint(path)
		if err != nil {
			return err
		}
		if !notMount {
			return nil
		}
		time.Sleep(interval)
		elapsed = elapsed + interval
		if elapsed >= timeout {
			return errors.New("timeout waiting for mount")
		}
	}
}

// checkMount reports whether targetPath is already a mount point,
// creating the directory if it does not exist yet.
func checkMount(targetPath string) (bool, error) {
	isMnt, err := mountutil.IsMountPoint(targetPath)
	if err != nil {
		if os.IsNotExist(err) {
			if err = os.MkdirAll(targetPath, 0750); err != nil {
				return false, err
			}
			isMnt = false
		} else if mount.IsCorruptedMnt(err) {
			if err := mountutil.Unmount(targetPath); err != nil {
				return false, err
			}
			isMnt, err = mountutil.IsMountPoint(targetPath)
		} else {
			return false, err
		}
	}
	return isMnt, nil
}

// defaultBindMount performs a real bind mount via mountutil. It is the
// production implementation of BindMountFn.
func defaultBindMount(source, target string, readOnly bool) error {
	mountOptions := []string{"bind"}
	if readOnly {
		mountOptions = append(mountOptions, "ro")
	}
	return mountutil.Mount(source, target, "", mountOptions)
}

// unmountVolume unmounts the mount at path.
func unmountVolume(path string) error {
	return mountutil.Unmount(path)
}

// cleanupMountPoint unmounts (forcefully if needed) and removes the
// mount point at path.
func cleanupMountPoint(path string) error {
	return mount.CleanupMountPoint(path, mountutil, true)
}

// isCorruptedMount reports whether err indicates a corrupted mount
// ("transport endpoint is not connected" and friends).
func isCorruptedMount(err error) bool {
	return mount.IsCorruptedMnt(err)
}

// isPathMounted reports whether path is currently a mount point.
func isPathMounted(path string) (bool, error) {
	notMnt, err := mountutil.IsLikelyNotMountPoint(path)
	if err != nil {
		return false, err
	}
	return !notMnt, nil
}
