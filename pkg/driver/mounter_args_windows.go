//go:build windows

package driver

import (
	"context"

	"github.com/seaweedfs/seaweedfs-csi-driver/pkg/k8s"
	"github.com/seaweedfs/seaweedfs-csi-driver/pkg/mountmanager"
)

// platformMountArgs returns weed mount arguments that only apply on this
// platform. Windows (WinFsp) has no POSIX umask semantics; the mount reads
// where pods mount the volume from the file publishes record (mount_roots.go).
func platformMountArgs(volumeSocketDir, volumeID string) []string {
	return []string{"-winfspMountRoots=" + mountmanager.MountRootsPath(volumeSocketDir, volumeID)}
}

const mountRootsRecorded = true

func platformPodVolumeMounts(driverName string) PodVolumeMountsFn {
	return func(ctx context.Context, volumeID, namespace, name string) ([][]k8s.VolumeMount, error) {
		return k8s.GetPodVolumeMounts(ctx, driverName, volumeID, namespace, name)
	}
}
