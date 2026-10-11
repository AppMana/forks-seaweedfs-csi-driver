//go:build !linux

package mountmanager

// removeUnheldInstanceCaches needs /proc to find the holders of a cache
// directory; elsewhere it keeps every instance cache.
func removeUnheldInstanceCaches(cacheDir, procRoot string) {}
