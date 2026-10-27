package driver

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// A node plugin started without --cacheDir or --stagingScanDir must use the
// platform's own paths: Windows must never fall back to a Linux path or the
// SYSTEM temp directory, and staged volumes must be rediscovered after a
// plugin restart without an extra flag.
func TestPlatformDefaults(t *testing.T) {
	var wantCache, wantScan string
	switch runtime.GOOS {
	case "linux":
		wantCache = "/var/cache/seaweedfs"
		wantScan = "/var/lib/kubelet/plugins/kubernetes.io/csi"
	case "windows":
		wantCache = `C:\var\cache\seaweedfs`
		wantScan = `C:\var\lib\kubelet\plugins\kubernetes.io\csi`
	default:
		t.Fatalf("no platform defaults for %s", runtime.GOOS)
	}
	if DefaultCacheDir != wantCache {
		t.Errorf("DefaultCacheDir = %q, want %q", DefaultCacheDir, wantCache)
	}
	if DefaultStagingScanDir != wantScan {
		t.Errorf("DefaultStagingScanDir = %q, want %q", DefaultStagingScanDir, wantScan)
	}
	if DefaultCacheDir == filepath.Clean(os.TempDir()) {
		t.Errorf("DefaultCacheDir is the temp directory, which the orphan sweep never cleans")
	}
}

// The CSI endpoint's directory may not exist on a fresh node (Windows has no
// DirectoryOrCreate hostPath to create it); the plugin creates it.
func TestPrepareUnixEndpointCreatesDirectoryAndRemovesStaleSocket(t *testing.T) {
	root := t.TempDir()
	addr := filepath.Join(root, "plugins", "seaweedfs-csi-driver", "csi.sock")
	if err := prepareUnixEndpoint(addr); err != nil {
		t.Fatalf("prepareUnixEndpoint on a missing directory: %v", err)
	}
	if info, err := os.Stat(filepath.Dir(addr)); err != nil || !info.IsDir() {
		t.Fatalf("endpoint directory not created: %v", err)
	}
	if err := os.WriteFile(addr, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prepareUnixEndpoint(addr); err != nil {
		t.Fatalf("prepareUnixEndpoint with a stale socket file: %v", err)
	}
	if _, err := os.Stat(addr); !os.IsNotExist(err) {
		t.Fatalf("stale socket file not removed: %v", err)
	}
}
