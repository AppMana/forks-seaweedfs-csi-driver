package mountmanager

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// PinnedWinFspVersion is the WinFsp release the mount image's MSI installs
// (cmd/seaweedfs-mount/Dockerfile.Windows WINFSP_MSI_URL) and the driver ABI
// of the WinFsp DLL shipped beside weed.exe.
const PinnedWinFspVersion = "2.1.25156"

// WinFspInstaller installs WinFsp on a Windows host from the MSI the mount
// image ships, when the host does not have it. The kernel driver is a host
// component every weed.exe mount needs; the mount service installs it before
// it accepts mounts, so a node needs no separate bootstrap.
type WinFspInstaller struct {
	// MSI is the installer inside the image.
	MSI string
	// Version is the WinFsp version MSI installs, which the host must run.
	// Empty accepts any installed version (lab kernel-driver candidates).
	Version string
	// StagingDir is a host directory msiexec can read. The Windows Installer
	// service runs outside the HostProcess container's silo and cannot open
	// files under CONTAINER_SANDBOX_MOUNT_POINT (exit 1619).
	StagingDir string
	// Installed reports whether WinFsp is installed on the host, and its
	// version.
	Installed func() (version string, installed bool, err error)
	// RunMsiexec runs msiexec with args to completion and returns its exit code.
	RunMsiexec func(args ...string) (int, error)
}

// Ensure installs WinFsp unless it is installed already, and requires the
// installed version to be Version. Any non-zero msiexec exit fails it, naming
// msiexec's verbose log; so does a host that lacks WinFsp, or runs another
// version, after a successful install. Another installed version is never
// replaced: changing a host's kernel driver is the operator's decision.
func (w WinFspInstaller) Ensure() error {
	version, installed, err := w.Installed()
	if err != nil {
		return fmt.Errorf("checking for WinFsp: %w", err)
	}
	if installed {
		return w.requireVersion(version)
	}
	staged := filepath.Join(w.StagingDir, "winfsp.msi")
	if err := copyFile(w.MSI, staged); err != nil {
		return fmt.Errorf("copying the WinFsp MSI to %s: %w", staged, err)
	}
	defer os.Remove(staged)
	logPath := filepath.Join(w.StagingDir, "seaweedfs-winfsp-install.log")
	code, err := w.RunMsiexec("/i", staged, "/qn", "INSTALLLEVEL=1000", "/l*v", logPath)
	if err != nil {
		return fmt.Errorf("running msiexec (log %s): %w", logPath, err)
	}
	if code != 0 {
		return fmt.Errorf("WinFsp %s MSI install failed with exit code %d; msiexec log %s", w.Version, code, logPath)
	}
	if version, installed, err = w.Installed(); err != nil {
		return fmt.Errorf("checking for WinFsp after install: %w", err)
	}
	if !installed {
		return fmt.Errorf("WinFsp is still not installed after a successful MSI install; msiexec log %s", logPath)
	}
	if err := w.requireVersion(version); err != nil {
		return fmt.Errorf("after install: %w; msiexec log %s", err, logPath)
	}
	os.Remove(logPath)
	return nil
}

func (w WinFspInstaller) requireVersion(installed string) error {
	if w.Version != "" && installed != w.Version {
		return fmt.Errorf("host runs WinFsp %q, but this mount image ships the DLL for WinFsp %s; install WinFsp %s on the host", installed, w.Version, w.Version)
	}
	return nil
}

func copyFile(source, target string) (err error) {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := out.Close(); err == nil {
			err = closeErr
		}
	}()
	_, err = io.Copy(out, in)
	return err
}

// DefaultWinFspMSI is the MSI the mount image ships beside the executable.
func DefaultWinFspMSI() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(exe), "winfsp.msi"), nil
}
