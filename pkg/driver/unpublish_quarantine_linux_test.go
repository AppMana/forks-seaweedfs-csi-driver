package driver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
)

// stalePublishTarget builds <root>/pods/<uid>/volumes/kubernetes.io~csi/<pvc>/mount
// holding a file, as kubelet leaves it when something wrote there while the
// volume was not mounted.
func stalePublishTarget(t *testing.T, root string) (target, file string) {
	t.Helper()
	target = filepath.Join(root, "pods", "7f81d1a2-e3ba-4acf-b609-ac62dee863af", "volumes", "kubernetes.io~csi", "pvc-7df5aca8", "mount")
	if err := os.MkdirAll(filepath.Join(target, "lbxx"), 0o750); err != nil {
		t.Fatal(err)
	}
	file = filepath.Join(target, "lbxx", "cache.bin")
	if err := os.WriteFile(file, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	return target, file
}

func quarantinedCopy(t *testing.T, root string) string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, quarantineDirName))
	if err != nil {
		t.Fatalf("no quarantine directory: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("quarantine holds %d entries, want 1", len(entries))
	}
	name := entries[0].Name()
	if !strings.Contains(name, "7f81d1a2-e3ba-4acf-b609-ac62dee863af") || !strings.Contains(name, "pvc-7df5aca8") {
		t.Fatalf("quarantine entry %q does not name the pod and volume", name)
	}
	got, err := os.ReadFile(filepath.Join(root, quarantineDirName, name, "lbxx", "cache.bin"))
	if err != nil || string(got) != "keep me" {
		t.Fatalf("quarantined data changed: %q, %v", got, err)
	}
	return name
}

// A nonempty, unmounted publish target fails kubelet's unpublish forever and
// keeps its pod terminating. With a kubelet root configured, the driver moves
// the target aside intact and reports success.
func TestUnpublishQuarantinesNonemptyUnmountedTarget(t *testing.T) {
	for _, published := range []bool{true, false} {
		t.Run(map[bool]string{true: "published", false: "unknown volume"}[published], func(t *testing.T) {
			root := t.TempDir()
			target, file := stalePublishTarget(t, root)
			ns := &NodeServer{Driver: &SeaweedFsDriver{KubeletRootDir: root, KubeletRootMount: root}, volumeMutexes: NewKeyMutex()}
			if published {
				vol := &Volume{VolumeId: "test-volume", StagedPath: t.TempDir()}
				vol.AddPublishPath(target, false)
				ns.volumes.Store(vol.VolumeId, vol)
			}
			_, err := ns.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
				VolumeId: "test-volume", TargetPath: target,
			})
			if err != nil {
				t.Fatalf("unpublish: %v", err)
			}
			if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
				t.Fatalf("target still present: %v", statErr)
			}
			if _, statErr := os.Stat(file); !os.IsNotExist(statErr) {
				t.Fatalf("file still at target: %v", statErr)
			}
			quarantinedCopy(t, root)
		})
	}
}

// The kubelet root is reached through its own mount in the container; target
// paths are host paths and must be translated before the rename.
func TestUnpublishQuarantineTranslatesHostPathsToTheRootMount(t *testing.T) {
	mounted := t.TempDir()
	target, _ := stalePublishTarget(t, mounted)
	hostRoot := "/var/lib/k0s/kubelet"
	hostTarget := hostRoot + strings.TrimPrefix(target, mounted)
	ns := &NodeServer{Driver: &SeaweedFsDriver{KubeletRootDir: hostRoot, KubeletRootMount: mounted}, volumeMutexes: NewKeyMutex()}
	if got := ns.quarantineStaleTarget(hostTarget, target); got == "" {
		t.Fatal("stale target was not quarantined")
	}
	quarantinedCopy(t, mounted)
}

// Paths outside the kubelet root, and drivers without one configured, keep
// today's contract: the error is reported and nothing moves.
func TestUnpublishQuarantineLeavesOtherTargetsAlone(t *testing.T) {
	root := t.TempDir()
	target, file := stalePublishTarget(t, root)
	for name, drv := range map[string]*SeaweedFsDriver{
		"not configured":   {},
		"outside the root": {KubeletRootDir: t.TempDir(), KubeletRootMount: t.TempDir()},
	} {
		t.Run(name, func(t *testing.T) {
			ns := &NodeServer{Driver: drv, volumeMutexes: NewKeyMutex()}
			vol := &Volume{VolumeId: "test-volume", StagedPath: t.TempDir()}
			vol.AddPublishPath(target, false)
			ns.volumes.Store(vol.VolumeId, vol)
			if _, err := ns.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
				VolumeId: "test-volume", TargetPath: target,
			}); err == nil {
				t.Fatal("nonempty target cleanup must still report failure")
			}
			if got, err := os.ReadFile(file); err != nil || string(got) != "keep me" {
				t.Fatalf("data changed: %q, %v", got, err)
			}
		})
	}
}
