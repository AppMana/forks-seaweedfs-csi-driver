//go:build !windows

package main

// ensureHostPrerequisites: Linux FUSE needs nothing installed on the host.
func ensureHostPrerequisites() error { return nil }
