//go:build linux

package driver

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRecoverStagedVolumesFromDiskRestoresPublishPaths(t *testing.T) {
	root := t.TempDir()
	scanDir := filepath.Join(root, "plugins", "kubernetes.io", "csi")
	driverName := "seaweedfs-csi-driver"
	volumeName := "pvc-7df5aca8-4b83-4092-8b84-c1e7b536232b"
	volumeID := "/buckets/" + volumeName

	stagingDir := filepath.Join(scanDir, driverName, "staging-hash")
	if err := os.MkdirAll(filepath.Join(stagingDir, "globalmount"), 0o755); err != nil {
		t.Fatal(err)
	}
	volData := []byte(`{"driverName":"seaweedfs-csi-driver","volumeHandle":"` + volumeID + `"}`)
	if err := os.WriteFile(filepath.Join(stagingDir, "vol_data.json"), volData, 0o600); err != nil {
		t.Fatal(err)
	}

	publishPath := filepath.Join(root, "pods", "pod-uid", "volumes", "kubernetes.io~csi", volumeName, "mount")
	if err := os.MkdirAll(publishPath, 0o755); err != nil {
		t.Fatal(err)
	}

	ns := newTestNodeServer(t, &fakeMounter{})
	ns.Driver.name = driverName
	ns.recoverStagedVolumesFromDisk(scanDir)

	value, ok := ns.volumes.Load(volumeID)
	if !ok {
		t.Fatalf("volume %q was not recovered", volumeID)
	}
	vol := value.(*Volume)
	if _, ok := vol.publishPaths.Load(publishPath); !ok {
		t.Fatalf("publish path %q was not recovered", publishPath)
	}
}
