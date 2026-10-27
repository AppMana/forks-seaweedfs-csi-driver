package mountmanager

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type fakeWinFspHost struct {
	installed   []bool // answers to successive Installed calls
	version     string // version the host reports while installed
	checks      int
	exitCode    int
	runs        [][]string
	stagedBytes []byte
}

func (h *fakeWinFspHost) Installed() (string, bool, error) {
	answer := h.installed[min(h.checks, len(h.installed)-1)]
	h.checks++
	if !answer {
		return "", false, nil
	}
	return h.version, true, nil
}

func (h *fakeWinFspHost) RunMsiexec(args ...string) (int, error) {
	h.runs = append(h.runs, args)
	// msiexec reads the package the moment it runs.
	if len(args) > 1 {
		h.stagedBytes, _ = os.ReadFile(args[1])
	}
	return h.exitCode, nil
}

func newTestInstaller(t *testing.T, host *fakeWinFspHost) (WinFspInstaller, []byte) {
	t.Helper()
	image := t.TempDir()
	msi := filepath.Join(image, "winfsp.msi")
	payload := []byte("pinned winfsp msi")
	if err := os.WriteFile(msi, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if host.version == "" {
		host.version = PinnedWinFspVersion
	}
	return WinFspInstaller{
		MSI:        msi,
		Version:    PinnedWinFspVersion,
		StagingDir: t.TempDir(),
		Installed:  host.Installed,
		RunMsiexec: host.RunMsiexec,
	}, payload
}

func TestWinFspInstallerLeavesAnInstalledHostAlone(t *testing.T) {
	host := &fakeWinFspHost{installed: []bool{true}}
	installer, _ := newTestInstaller(t, host)
	if err := installer.Ensure(); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if len(host.runs) != 0 {
		t.Fatalf("msiexec ran on a host that has WinFsp: %v", host.runs)
	}
}

// msiexec's service runs outside the HostProcess container's silo and cannot
// open files under CONTAINER_SANDBOX_MOUNT_POINT (exit 1619), so the image's
// MSI is copied to a host directory first and installed from there.
func TestWinFspInstallerInstallsFromAHostVisibleCopy(t *testing.T) {
	host := &fakeWinFspHost{installed: []bool{false, true}}
	installer, payload := newTestInstaller(t, host)
	if err := installer.Ensure(); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if len(host.runs) != 1 {
		t.Fatalf("msiexec runs = %v, want one", host.runs)
	}
	args := host.runs[0]
	staged := filepath.Join(installer.StagingDir, "winfsp.msi")
	logPath := filepath.Join(installer.StagingDir, "seaweedfs-winfsp-install.log")
	if want := []string{"/i", staged, "/qn", "INSTALLLEVEL=1000", "/l*v", logPath}; !slices.Equal(args, want) {
		t.Fatalf("msiexec args = %q, want %q", args, want)
	}
	if !bytes.Equal(host.stagedBytes, payload) {
		t.Fatalf("msiexec read %q from the staged copy, want the image's MSI", host.stagedBytes)
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Fatalf("staged MSI copy left on the host: %v", err)
	}
}

// Start-Process without -PassThru swallowed msiexec's result and the first
// bootstrap failed silently; the exit code decides.
func TestWinFspInstallerFailsOnMsiexecExitCode(t *testing.T) {
	host := &fakeWinFspHost{installed: []bool{false, true}, exitCode: 1603}
	installer, _ := newTestInstaller(t, host)
	err := installer.Ensure()
	if err == nil || !strings.Contains(err.Error(), "1603") {
		t.Fatalf("Ensure error = %v, want the msiexec exit code 1603", err)
	}
	if logPath := filepath.Join(installer.StagingDir, "seaweedfs-winfsp-install.log"); !strings.Contains(err.Error(), logPath) {
		t.Fatalf("Ensure error = %v, want it to name the msiexec log %s", err, logPath)
	}
	if _, statErr := os.Stat(filepath.Join(installer.StagingDir, "winfsp.msi")); !os.IsNotExist(statErr) {
		t.Fatalf("staged MSI copy left on the host after a failed install: %v", statErr)
	}
}

func TestWinFspInstallerFailsWhenStillMissingAfterInstall(t *testing.T) {
	host := &fakeWinFspHost{installed: []bool{false, false}}
	installer, _ := newTestInstaller(t, host)
	if err := installer.Ensure(); err == nil {
		t.Fatal("Ensure succeeded although WinFsp is still not registered")
	}
}

func TestWinFspInstallerFailsWithoutTheMSI(t *testing.T) {
	host := &fakeWinFspHost{installed: []bool{false}}
	installer, _ := newTestInstaller(t, host)
	installer.MSI = filepath.Join(t.TempDir(), "missing.msi")
	if err := installer.Ensure(); err == nil {
		t.Fatal("Ensure succeeded without an MSI to install")
	}
	if len(host.runs) != 0 {
		t.Fatalf("msiexec ran without an MSI: %v", host.runs)
	}
}

// The mount image puts winfsp.msi beside seaweedfs-mount.exe; HostProcess
// containers run the binary from CONTAINER_SANDBOX_MOUNT_POINT, whose path
// differs per container, so the MSI is found relative to the executable.
func TestDefaultWinFspMSIIsBesideTheExecutable(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	got, err := DefaultWinFspMSI()
	if err != nil {
		t.Fatalf("DefaultWinFspMSI: %v", err)
	}
	if want := filepath.Join(filepath.Dir(exe), "winfsp.msi"); got != want {
		t.Fatalf("DefaultWinFspMSI = %q, want %q", got, want)
	}
}

// weed.exe loads the DLL shipped beside it, built against the pinned
// driver's ABI. A host with another WinFsp version cannot serve mounts, and
// a DaemonSet roll must not up- or downgrade a kernel driver behind the
// operator's back: startup fails and names both versions.
func TestWinFspInstallerRefusesAnotherInstalledVersion(t *testing.T) {
	host := &fakeWinFspHost{installed: []bool{true}, version: "2.0.23075"}
	installer, _ := newTestInstaller(t, host)
	err := installer.Ensure()
	if err == nil || !strings.Contains(err.Error(), "2.0.23075") || !strings.Contains(err.Error(), PinnedWinFspVersion) {
		t.Fatalf("Ensure error = %v, want both the installed and the pinned version", err)
	}
	if len(host.runs) != 0 {
		t.Fatalf("msiexec ran over another installed version: %v", host.runs)
	}
}

func TestWinFspInstallerChecksTheVersionItInstalled(t *testing.T) {
	host := &fakeWinFspHost{installed: []bool{false, true}, version: "2.0.23075"}
	installer, _ := newTestInstaller(t, host)
	if err := installer.Ensure(); err == nil {
		t.Fatal("Ensure accepted a version other than the pinned one after installing")
	}
}

// The pinned version is the one the mount image's MSI installs.
func TestPinnedWinFspVersionMatchesTheImageMSI(t *testing.T) {
	dockerfile, err := os.ReadFile(filepath.Join("..", "..", "cmd", "seaweedfs-mount", "Dockerfile.Windows"))
	if err != nil {
		t.Fatal(err)
	}
	want := "/winfsp-" + PinnedWinFspVersion + ".msi"
	for _, line := range strings.Split(string(dockerfile), "\n") {
		// Windows checkouts may convert line endings.
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "ARG WINFSP_MSI_URL=") {
			if !strings.HasSuffix(line, want) {
				t.Fatalf("%s does not install WinFsp %s", line, PinnedWinFspVersion)
			}
			return
		}
	}
	t.Fatal("Dockerfile.Windows has no WINFSP_MSI_URL")
}

// The lab's kernel-candidate lane installs a candidate WinFsp before the
// mount starts; with no required version the installed one is accepted.
func TestWinFspInstallerWithoutARequiredVersionAcceptsTheInstalledOne(t *testing.T) {
	host := &fakeWinFspHost{installed: []bool{true}, version: "2.1.99999"}
	installer, _ := newTestInstaller(t, host)
	installer.Version = ""
	if err := installer.Ensure(); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
}
