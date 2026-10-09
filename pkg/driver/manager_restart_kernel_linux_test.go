//go:build linux

package driver

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// Run only inside a disposable private mount namespace with CAP_SYS_ADMIN.
// Uses a real kernel mount to exercise the safety guard, without FUSE, a
// Kubernetes cluster, network access, or production data. The mount manager's
// lost state and dead-health result are modeled; kernel attachment is real.
func TestManagerRestartKernelMountRecovery(t *testing.T) {
	if os.Getenv("CSI_ISOLATED_KERNEL_MOUNT_TEST") != "1" {
		t.Skip("requires explicitly isolated mount namespace and CAP_SYS_ADMIN")
	}
	state := newFakeMountState()
	ns := newNodeServerWithFakes(t, state)
	path := filepath.Join(t.TempDir(), "staging")
	vol, err := ns.stageNewVolume("kernel-forgotten", path, map[string]string{}, false)
	if err != nil {
		t.Fatal(err)
	}
	vol.volContext = map[string]string{}
	ns.volumes.Store("kernel-forgotten", vol)
	if err := unix.Mount("tmpfs", path, "tmpfs", 0, "size=1m"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Unmount(path, unix.MNT_DETACH) })
	if mounted, err := isPathMounted(path); err != nil || !mounted {
		t.Fatalf("kernel mount precondition: mounted=%v err=%v", mounted, err)
	}
	state.healthy.Store(false)
	// tmpfs answers statfs; a dead FUSE daemon fails it with ENOTCONN.
	origStatfs := statfsFn
	t.Cleanup(func() { statfsFn = origStatfs })
	statfsFn = func(p string) error {
		if p == path {
			return unix.ENOTCONN
		}
		return origStatfs(p)
	}
	ns.detachStagingFn = detachDeadMountPoint
	ns.cleanupStagingFn = func(path string) error { return os.Remove(path) }
	ns.recoverVolume("kernel-forgotten")
	if mounted, err := isPathMounted(path); err != nil || mounted {
		t.Fatalf("forgotten kernel mount remains: mounted=%v err=%v", mounted, err)
	}
	if state.stageCalls != 2 {
		t.Fatalf("expected re-stage, got %d stage calls", state.stageCalls)
	}
}
