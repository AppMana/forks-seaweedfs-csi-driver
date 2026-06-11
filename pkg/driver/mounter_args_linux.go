//go:build linux

package driver

// platformMountArgs returns weed mount arguments that only apply on this
// platform. On Linux the FUSE mount is opened up with umask 000 so that
// arbitrary pod UIDs can use the volume.
func platformMountArgs() []string {
	return []string{"-umask=000"}
}
