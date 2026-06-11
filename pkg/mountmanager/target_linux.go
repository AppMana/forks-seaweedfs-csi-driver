//go:build linux

package mountmanager

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/glog"
	"k8s.io/mount-utils"
)

var kubeMounter = mount.New("")

// ensureTargetClean makes sure targetPath is ready for a fresh weed
// mount: any existing or corrupted mount is unmounted, and the path is
// (re-)created as a plain directory for FUSE to mount over.
func ensureTargetClean(targetPath string) error {
	// Use IsLikelyNotMountPoint instead of deprecated IsMountPoint
	notMnt, err := kubeMounter.IsLikelyNotMountPoint(targetPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Path does not exist, which is a clean state. Directory will be created below.
		} else if mount.IsCorruptedMnt(err) {
			glog.Warningf("Target path %s is a corrupted mount, attempting to unmount", targetPath)
			if unmountErr := kubeMounter.Unmount(targetPath); unmountErr != nil {
				return fmt.Errorf("failed to unmount corrupted mount %s: %w", targetPath, unmountErr)
			}
		} else {
			return err
		}
	} else if !notMnt {
		glog.Infof("Target path %s is an existing mount, attempting to unmount", targetPath)
		if unmountErr := kubeMounter.Unmount(targetPath); unmountErr != nil {
			return fmt.Errorf("failed to unmount existing mount %s: %w", targetPath, unmountErr)
		}
	}

	// Ensure the path exists and is a directory.
	return os.MkdirAll(targetPath, 0755)
}

// cleanupDeadMountPoint cleans up the mount point left behind by a weed
// mount process that has exited.
func cleanupDeadMountPoint(target string) {
	_ = kubeMounter.Unmount(target)
}

// waitForMount polls until path is a mount point or the timeout elapses.
func waitForMount(path string, timeout time.Duration) error {
	var elapsed time.Duration
	interval := 10 * time.Millisecond

	for {
		notMount, err := kubeMounter.IsLikelyNotMountPoint(path)
		if err != nil {
			return err
		}
		if !notMount {
			return nil
		}

		time.Sleep(interval)
		elapsed += interval
		if elapsed >= timeout {
			return errors.New("timeout waiting for mount")
		}
	}
}
