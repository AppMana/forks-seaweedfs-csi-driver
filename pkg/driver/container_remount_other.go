//go:build !linux

package driver

// remountInContainers is a no-op on non-Linux platforms.
// Container mount namespace manipulation requires Linux-specific
// setns(2) and /proc filesystem support.
func remountInContainers(publishPath, stagingPath, oldDevice string, readOnly bool) {}

// repairStaleContainerMounts is a no-op on non-Linux platforms.
func repairStaleContainerMounts(publishPath, stagingPath string) int { return 0 }

// getMountDevice is a stub on non-Linux platforms.
func getMountDevice(mountPath string) (string, error) {
	return "", nil
}
