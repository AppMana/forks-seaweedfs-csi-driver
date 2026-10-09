//go:build linux

package driver

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/seaweedfs/seaweedfs-csi-driver/pkg/mountmanager"
	"github.com/seaweedfs/seaweedfs/weed/glog"
	"golang.org/x/sys/unix"
	"k8s.io/mount-utils"
)

var mountutil = mount.New("")

func detachDeadMountPoint(path string) error {
	err := lazyUnmount(path)
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOENT) {
		return nil
	}
	return err
}

var lazyUnmount = mountmanager.LazyUnmount

var isLikelyNotMountPointFn = mountutil.IsLikelyNotMountPoint

// isStagingPathHealthy checks if the staging path has a healthy FUSE mount.
// It returns true if the path is mounted and accessible, false otherwise.
// A hung daemon is not a usable mount either, so a timed-out statfs probe
// reports unhealthy here — the cleanup guards downstream still refuse to
// remove a live mount.
func isStagingPathHealthy(stagingPath string) bool {
	return stagingPathHealth(stagingPath) == healthOK
}

// stagingPathHealth is the health monitor's tri-state view of the staging
// path. A statfs probe that times out is healthSlow, never healthDead: the
// daemon is hung or waiting on the filer, which does not prove it is gone,
// so it must not count toward destructive recovery.
func stagingPathHealth(stagingPath string) healthResult {
	// Check if path exists
	info, err := os.Stat(stagingPath)
	if err != nil {
		if os.IsNotExist(err) {
			glog.V(4).Infof("staging path %s does not exist", stagingPath)
			return healthDead
		}
		// "Transport endpoint is not connected" or similar FUSE errors
		if mount.IsCorruptedMnt(err) {
			glog.Warningf("staging path %s has corrupted mount: %v", stagingPath, err)
			return healthDead
		}
		glog.V(4).Infof("staging path %s stat error: %v", stagingPath, err)
		return healthDead
	}

	// Check if it's a directory
	if !info.IsDir() {
		glog.Warningf("staging path %s is not a directory", stagingPath)
		return healthDead
	}

	// Check if it's a mount point
	isMnt, err := mountutil.IsMountPoint(stagingPath)
	if err != nil {
		if mount.IsCorruptedMnt(err) {
			glog.Warningf("staging path %s has corrupted mount point: %v", stagingPath, err)
			return healthDead
		}
		glog.V(4).Infof("staging path %s mount point check error: %v", stagingPath, err)
		return healthDead
	}

	if !isMnt {
		glog.V(4).Infof("staging path %s is not a mount point", stagingPath)
		return healthDead
	}

	// The checks above can be answered from the inode-attribute cache even
	// after the daemon is gone; statfs reaches the daemon, so a
	// corrupted-mount errno here proves the mount is dead. The probe is
	// bounded so no caller waits on a hung daemon indefinitely.
	statfsErr, probed := probeStatfs(stagingPath)
	if !probed {
		glog.Warningf("staging path %s statfs probe timed out after %v; the mount is hung, not proven dead", stagingPath, statfsProbeTimeout)
		return healthSlow
	}
	if mount.IsCorruptedMnt(statfsErr) {
		glog.Warningf("staging path %s mount is dead: %v", stagingPath, statfsErr)
		return healthDead
	}

	// Deliberately not calling os.ReadDir(stagingPath) here. It used to be a
	// "FUSE is responsive" probe, but ReadDir enumerates the *entire* root
	// directory, and seaweedfs mount answers a readdir by fetching the whole
	// listing from the filer before returning anything to the kernel — on a
	// bucket with a large root this can legitimately take many minutes even
	// though the mount is perfectly healthy. checkHealth's 5s timeout then
	// marks it unhealthy and triggers a remount, which restarts that same
	// slow listing from scratch: the mount can never finish enumerating and
	// gets stuck in a permanent recovery loop.
	//
	// The statfs probe above is cheap (cost independent of directory size)
	// and catches a dead/disconnected daemon via IsCorruptedMnt (ENOTCONN).
	// That's sufficient liveness evidence without paying for a full
	// directory scan.
	glog.V(4).Infof("staging path %s is healthy", stagingPath)
	return healthOK
}

// cleanupCorruptedStagingPath force-cleans a staging path whose FUSE
// daemon is already dead (ENOTCONN / IsCorruptedMnt). Safe because the
// kernel will reject reads/writes through a corrupted mount, so cleanup
// cannot propagate deletes through a live FUSE.
func cleanupCorruptedStagingPath(stagingPath string) error {
	if err := mount.CleanupMountPoint(stagingPath, mountutil, true); err != nil {
		glog.Warningf("standard cleanup of corrupted staging path %s failed: %v; trying lazy unmount", stagingPath, err)
		if lazyErr := lazyUnmount(stagingPath); lazyErr != nil {
			return fmt.Errorf("cleanup corrupted mount %s: cleanup %v, lazy unmount %v", stagingPath, err, lazyErr)
		}
		// Never recursive: after the detach the directory may hold local
		// writes that landed while nothing was mounted.
		if err := os.Remove(stagingPath); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	// The mount here was detached; a still-blocked probe's result
	// describes the old mount and must not be reused for its replacement.
	resetStatfsProbe(stagingPath)
	glog.Infof("successfully cleaned up corrupted staging path %s", stagingPath)
	return nil
}

// cleanupStaleStagingPath cleans up a stale or corrupted staging mount point.
// It attempts to unmount and remove the directory.
//
// Safety invariant: never recursively delete a staging path. It may still
// be a live FUSE mount, or contain intact local writes exposed after unmount.
// Neither is disposable. Callers must treat a refusal as a hard failure
// rather than re-staging over an undeleted path.
func cleanupStaleStagingPath(stagingPath string) error {
	glog.Infof("cleaning up stale staging path %s", stagingPath)

	// Surface unmount errors. A failed unmount almost always means the
	// FUSE mount is still alive (EBUSY because pods or bind mounts still
	// pin it), and silently dropping the error is what lets the
	// post-unmount cleanup hide an unsuccessful detach.
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
			// Nothing mounted here; any in-flight probe's result is stale.
			resetStatfsProbe(stagingPath)
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
	// while the kernel still routes I/O to the FUSE daemon).
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

	// Remove only an empty directory (or a leftover symlink), never its
	// contents. This also stays safe if a mount appears after the check.
	if err := os.Remove(stagingPath); err != nil && !os.IsNotExist(err) {
		glog.Warningf("failed to remove staging path %s: %v", stagingPath, err)
		return err
	}

	resetStatfsProbe(stagingPath)
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
			resetStatfsProbe(targetPath)
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
