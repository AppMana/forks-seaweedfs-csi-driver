//go:build linux

package driver

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/seaweedfs/seaweedfs-csi-driver/pkg/mountmanager"
)

// The mount service keeps weed mounts running across node-plugin restarts, and
// each mount writes its chunk cache, metadata store and swap files under
// CacheDir. Starting the node server must leave those directories in place:
// a mount whose cache disappears underneath it fails its next cache reset and
// stops answering FUSE requests.
func TestNodeServerStartupKeepsCachesOfLiveMounts(t *testing.T) {
	cacheBase := t.TempDir()
	socketDir := t.TempDir()
	scanDir := filepath.Join(t.TempDir(), "plugins", "kubernetes.io", "csi")
	driverName := "seaweedfs-csi-driver"

	seedCache := func(volumeID string) string {
		t.Helper()
		dir := GetCacheDir(cacheBase, volumeID)
		if err := os.MkdirAll(filepath.Join(dir, "c1"), 0o755); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(dir, "c1", "c1_3_0.dat")
		if err := os.WriteFile(marker, []byte("chunk"), 0o600); err != nil {
			t.Fatal(err)
		}
		return marker
	}

	// A mount the mount service is serving: its weed process listens on the
	// volume's local socket.
	served := "/buckets/pvc-served"
	servedMarker := seedCache(served)
	listener, err := net.Listen("unix", mountmanager.LocalSocketPath(socketDir, served))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	// A volume kubelet still has staged on this node.
	staged := "/buckets/pvc-staged"
	stagedMarker := seedCache(staged)
	volDir := filepath.Join(scanDir, driverName, "staged-hash")
	if err := os.MkdirAll(volDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(volDir, "vol_data.json"), []byte(`{"driverName":"seaweedfs-csi-driver","volumeHandle":"/buckets/pvc-staged"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// A cache left behind by a volume that is neither served nor staged, and
	// one whose socket file outlived its process.
	orphan := "/buckets/pvc-orphan"
	orphanMarker := seedCache(orphan)
	stale := "/buckets/pvc-stale-socket"
	staleMarker := seedCache(stale)
	if err := os.WriteFile(mountmanager.LocalSocketPath(socketDir, stale), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	driver := &SeaweedFsDriver{
		name:            driverName,
		CacheDir:        cacheBase,
		volumeSocketDir: socketDir,
		StagingScanDir:  scanDir,
	}
	ns := NewNodeServer(driver)
	defer ns.NodeCleanup()

	for name, marker := range map[string]string{"served": servedMarker, "staged": stagedMarker} {
		if _, err := os.Stat(marker); err != nil {
			t.Errorf("%s volume lost its cache at node-server startup: %v", name, err)
		}
	}
	for name, marker := range map[string]string{"orphan": orphanMarker, "stale socket": staleMarker} {
		if _, err := os.Stat(filepath.Dir(filepath.Dir(marker))); !os.IsNotExist(err) {
			t.Errorf("%s cache dir was kept at node-server startup (stat err %v)", name, err)
		}
	}
}
