//go:build linux

package driver

import (
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"slices"
	"testing"

	"github.com/seaweedfs/seaweedfs-csi-driver/pkg/mountmanager"
	"github.com/seaweedfs/seaweedfs/weed/pb"
)

// The node plugin tells the mount service which flags the mount must not run
// without, so a mount service of an older build, whose weed lacks a flag
// this build requires, fails the mount instead of dropping the flag.
func TestMountRequestCarriesRequiredFlags(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "mount.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	requests := make(chan mountmanager.MountRequest, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req mountmanager.MountRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode mount request: %v", err)
		}
		requests <- req
		_ = json.NewEncoder(w).Encode(mountmanager.MountResponse{LocalSocket: req.LocalSocket})
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	driver := &SeaweedFsDriver{
		mountEndpoint:   "unix://" + socket,
		volumeSocketDir: t.TempDir(),
		CacheDir:        t.TempDir(),
		filers:          []pb.ServerAddress{"filer:8888"},
	}
	mounter, err := newMounter("/buckets/pvc-1", false, driver, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mounter.Mount("/staging"); err != nil {
		t.Fatalf("Mount: %v", err)
	}

	req := <-requests
	if !slices.Equal(req.RequiredFlags, mountmanager.RequiredMountFlags) {
		t.Fatalf("requiredFlags = %v, want %v", req.RequiredFlags, mountmanager.RequiredMountFlags)
	}
}
