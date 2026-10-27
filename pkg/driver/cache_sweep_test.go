package driver

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func seedCacheFile(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "c1_3_0.dat")
	if err := os.WriteFile(marker, []byte("chunk"), 0o600); err != nil {
		t.Fatal(err)
	}
	return marker
}

// Caches are orphaned after startup too: an unstage whose cleanup failed
// (Windows refuses to delete files weed.exe still holds while it exits)
// leaves the volume's cache behind. The node plugin sweeps them while it
// runs, never the shared chunk cache.
func TestNodeServerSweepsOrphanCachesWhileRunning(t *testing.T) {
	previous := cacheSweepInterval
	cacheSweepInterval = 20 * time.Millisecond
	defer func() { cacheSweepInterval = previous }()

	cacheBase := t.TempDir()
	ns := NewNodeServer(&SeaweedFsDriver{
		name:            "seaweedfs-csi-driver",
		CacheDir:        cacheBase,
		volumeSocketDir: t.TempDir(),
	})
	defer ns.NodeCleanup()

	orphan := GetCacheDir(cacheBase, "/buckets/pvc-unstaged")
	seedCacheFile(t, orphan)
	sharedMarker := seedCacheFile(t, filepath.Join(SharedChunkCacheDir(cacheBase), "00"))

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(orphan); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("orphaned cache %s was not swept while the node plugin ran", orphan)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(sharedMarker); err != nil {
		t.Fatalf("sweep removed the shared chunk cache: %v", err)
	}
}

// kubelet keeps a volume's staging directory, named by the same hash as its
// cache, until NodeUnstageVolume succeeds. While the mount pod rolls, every
// mount's socket is dead for about a minute and a volume the plugin could
// not rediscover is not in its staged set: the staging directory alone keeps
// the cache.
func TestCacheSweepKeepsCacheWhileKubeletHasTheVolumeStaged(t *testing.T) {
	cacheBase := t.TempDir()
	scanDir := t.TempDir()
	driverName := "seaweedfs-csi-driver"
	volumeID := "/buckets/pvc-staged-not-rediscovered"
	marker := seedCacheFile(t, GetCacheDir(cacheBase, volumeID))
	stagingDir := filepath.Join(scanDir, driverName, filepath.Base(GetCacheDir(cacheBase, volumeID)), "globalmount")
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ns := NewNodeServer(&SeaweedFsDriver{
		name:            driverName,
		CacheDir:        cacheBase,
		volumeSocketDir: t.TempDir(),
		StagingScanDir:  scanDir,
	})
	defer ns.NodeCleanup()
	ns.removeOrphanCacheDirs()
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("sweep removed the cache of a volume kubelet has staged: %v", err)
	}
}
