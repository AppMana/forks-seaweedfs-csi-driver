//go:build windows

package mountmanager

import (
	"os"
	"path/filepath"
	"testing"
)

// The pinned MSI the mount image ships, downloaded and checksum-verified by
// CI. The hosted runner has no WinFsp, so this installs it for real through
// the same code the mount service runs at startup.
func TestEnsureWinFspInstallsPinnedMSI(t *testing.T) {
	msi := os.Getenv("SEAWEEDFS_TEST_WINFSP_MSI")
	if msi == "" {
		t.Skip("SEAWEEDFS_TEST_WINFSP_MSI names the pinned WinFsp MSI")
	}
	_, installed, err := winFspInstalled()
	if err != nil {
		t.Fatalf("reading the WinFsp registry key: %v", err)
	}
	if installed {
		t.Fatal("WinFsp is already installed on this host, so the install path would not run")
	}
	if err := EnsureWinFsp(msi, PinnedWinFspVersion); err != nil {
		t.Fatalf("EnsureWinFsp: %v", err)
	}
	version, installed, err := winFspInstalled()
	if err != nil || !installed || version != PinnedWinFspVersion {
		t.Fatalf("after EnsureWinFsp: version=%q installed=%v err=%v, want %s", version, installed, err, PinnedWinFspVersion)
	}
	dll := filepath.Join(os.Getenv("ProgramFiles(x86)"), "WinFsp", "bin", "winfsp-x64.dll")
	if _, err := os.Stat(dll); err != nil {
		t.Fatalf("WinFsp DLL missing after install: %v", err)
	}
	// A second start finds it installed and does not run msiexec again.
	if err := EnsureWinFsp(filepath.Join(t.TempDir(), "absent.msi"), PinnedWinFspVersion); err != nil {
		t.Fatalf("EnsureWinFsp on an installed host: %v", err)
	}
}
