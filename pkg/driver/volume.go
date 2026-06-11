package driver

import (
	"context"
	"net"
	"os"
	"sync"

	"github.com/seaweedfs/seaweedfs-csi-driver/pkg/mountmanager"
	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/pb/mount_pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type Volume struct {
	VolumeId   string
	StagedPath string

	mounter   Mounter
	unmounter Unmounter

	// unix socket used to manage volume
	localSocket string
	driver      *SeaweedFsDriver

	// Fields for health monitor recovery
	publishPaths sync.Map          // targetPath (string) -> bool (readOnly)
	volContext   map[string]string // volume context stored for re-staging
	readOnly     bool              // FUSE-level readOnly flag

	// bindMountFn is used by Publish to perform the bind mount from the
	// staging path to the pod-specific target path. Populated by the
	// NodeServer that owns this volume; tests override it with a fake.
	bindMountFn BindMountFn
}

func NewVolume(volumeID string, mounter Mounter, driver *SeaweedFsDriver) *Volume {
	return &Volume{
		VolumeId:    volumeID,
		mounter:     mounter,
		localSocket: mountmanager.LocalSocketPath(driver.volumeSocketDir, volumeID),
		driver:      driver,
	}
}

func (vol *Volume) Stage(stagingTargetPath string) error {
	// check whether it can be mounted
	if isMnt, err := checkMount(stagingTargetPath); err != nil {
		return err
	} else if isMnt {
		// try to unmount before mounting again
		_ = unmountVolume(stagingTargetPath)
	}

	if u, err := vol.mounter.Mount(stagingTargetPath); err == nil {
		if vol.StagedPath != "" {
			if vol.StagedPath == stagingTargetPath {
				glog.Warningf("staged path is already set to %s for volume %s", vol.StagedPath, vol.VolumeId)
			} else {
				glog.Warningf("staged path is already set to %s and differs from %s for volume %s", vol.StagedPath, stagingTargetPath, vol.VolumeId)
			}
		}

		vol.StagedPath = stagingTargetPath
		vol.unmounter = u

		return nil
	} else {
		return err
	}
}

func (vol *Volume) Publish(stagingTargetPath string, targetPath string, readOnly bool) error {
	// check whether it can be mounted
	if isMnt, err := checkMount(targetPath); err != nil {
		return err
	} else if isMnt {
		// maybe already mounted?
		return nil
	}

	bind := vol.bindMountFn
	if bind == nil {
		bind = defaultBindMount
	}
	return bind(stagingTargetPath, targetPath, readOnly)
}

func (vol *Volume) Quota(sizeByte int64) error {
	target := "passthrough:///" + vol.localSocket

	clientConn, err := grpc.NewClient(target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", addr)
		}),
	)
	if err != nil {
		return err
	}
	defer clientConn.Close()

	// We can't create PV of zero size, so we're using quota of 1 byte to define no quota.
	if sizeByte == 1 {
		sizeByte = 0
	}

	client := mount_pb.NewSeaweedMountClient(clientConn)
	_, err = client.Configure(context.Background(), &mount_pb.ConfigureRequest{
		CollectionCapacity: sizeByte,
	})
	return err
}

func (vol *Volume) Unpublish(targetPath string) error {
	// Try unmounting target path and deleting it.
	if err := cleanupMountPoint(targetPath); err != nil {
		return err
	}

	return nil
}

func (vol *Volume) AddPublishPath(path string, readOnly bool) {
	vol.publishPaths.Store(path, readOnly)
}

func (vol *Volume) RemovePublishPath(path string) {
	vol.publishPaths.Delete(path)
}

func (vol *Volume) Unstage(stagingTargetPath string) error {
	glog.V(0).Infof("unmounting volume %s from %s", vol.VolumeId, stagingTargetPath)

	if stagingTargetPath != vol.StagedPath && vol.StagedPath != "" {
		glog.Warningf("staging path %s differs for volume %s at %s", stagingTargetPath, vol.VolumeId, vol.StagedPath)
	}

	if vol.unmounter == nil {
		// This can happen when the volume was rebuilt from an existing staging path
		// after a CSI driver restart. In this case, we need to force unmount.
		glog.Infof("volume %s has no unmounter (rebuilt from existing mount), using force unmount", vol.VolumeId)

		// Clean up using mount utilities. This will also handle unmounting.
		if err := cleanupMountPoint(stagingTargetPath); err != nil {
			glog.Errorf("error cleaning up mount point for volume %s: %v", vol.VolumeId, err)
			return err
		}
	} else {
		if err := vol.unmounter.Unmount(); err != nil {
			glog.Errorf("error unmounting volume during unstage: %s, err: %v", stagingTargetPath, err)
			return err
		}

		if err := os.Remove(stagingTargetPath); err != nil && !os.IsNotExist(err) {
			glog.Errorf("error removing staging path for volume %s at %s, err: %v", vol.VolumeId, stagingTargetPath, err)
			return err
		}
	}

	// Always attempt to remove the cache directory and socket file
	CleanupVolumeResources(vol.driver, vol.VolumeId)

	return nil
}
