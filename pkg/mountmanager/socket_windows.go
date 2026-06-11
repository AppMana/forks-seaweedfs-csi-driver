//go:build windows

package mountmanager

// DefaultSocketDir is the default directory for volume sockets. AF_UNIX
// socket paths must stay well under the 108-byte limit, so a short path
// is used.
const DefaultSocketDir = `C:\var\lib\seaweedfs-mount`
