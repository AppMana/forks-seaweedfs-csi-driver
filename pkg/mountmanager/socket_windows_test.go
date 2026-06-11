//go:build windows

package mountmanager

import (
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"testing"
)

// TestUnixSocketRoundTrip is the AF_UNIX-on-Windows canary: it verifies
// that the mount-server listen side (net.Listen("unix", ...)) and the
// Client's unix dialer round-trip a request on Windows Server 2019+.
func TestUnixSocketRoundTrip(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "s.sock")
	if len(sock) >= 108 {
		t.Skipf("socket path too long for AF_UNIX (%d bytes): %s", len(sock), sock)
	}

	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("net.Listen(unix, %s): %v", sock, err)
	}
	defer listener.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/mount", func(w http.ResponseWriter, r *http.Request) {
		var req MountRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(MountResponse{LocalSocket: req.LocalSocket})
	})
	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()

	client, err := NewClient("unix://" + sock)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	resp, err := client.Mount(&MountRequest{
		VolumeID:    "vol-1",
		TargetPath:  `C:\target`,
		CacheDir:    `C:\cache`,
		MountArgs:   []string{"mount"},
		LocalSocket: `C:\var\lib\seaweedfs-mount\vol-1.sock`,
	})
	if err != nil {
		t.Fatalf("client.Mount over AF_UNIX failed: %v", err)
	}
	if resp.LocalSocket != `C:\var\lib\seaweedfs-mount\vol-1.sock` {
		t.Fatalf("unexpected response: %+v", resp)
	}
}
