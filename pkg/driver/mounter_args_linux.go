//go:build linux

package driver

// platformMountArgs returns weed mount arguments that only apply on this
// platform. On Linux the FUSE mount is opened up with umask 000 so that
// arbitrary pod UIDs can use the volume.
func platformMountArgs(volumeSocketDir, volumeID string) []string {
	return []string{"-umask=000"}
}

// mountRootsRecorded: Linux pods see the volume through a bind mount of the
// staging path, so absolute symlinks need no mapping.
const mountRootsRecorded = false

func platformPodVolumeMounts(driverName string) PodVolumeMountsFn {
	return nil
}
