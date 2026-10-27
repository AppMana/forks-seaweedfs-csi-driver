//go:build linux

package driver

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
)

// blockingMounter writes the volume's cache, as weed does when it starts,
// then waits until released before reporting the mount ready.
type blockingMounter struct {
	cacheDir string
	marker   string
	created  chan struct{}
	release  chan struct{}
}

func (m *blockingMounter) Mount(target string) (Unmounter, error) {
	if err := os.MkdirAll(m.cacheDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(m.marker, []byte("chunk"), 0o600); err != nil {
		return nil, err
	}
	close(m.created)
	<-m.release
	return &fakeUnmounter{parent: &fakeMounter{}}, nil
}

// A volume being staged has a cache before it is recorded as staged and
// before its mount answers on its local socket. A sweep during that window
// must not take the cache from under the starting mount.
func TestCacheSweepKeepsCacheOfVolumeBeingStaged(t *testing.T) {
	cacheBase := t.TempDir()
	volumeID := "/buckets/pvc-staging"
	cacheDir := GetCacheDir(cacheBase, volumeID)
	mounter := &blockingMounter{
		cacheDir: cacheDir,
		marker:   filepath.Join(cacheDir, "c1_3_0.dat"),
		created:  make(chan struct{}),
		release:  make(chan struct{}),
	}
	ns := newTestNodeServer(t, nil)
	ns.Driver.CacheDir = cacheBase
	ns.Driver.volumeSocketDir = t.TempDir()
	ns.vacLoader = func(context.Context, string) (map[string]string, error) { return nil, nil }
	ns.mounterFactory = func(string, bool, *SeaweedFsDriver, map[string]string) (Mounter, error) {
		return mounter, nil
	}

	staged := make(chan error, 1)
	go func() {
		_, err := ns.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
			VolumeId:          volumeID,
			StagingTargetPath: filepath.Join(t.TempDir(), "globalmount"),
			VolumeCapability: &csi.VolumeCapability{
				AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
				AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER},
			},
		})
		staged <- err
	}()
	<-mounter.created

	swept := make(chan struct{})
	go func() {
		ns.removeOrphanCacheDirs()
		close(swept)
	}()
	select {
	case <-swept:
	case <-time.After(200 * time.Millisecond):
	}
	close(mounter.release)
	if err := <-staged; err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}
	<-swept
	if _, err := os.Stat(mounter.marker); err != nil {
		t.Fatalf("sweep removed the cache of a volume being staged: %v", err)
	}
}

// A sweep waiting for a slow stage must not hold up other stages on the
// node: it gives up for this round instead of queueing for the lock.
func TestPendingCacheSweepDoesNotDelayOtherStages(t *testing.T) {
	cacheBase := t.TempDir()
	slowID, fastID := "/buckets/pvc-slow", "/buckets/pvc-fast"
	slowCache := GetCacheDir(cacheBase, slowID)
	slow := &blockingMounter{
		cacheDir: slowCache,
		marker:   filepath.Join(slowCache, "c1_3_0.dat"),
		created:  make(chan struct{}),
		release:  make(chan struct{}),
	}
	defer func() {
		select {
		case <-slow.release:
		default:
			close(slow.release)
		}
	}()
	ns := newTestNodeServer(t, nil)
	ns.Driver.CacheDir = cacheBase
	ns.Driver.volumeSocketDir = t.TempDir()
	ns.vacLoader = func(context.Context, string) (map[string]string, error) { return nil, nil }
	ns.mounterFactory = func(volumeID string, _ bool, _ *SeaweedFsDriver, _ map[string]string) (Mounter, error) {
		if volumeID == slowID {
			return slow, nil
		}
		return &fakeMounter{}, nil
	}
	stage := func(volumeID string) error {
		_, err := ns.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
			VolumeId:          volumeID,
			StagingTargetPath: filepath.Join(t.TempDir(), "globalmount"),
			VolumeCapability: &csi.VolumeCapability{
				AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
				AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER},
			},
		})
		return err
	}

	slowDone := make(chan error, 1)
	go func() { slowDone <- stage(slowID) }()
	<-slow.created

	swept := make(chan struct{})
	go func() {
		ns.runCacheSweep()
		close(swept)
	}()
	time.Sleep(50 * time.Millisecond)

	fastDone := make(chan error, 1)
	go func() { fastDone <- stage(fastID) }()
	select {
	case err := <-fastDone:
		if err != nil {
			t.Fatalf("stage of %s: %v", fastID, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a pending cache sweep held up an unrelated stage")
	}

	close(slow.release)
	if err := <-slowDone; err != nil {
		t.Fatalf("stage of %s: %v", slowID, err)
	}
	<-swept
	if _, err := os.Stat(slow.marker); err != nil {
		t.Fatalf("sweep removed the cache of a volume being staged: %v", err)
	}
}
