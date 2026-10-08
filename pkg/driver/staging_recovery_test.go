//go:build linux

package driver

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Disk recovery restores tracking, not a guarantee that the mount survived a
// reboot. Exercise the public RPCs against that exact recovered state, without
// a mount service, Kubernetes cluster, or Windows VM.
func TestRecoveredStageMustBeHealthyBeforeRPCSuccess(t *testing.T) {
	for _, operation := range []string{"stage", "publish"} {
		for _, health := range []string{"healthy", "dead", "slow"} {
			t.Run(operation+"/"+health, func(t *testing.T) {
				ns := newTestNodeServer(t, &fakeMounter{})
				ns.Driver.name = "seaweedfs-csi-driver"
				scanDir := filepath.Join(t.TempDir(), "plugins", "kubernetes.io", "csi")
				volDir := filepath.Join(scanDir, ns.Driver.name, "hash")
				if err := os.MkdirAll(volDir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(volDir, "vol_data.json"), []byte(`{"driverName":"seaweedfs-csi-driver","volumeHandle":"/buckets/pvc-test"}`), 0o600); err != nil {
					t.Fatal(err)
				}
				ns.isHealthyFn = func(string) bool { return false }
				ns.recoverStagedVolumesFromDisk(scanDir)
				ns.healthCheckTimeout = 5 * time.Millisecond
				gate := make(chan struct{})
				defer close(gate)
				ns.isHealthyFn = func(string) bool {
					if health == "slow" {
						<-gate
					}
					return health == "healthy"
				}
				binds := 0
				value, _ := ns.volumes.Load("/buckets/pvc-test")
				vol := value.(*Volume)
				vol.bindMountFn = func(string, string, bool) error { binds++; return nil }
				ns.cleanupStagingFn = func(string) error { t.Error("RPC tore down staging"); return nil }
				capability := &csi.VolumeCapability{AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}}, AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER}}
				var err error
				if operation == "stage" {
					_, err = ns.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{VolumeId: vol.VolumeId, StagingTargetPath: vol.StagedPath, VolumeCapability: capability})
				} else {
					_, err = ns.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{VolumeId: vol.VolumeId, StagingTargetPath: vol.StagedPath, TargetPath: filepath.Join(t.TempDir(), "publish"), VolumeCapability: capability})
				}
				want := codes.Unavailable
				if health == "healthy" {
					want = codes.OK
				}
				if status.Code(err) != want {
					t.Fatalf("RPC error=%v, want %v", err, want)
				}
				if health != "healthy" && binds != 0 {
					t.Fatalf("bound unreadable staging %d times", binds)
				}
				if operation == "publish" && health == "healthy" && binds != 1 {
					t.Fatalf("healthy publish binds=%d, want1", binds)
				}
			})
		}
	}
}

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

func TestRecoverStagedVolumesFromDiskRestoresMissingPublishMount(t *testing.T) {
	root := t.TempDir()
	scanDir := filepath.Join(root, "plugins", "kubernetes.io", "csi")
	driverName := "seaweedfs-csi-driver"
	volumeName := "pvc-7df5aca8-4b83-4092-8b84-c1e7b536232b"
	volumeID := "/buckets/" + volumeName

	stagingDir := filepath.Join(scanDir, driverName, "staging-hash")
	if err := os.MkdirAll(filepath.Join(stagingDir, "globalmount"), 0o755); err != nil {
		t.Fatal(err)
	}
	stagingData := []byte(`{"driverName":"seaweedfs-csi-driver","volumeHandle":"` + volumeID + `"}`)
	if err := os.WriteFile(filepath.Join(stagingDir, "vol_data.json"), stagingData, 0o600); err != nil {
		t.Fatal(err)
	}

	volumeDir := filepath.Join(root, "pods", "pod-uid", "volumes", "kubernetes.io~csi", volumeName)
	if err := os.MkdirAll(volumeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	podData := []byte(`{"driverName":"seaweedfs-csi-driver","volumeHandle":"` + volumeID + `"}`)
	if err := os.WriteFile(filepath.Join(volumeDir, "vol_data.json"), podData, 0o600); err != nil {
		t.Fatal(err)
	}
	publishPath := filepath.Join(volumeDir, "mount")

	ns := newTestNodeServer(t, &fakeMounter{})
	ns.Driver.name = driverName
	ns.recoverStagedVolumesFromDisk(scanDir)

	value, ok := ns.volumes.Load(volumeID)
	if !ok {
		t.Fatalf("volume %q was not recovered", volumeID)
	}
	if _, ok := value.(*Volume).publishPaths.Load(publishPath); !ok {
		t.Fatalf("missing publish path %q was not recovered from pod vol_data.json", publishPath)
	}
}

// Kubelet names a pod's publish directory after the PersistentVolume, which
// for a statically provisioned volume need not match the volume handle. On
// appmana-031 (2026-10-08) PVs hf-cache-shared-appmana and
// hf-cache-spellsource-appmana (handles /buckets/pvc-a0c4794b-..., /buckets/
// pvc-fb1f1647-...) were found with "0 publish paths" after a node-plugin
// restart, so a supervisor restart left three pods on dead FUSE binds
// (ENOTCONN) while the staging mounts recovered.
func TestRecoverStagedVolumesFromDiskFindsPublishPathsOfStaticPVs(t *testing.T) {
	root := t.TempDir()
	scanDir := filepath.Join(root, "plugins", "kubernetes.io", "csi")
	driverName := "seaweedfs-csi-driver"
	volumeID := "/buckets/pvc-a0c4794b-c512-4459-86c6-37fa291d3a57"
	otherID := "/buckets/pvc-fb1f1647-63e0-4ad7-bda0-0109a8f0e744"

	stagingDir := filepath.Join(scanDir, driverName, "staging-hash")
	if err := os.MkdirAll(filepath.Join(stagingDir, "globalmount"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stagingDir, "vol_data.json"), []byte(`{"driverName":"seaweedfs-csi-driver","volumeHandle":"`+volumeID+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	publish := func(pod, pvName, handle string) string {
		volumeDir := filepath.Join(root, "pods", pod, "volumes", "kubernetes.io~csi", pvName)
		if err := os.MkdirAll(filepath.Join(volumeDir, "mount"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(volumeDir, "vol_data.json"), []byte(`{"driverName":"seaweedfs-csi-driver","volumeHandle":"`+handle+`","specVolID":"`+pvName+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		return filepath.Join(volumeDir, "mount")
	}
	mine := publish("pod-a", "hf-cache-shared-appmana", volumeID)
	other := publish("pod-a", "hf-cache-spellsource-appmana", otherID)

	ns := newTestNodeServer(t, &fakeMounter{})
	ns.Driver.name = driverName
	ns.recoverStagedVolumesFromDisk(scanDir)

	value, ok := ns.volumes.Load(volumeID)
	if !ok {
		t.Fatalf("volume %q was not recovered", volumeID)
	}
	vol := value.(*Volume)
	if _, ok := vol.publishPaths.Load(mine); !ok {
		t.Fatalf("publish path %q of a static PV was not recovered", mine)
	}
	if _, ok := vol.publishPaths.Load(other); ok {
		t.Fatalf("publish path %q of another volume was attributed to %s", other, volumeID)
	}
}
