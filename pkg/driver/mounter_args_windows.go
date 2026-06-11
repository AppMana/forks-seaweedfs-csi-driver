//go:build windows

package driver

// platformMountArgs returns weed mount arguments that only apply on this
// platform. Windows (WinFsp) has no POSIX umask semantics, so no extra
// arguments are needed.
func platformMountArgs() []string {
	return nil
}
