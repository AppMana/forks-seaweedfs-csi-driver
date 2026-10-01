package driver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
)

// Uses the real platform cleanup on an unmounted nonempty directory; no
// privileges, mount daemon, Kubernetes API, or VM are required.
func TestFailedUnpublishMustNotRepublishNonemptyTarget(t *testing.T) {
	ns := &NodeServer{Driver: &SeaweedFsDriver{}, volumeMutexes: NewKeyMutex()}
	target := t.TempDir()
	file := filepath.Join(target, "intact-local-data")
	if err := os.WriteFile(file, []byte("keep me"), 0600); err != nil {
		t.Fatal(err)
	}
	vol := &Volume{VolumeId: "test-volume", StagedPath: t.TempDir()}
	vol.bindMountFn = func(string, string, bool) error {
		return errors.New("unexpected republish intercepted; no real mount in this test")
	}
	vol.AddPublishPath(target, false)
	ns.volumes.Store(vol.VolumeId, vol)
	ns.isHealthyFn = func(path string) bool { return path == vol.StagedPath }
	unmounts := 0
	ns.unmountFn = func(string) error { unmounts++; return nil }
	_, err := ns.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId: vol.VolumeId, TargetPath: target,
	})
	if err == nil {
		t.Fatal("nonempty target cleanup must report failure, not delete its contents")
	}
	if got, readErr := os.ReadFile(file); readErr != nil || string(got) != "keep me" {
		t.Fatalf("intact data changed: %q, %v", got, readErr)
	}
	if _, tracked := vol.publishPaths.Load(target); tracked {
		t.Error("unpublish intent must cancel recovery even when directory cleanup fails")
	}
	ns.retryPublishPaths(vol.VolumeId)
	if unmounts != 0 {
		t.Errorf("recovery tried to republish a target kubelet requested to remove: %d", unmounts)
	}
}
