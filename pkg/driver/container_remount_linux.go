package driver

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/glog"
	"golang.org/x/sys/unix"
)

// remountInContainers fixes stale FUSE mounts inside pod containers after
// the health monitor has recovered a volume on the host.
//
// When the CSI driver recovers a dead FUSE mount (unmount stale staging,
// re-stage, re-bind publish paths), the host-level mounts are correct.
// However, container runtimes create the pod's volume bind-mount with
// rprivate propagation by default, so the host-side remount does NOT
// propagate into existing containers. The pod still sees the old dead
// FUSE mount ("Transport endpoint is not connected").
//
// This function enters each affected container's mount namespace via
// setns(2) and creates a fresh bind mount from the recovered staging
// path, replacing the stale one. This requires the CSI driver pod to
// run with hostPID: true so it can see container processes in /proc.
//
// oldDevice is the "major:minor" string of the dead FUSE mount that was
// at stagingPath before recovery. It is used to identify the
// corresponding stale mount entry inside each container's mountinfo.
func remountInContainers(publishPath, stagingPath, oldDevice string, readOnly bool) {
	podUID := extractPodUID(publishPath)
	if podUID == "" {
		glog.V(4).Infof("container remount: could not extract pod UID from %s", publishPath)
		return
	}

	pids, err := findContainerPIDsForPod(podUID)
	if err != nil {
		glog.V(4).Infof("container remount: could not find container PIDs for pod %s: %v (hostPID may not be enabled)", podUID, err)
		return
	}
	if len(pids) == 0 {
		glog.V(4).Infof("container remount: no container PIDs found for pod %s", podUID)
		return
	}

	for _, pid := range pids {
		mounts, err := findContainerMountsByDevice(pid, oldDevice)
		if err != nil || len(mounts) == 0 {
			continue
		}

		for _, m := range mounts {
			glog.Infof("container remount: fixing stale mount %s (subPath %q) in container PID %d (pod %s)", m.mountpoint, m.root, pid, podUID)
			if err := remountViaSetns(pid, m.mountpoint, stagingPath, m.root, readOnly || m.readOnly()); err != nil {
				glog.Warningf("container remount: failed to remount %s in PID %d: %v", m.mountpoint, pid, err)
			} else {
				glog.Infof("container remount: successfully remounted %s in PID %d", m.mountpoint, pid)
			}
		}
	}
}

// containerMountProbeWait bounds the statfs probe of a container's FUSE
// mount. A mount whose daemon is gone answers ENOTCONN at once; one that
// does not answer within this wait is not proven dead and is left alone.
const containerMountProbeWait = 5 * time.Second

// repairStaleContainerMounts replaces, in every container of the pod that
// owns publishPath, each mount of the staged filesystem whose FUSE daemon is
// gone with a fresh bind of the live staging mount, and returns how many it
// replaced.
//
// It is level-triggered: it compares what each container holds now with
// the staging mount, so it repairs a container no matter which event left
// it on a dead mount. remountInContainers only finds container mounts by the
// device of the staging mount a recovery replaces. A container whose mount
// is older than that one, because kubelet unstaged and re-staged the volume
// under the running pod or because a recovery could not reach it, is never
// on that device, and stays on its dead mount through every later recovery.
//
// A container mount belongs to the staged filesystem when its mount source
// names the same filer path. Only a mount on another device whose statfs
// fails with a dead-connection errno is replaced, so the live mount, another
// volume's mounts and a mount that is merely slow are never touched.
func repairStaleContainerMounts(publishPath, stagingPath string) int {
	podUID := extractPodUID(publishPath)
	if podUID == "" {
		return 0
	}
	staging, err := mountEntryAt(os.Getpid(), stagingPath)
	if err != nil || !isFuseFS(staging.fstype) {
		glog.V(4).Infof("container repair: staging path %s is not a FUSE mount (%v)", stagingPath, err)
		return 0
	}
	pids, err := findContainerPIDsForPod(podUID)
	if err != nil {
		glog.Warningf("container repair: cannot list containers of pod %s (hostPID may not be enabled): %v", podUID, err)
		return 0
	}
	repaired := 0
	for _, pid := range pids {
		entries, err := parseMountInfo(pid)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !isFuseFS(e.fstype) || e.device == staging.device || !sameFuseFilesystem(e.source, staging.source) {
				continue
			}
			checkPath := fmt.Sprintf("/proc/%d/root%s", pid, e.mountpoint)
			statErr, answered := probeStatfsWithin(checkPath, containerMountProbeWait)
			if !answered {
				glog.Warningf("container repair: %s in container PID %d (pod %s) is an older mount of %s that does not answer statfs; leaving it", e.mountpoint, pid, podUID, staging.source)
				continue
			}
			if !isStaleMount(statErr) {
				continue
			}
			readOnly := e.readOnly()
			glog.Warningf("container repair: %s (subPath %q) in container PID %d (pod %s) is on dead mount %s of %s; binding live staging mount %s", e.mountpoint, e.root, pid, podUID, e.device, staging.source, staging.device)
			if err := remountViaSetns(pid, e.mountpoint, stagingPath, e.root, readOnly); err != nil {
				glog.Errorf("container repair: failed to remount %s in PID %d (pod %s): %v", e.mountpoint, pid, podUID, err)
				continue
			}
			resetStatfsProbe(checkPath)
			repaired++
			glog.Infof("container repair: remounted %s in PID %d (pod %s)", e.mountpoint, pid, podUID)
		}
	}
	return repaired
}

// sameFuseFilesystem reports whether two FUSE mount sources name the same
// filesystem. weed mount's source is "<filer addresses>:<filer path>"; the
// filer path identifies the volume even when the filer addresses differ.
func sameFuseFilesystem(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	pa, pb := fuseSourcePath(a), fuseSourcePath(b)
	return pa != "" && pa == pb
}

// fuseSourcePath returns the filer path of a weed mount source, or "".
func fuseSourcePath(source string) string {
	if i := strings.Index(source, ":/"); i >= 0 {
		return source[i+1:]
	}
	return ""
}

// mountEntryAt returns the topmost mount at path in the mount namespace of
// pid.
func mountEntryAt(pid int, path string) (mountInfoEntry, error) {
	entries, err := parseMountInfo(pid)
	if err != nil {
		return mountInfoEntry{}, err
	}
	var top mountInfoEntry
	found := false
	for _, e := range entries {
		if e.mountpoint == path {
			top, found = e, true
		}
	}
	if !found {
		return mountInfoEntry{}, fmt.Errorf("no mount at %s", path)
	}
	return top, nil
}

// extractPodUID extracts the pod UID from a CSI publish path.
// Expected format: .../pods/<uid>/volumes/...
func extractPodUID(publishPath string) string {
	parts := strings.Split(publishPath, string(os.PathSeparator))
	for i, part := range parts {
		if part == "pods" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

// findContainerPIDsForPod returns one PID per unique mount namespace
// for containers belonging to the given pod. It scans /proc/*/cgroup
// for the pod UID. Requires hostPID: true on the CSI driver pod.
func findContainerPIDsForPod(podUID string) ([]int, error) {
	// Kubernetes uses both dash-separated and underscore-separated UIDs
	// in cgroup paths depending on the container runtime.
	normalizedUID := strings.ReplaceAll(podUID, "-", "_")

	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("read /proc: %w", err)
	}

	// Cache our own mount namespace inode to skip processes that share it.
	selfNS, _ := os.Readlink("/proc/self/ns/mnt")

	// Collect one PID per mount namespace to avoid redundant remounts.
	seenNS := make(map[string]bool)
	var pids []int

	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}

		cgroupData, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
		if err != nil {
			continue
		}

		content := string(cgroupData)
		if !strings.Contains(content, podUID) && !strings.Contains(content, normalizedUID) {
			continue
		}

		// Deduplicate by mount namespace inode so we remount once per
		// container, not once per process.
		nsPath := fmt.Sprintf("/proc/%d/ns/mnt", pid)
		nsLink, err := os.Readlink(nsPath)
		if err != nil {
			continue
		}

		// Skip processes that share the CSI driver's own mount namespace.
		if selfNS != "" && nsLink == selfNS {
			continue
		}

		if !seenNS[nsLink] {
			seenNS[nsLink] = true
			pids = append(pids, pid)
		}
	}

	return pids, nil
}

// mountInfoEntry holds a parsed line from /proc/<pid>/mountinfo.
type mountInfoEntry struct {
	device string // "major:minor"
	// root is the pathname of the directory in the filesystem that forms
	// the root of this mount (mountinfo field 4). For a bind created from
	// a volumeMount subPath it is the subPath offset (e.g. "/store"); for
	// a whole-filesystem mount it is "/". Preserving it is what keeps a
	// subPath mount pointing at <staging>/<subPath> after a remount.
	root       string
	mountpoint string
	// options are the per-mount options (mountinfo field 6), e.g. "ro".
	options string
	fstype  string
	// source is the filesystem's mount source, for weed mount
	// "<filer addresses>:<filer path>".
	source string
}

// readOnly reports whether the mount itself is read-only.
func (e mountInfoEntry) readOnly() bool {
	for _, o := range strings.Split(e.options, ",") {
		if o == "ro" {
			return true
		}
	}
	return false
}

// parseMountInfo parses /proc/<pid>/mountinfo and returns entries.
func parseMountInfo(pid int) ([]mountInfoEntry, error) {
	path := fmt.Sprintf("/proc/%d/mountinfo", pid)
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseMountInfoReader(f)
}

// parseMountInfoReader parses mountinfo-formatted content. Split out from
// parseMountInfo so the field parsing (including the subPath-bearing root
// field) can be unit-tested without a live /proc.
func parseMountInfoReader(r io.Reader) ([]mountInfoEntry, error) {
	var entries []mountInfoEntry
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 7 {
			continue
		}
		// Format: id parent major:minor root mountpoint opts [optional...] - fstype source superopts
		// root and mountpoint are octal-escaped by util-linux; decode them
		// before use in filesystem operations.
		device := fields[2]
		root := unescapeMountField(fields[3])
		mountpoint := unescapeMountField(fields[4])

		// Find the "-" separator to get fstype
		sepIdx := -1
		for i := 6; i < len(fields); i++ {
			if fields[i] == "-" {
				sepIdx = i
				break
			}
		}
		var fstype, source string
		if sepIdx >= 0 && sepIdx+1 < len(fields) {
			fstype = fields[sepIdx+1]
		}
		if sepIdx >= 0 && sepIdx+2 < len(fields) {
			source = fields[sepIdx+2]
		}

		entries = append(entries, mountInfoEntry{
			device:     device,
			root:       root,
			mountpoint: mountpoint,
			options:    fields[5],
			fstype:     fstype,
			source:     source,
		})
	}
	return entries, scanner.Err()
}

// unescapeMountField decodes the octal escape sequences util-linux uses
// for the root and mountpoint fields of /proc/<pid>/mountinfo. Space
// (\040), tab (\011), newline (\012) and backslash (\134) are written as
// three-digit octal so each field stays whitespace-free and splittable, so
// any '\' in the field begins such a sequence. Unknown sequences are left
// verbatim. Unlike strconv.Unquote this never errors and never trips over
// an embedded quote, so a path that fails to decode cannot silently
// collapse to "" — which for the root field would drop the subPath offset
// and re-introduce the share-root bind this change exists to prevent.
func unescapeMountField(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) &&
			isOctalDigit(s[i+1]) && isOctalDigit(s[i+2]) && isOctalDigit(s[i+3]) {
			b.WriteByte((s[i+1]-'0')<<6 | (s[i+2]-'0')<<3 | (s[i+3] - '0'))
			i += 3
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isOctalDigit(c byte) bool { return c >= '0' && c <= '7' }

// getMountDevice returns the "major:minor" device string for a mount
// at the given path by parsing /proc/self/mountinfo.
func getMountDevice(mountPath string) (string, error) {
	entries, err := parseMountInfo(os.Getpid())
	if err != nil {
		return "", fmt.Errorf("parse self mountinfo: %w", err)
	}

	// Find the most specific (longest) matching mountpoint, since there
	// may be nested mounts.
	var best mountInfoEntry
	for _, e := range entries {
		if e.mountpoint == mountPath && len(e.mountpoint) >= len(best.mountpoint) {
			best = e
		}
	}
	if best.device == "" {
		return "", fmt.Errorf("no mount entry found for %s", mountPath)
	}
	return best.device, nil
}

// findContainerMountsByDevice finds all mounts inside a container's
// mount namespace whose device matches oldDevice and whose fstype is
// FUSE. A single PVC can appear at multiple mount points in the same
// container (e.g. a plain mount plus one or more subPath mounts), so all
// matches are returned. Each entry carries its root (subPath offset) so
// the remount can re-create the bind from the correct subdirectory.
func findContainerMountsByDevice(pid int, oldDevice string) ([]mountInfoEntry, error) {
	entries, err := parseMountInfo(pid)
	if err != nil {
		return nil, err
	}

	var matches []mountInfoEntry
	for _, e := range entries {
		if e.device == oldDevice && isFuseFS(e.fstype) {
			matches = append(matches, e)
		}
	}
	return matches, nil
}

// isFuseFS returns true if the fstype indicates a FUSE filesystem mount.
// "fusectl" is excluded -- it is the FUSE control filesystem, not a user mount.
func isFuseFS(fstype string) bool {
	return strings.HasPrefix(fstype, "fuse") && fstype != "fusectl"
}

// isStaleMount reports whether err shows the FUSE connection is gone: the
// kernel answers ENOTCONN once the daemon has closed /dev/fuse or the
// connection was aborted, and ECONNABORTED to requests the abort caught in
// flight. EIO is not proof: a live daemon can return it for a failed
// request.
func isStaleMount(err error) bool {
	if err == nil {
		return false
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno == syscall.ENOTCONN || errno == syscall.ECONNABORTED
	}
	return false
}

// remountViaSetns enters a container's mount namespace using setns(2),
// unmounts the stale FUSE mount, and creates a fresh bind mount from
// the recovered staging path. The staging path is accessible from
// inside the container's mount namespace because it resides on the
// host filesystem under the kubelet directory tree.
//
// subPath is the mountinfo root of the stale mount being replaced. When
// the volumeMount used a subPath, the original bind was offset into a
// subdirectory of the FUSE filesystem (e.g. "/store"), so the fresh bind
// must come from <stagingPath>/<subPath> rather than the share root.
// Otherwise a subPath mount silently resolves to the root after recovery.
// "" or "/" mean a whole-filesystem mount and bind stagingPath directly.
// See seaweedfs/seaweedfs-csi-driver#271.
//
// If readOnly is true, a second remount is performed with MS_RDONLY to
// preserve the original read-only semantics of the volume mount.
func remountViaSetns(containerPID int, containerMountPath, stagingPath, subPath string, readOnly bool) error {
	// Pin this goroutine to the current OS thread for the duration of
	// the namespace switch. No other goroutine will be scheduled on
	// this thread, preventing accidental cross-namespace operations.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// Give this thread its own filesystem context (cwd, root, umask).
	// Go threads share their fs_struct by default, and the kernel
	// rejects setns(CLONE_NEWNS) when the calling thread shares its
	// fs_struct with other threads. unshare(CLONE_FS) breaks that
	// sharing so setns can proceed.
	if err := unix.Unshare(unix.CLONE_FS); err != nil {
		return fmt.Errorf("unshare CLONE_FS: %w", err)
	}

	// Re-create the bind from the same subdirectory the stale mount was
	// offset into. For a subPath volumeMount this is <staging>/<subPath>;
	// for a plain mount subPath is "" or "/" and we bind the share root.
	source := stagingPath
	if subPath != "" && subPath != "/" {
		source = filepath.Join(stagingPath, subPath)
	}

	// open_tree the source BEFORE setns so we have an fd we can
	// move_mount into the container's namespace. A path-based bind
	// after setns fails with ENOENT — the staging path lives only in
	// the driver pod's namespace, and /proc/self/fd/N re-walks the
	// original path string in the current namespace. Requires Linux 5.2+.
	sourceFd, err := unix.OpenTree(unix.AT_FDCWD, source,
		uint(unix.OPEN_TREE_CLONE|unix.OPEN_TREE_CLOEXEC|unix.AT_NO_AUTOMOUNT))
	if err != nil {
		return fmt.Errorf("open_tree %s: %w", source, err)
	}
	defer unix.Close(sourceFd)

	// Save our current mount namespace so we can restore it.
	origNS, err := os.Open("/proc/self/ns/mnt")
	if err != nil {
		return fmt.Errorf("open current mount ns: %w", err)
	}
	defer origNS.Close()

	// Open the container's mount namespace.
	containerNSPath := fmt.Sprintf("/proc/%d/ns/mnt", containerPID)
	containerNS, err := os.Open(containerNSPath)
	if err != nil {
		return fmt.Errorf("open container mount ns %s: %w", containerNSPath, err)
	}
	defer containerNS.Close()

	// Enter the container's mount namespace.
	if err := unix.Setns(int(containerNS.Fd()), unix.CLONE_NEWNS); err != nil {
		return fmt.Errorf("setns to container PID %d: %w", containerPID, err)
	}

	// ALWAYS restore our namespace before returning, even on error.
	defer func() {
		if restoreErr := unix.Setns(int(origNS.Fd()), unix.CLONE_NEWNS); restoreErr != nil {
			// This is a critical error -- the goroutine is stuck in the
			// wrong namespace. LockOSThread prevents it from affecting
			// other goroutines, and the deferred UnlockOSThread will
			// retire the thread. Log loudly.
			glog.Errorf("container remount: CRITICAL: failed to restore mount namespace: %v", restoreErr)
		}
	}()

	// Lazy-unmount the stale FUSE mount inside the container.
	// MNT_DETACH ensures this succeeds even if processes have open
	// files, letting them drain while new accesses use the fresh mount.
	if umountErr := unix.Unmount(containerMountPath, unix.MNT_DETACH); umountErr != nil {
		glog.V(4).Infof("container remount: umount %s in PID %d: %v (may already be unmounted)", containerMountPath, containerPID, umountErr)
	}

	if err := unix.MoveMount(sourceFd, "", unix.AT_FDCWD, containerMountPath,
		unix.MOVE_MOUNT_F_EMPTY_PATH); err != nil {
		return fmt.Errorf("move_mount detached source -> %s in container PID %d (source path was %s): %w", containerMountPath, containerPID, source, err)
	}

	// A bind mount created with MS_BIND ignores MS_RDONLY in the same
	// call. To make it read-only a second remount is required.
	if readOnly {
		if err := unix.Mount("", containerMountPath, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, ""); err != nil {
			glog.Warningf("container remount: failed to set read-only on %s in PID %d: %v", containerMountPath, containerPID, err)
		}
	}

	return nil
}
