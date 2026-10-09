package driver

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/seaweedfs/seaweedfs-csi-driver/pkg/datalocality"
	"github.com/seaweedfs/seaweedfs-csi-driver/pkg/k8s"
	"github.com/seaweedfs/seaweedfs-csi-driver/pkg/mountmanager"
	"github.com/seaweedfs/seaweedfs/weed/glog"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
)

func NewNodeServer(n *SeaweedFsDriver) *NodeServer {
	ns := &NodeServer{
		Driver:         n,
		volumeMutexes:  NewKeyMutex(),
		stopCh:         make(chan struct{}),
		mounterFactory: newMounter,
		capacityFn: func(volumeID string) (int64, error) {
			return k8s.GetVolumeCapacity(n.name, volumeID)
		},
		isHealthyFn:      isStagingPathHealthy,
		cleanupStagingFn: cleanupStaleStagingPath,
		detachStagingFn:  detachDeadMountPoint,
		unmountFn:        unmountVolume,
		bindMountFn:      defaultBindMount,
	}
	ns.recoverStagedVolumesFromDisk(n.StagingScanDir)
	ns.removeOrphanCacheDirs()
	ns.startHealthMonitor(defaultHealthCheckInterval)
	return ns
}

// removeOrphanCacheDirs deletes cache directories that no mount can still be
// using. A directory is kept when its volume is staged on this node or when a
// weed mount process accepts connections on the volume's local socket.
func (ns *NodeServer) removeOrphanCacheDirs() {
	if ns.Driver.CacheDir == "" {
		return
	}
	cacheBase := filepath.Clean(ns.Driver.CacheDir)
	if cacheBase == filepath.Clean(os.TempDir()) {
		return
	}
	keep := map[string]struct{}{}
	ns.volumes.Range(func(key, _ any) bool {
		keep[filepath.Base(GetCacheDir(cacheBase, key.(string)))] = struct{}{}
		return true
	})
	entries, err := os.ReadDir(cacheBase)
	if err != nil {
		if !os.IsNotExist(err) {
			glog.Warningf("cannot read cache dir %s: %v", cacheBase, err)
		}
		return
	}
	for _, e := range entries {
		name := e.Name()
		if _, staged := keep[name]; staged {
			continue
		}
		if isCacheOfServedMount(ns.Driver.volumeSocketDir, name) {
			glog.Infof("keeping cache dir %s of a mount the mount service is serving", name)
			continue
		}
		if err := os.RemoveAll(filepath.Join(cacheBase, name)); err != nil {
			glog.Warningf("error removing orphan cache dir %s: %v", name, err)
		}
	}
}

// isCacheOfServedMount reports whether a weed mount accepts connections on
// the local socket of the volume whose cache directory is cacheDirName.
func isCacheOfServedMount(socketDir, cacheDirName string) bool {
	if len(cacheDirName) != sha256.Size*2 {
		return false
	}
	if _, err := hex.DecodeString(cacheDirName); err != nil {
		return false
	}
	socket := mountmanager.LocalSocketPathForHash(socketDir, cacheDirName)
	conn, err := net.DialTimeout("unix", socket, time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func GetCacheDir(cacheBase, volumeID string) string {
	if cacheBase == "" {
		cacheBase = os.TempDir()
	}
	// volumeIDs are full paths in seaweedfs
	// Use hash value instead to get flat cache dir structure
	h := sha256.Sum256([]byte(volumeID))
	hashStr := hex.EncodeToString(h[:])
	return filepath.Join(cacheBase, hashStr)
}

func GetLocalSocket(volumeSocketDir, volumeID string) string {
	return mountmanager.LocalSocketPath(volumeSocketDir, volumeID)
}

func CleanupVolumeResources(driver *SeaweedFsDriver, volumeID string) {
	cacheDir := GetCacheDir(driver.CacheDir, volumeID)

	// Validate that cacheDir is within cacheBase to prevent path traversal
	cacheBase := driver.CacheDir
	if cacheBase == "" {
		cacheBase = os.TempDir()
	}
	cleanCacheBase := filepath.Clean(cacheBase)
	cleanCacheDir := filepath.Clean(cacheDir)
	rel, err := filepath.Rel(cleanCacheBase, cleanCacheDir)
	if err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
		if err := os.RemoveAll(cleanCacheDir); err != nil {
			glog.Warningf("failed to remove cache dir %s for volume %s: %v", cleanCacheDir, volumeID, err)
		}
	} else {
		glog.Warningf("skipping cache dir removal for volume %s: invalid path %s (rel: %s, err: %v)", volumeID, cleanCacheDir, rel, err)
	}

	localSocket := GetLocalSocket(driver.volumeSocketDir, volumeID)
	if err := os.Remove(localSocket); err != nil && !os.IsNotExist(err) {
		glog.Warningf("failed to remove local socket %s for volume %s: %v", localSocket, volumeID, err)
	}
}

func NewIdentityServer(d *SeaweedFsDriver) *IdentityServer {
	return &IdentityServer{
		Driver: d,
	}
}

func NewControllerServer(d *SeaweedFsDriver) *ControllerServer {

	return &ControllerServer{
		Driver: d,
	}
}

func NewControllerServiceCapability(cap csi.ControllerServiceCapability_RPC_Type) *csi.ControllerServiceCapability {
	return &csi.ControllerServiceCapability{
		Type: &csi.ControllerServiceCapability_Rpc{
			Rpc: &csi.ControllerServiceCapability_RPC{
				Type: cap,
			},
		},
	}
}

func ParseEndpoint(ep string) (string, string, error) {
	if strings.HasPrefix(strings.ToLower(ep), "unix://") || strings.HasPrefix(strings.ToLower(ep), "tcp://") {
		s := strings.SplitN(ep, "://", 2)
		if s[1] != "" {
			return s[0], s[1], nil
		}
	}
	return "", "", fmt.Errorf("invalid endpoint: %v", ep)
}

func logGRPC(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	glog.V(3).Infof("GRPC %s request %+v", info.FullMethod, req)
	resp, err := handler(ctx, req)
	if err != nil {
		glog.Errorf("GRPC error: %v", err)
	}
	glog.V(3).Infof("GRPC %s response %+v", info.FullMethod, resp)
	return resp, err
}

type KeyMutex struct {
	mutexes sync.Map
}

func NewKeyMutex() *KeyMutex {
	return &KeyMutex{}
}

func (km *KeyMutex) GetMutex(key string) *sync.Mutex {
	m, _ := km.mutexes.LoadOrStore(key, &sync.Mutex{})

	return m.(*sync.Mutex)
}

func (km *KeyMutex) RemoveMutex(key string) {
	km.mutexes.Delete(key)
}

func CheckDataLocality(dataLocality *datalocality.DataLocality, dataCenter *string) error {
	if *dataLocality != datalocality.None && *dataCenter == "" {
		return fmt.Errorf("dataLocality set, but not all locality-definitions were set")
	}
	return nil
}
