//go:build windows

package mountmanager

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// winFspRegistryKey is where the WinFsp MSI registers its installation.
const winFspRegistryKey = `SOFTWARE\WOW6432Node\WinFsp`

// uninstallKeys hold the installed products' Windows Installer entries; the
// WinFsp entry (DisplayName "WinFsp <year>") carries its DisplayVersion,
// e.g. 2.1.25156.
var uninstallKeys = []string{
	`SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall`,
	`SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`,
}

// winFspInstalled reports whether WinFsp is installed and the version its
// Windows Installer entry records. An installation without such an entry
// reports an empty version.
func winFspInstalled() (string, bool, error) {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, winFspRegistryKey, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	key.Close()
	for _, path := range uninstallKeys {
		version, found, err := winFspUninstallVersion(path)
		if err != nil {
			return "", true, err
		}
		if found {
			return version, true, nil
		}
	}
	return "", true, nil
}

func winFspUninstallVersion(path string) (string, bool, error) {
	parent, err := registry.OpenKey(registry.LOCAL_MACHINE, path, registry.ENUMERATE_SUB_KEYS)
	if errors.Is(err, registry.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	defer parent.Close()
	names, err := parent.ReadSubKeyNames(-1)
	if err != nil {
		return "", false, err
	}
	for _, name := range names {
		product, err := registry.OpenKey(parent, name, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		displayName, _, nameErr := product.GetStringValue("DisplayName")
		version, _, versionErr := product.GetStringValue("DisplayVersion")
		product.Close()
		if nameErr == nil && versionErr == nil && strings.HasPrefix(displayName, "WinFsp ") {
			return version, true, nil
		}
	}
	return "", false, nil
}

func runMsiexec(args ...string) (int, error) {
	err := exec.Command("msiexec", args...).Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	if err != nil {
		return 0, err
	}
	return 0, nil
}

// EnsureWinFsp installs WinFsp from msi unless the host has it, staging the
// package in the Windows temp directory, and requires the installed version
// to be version (empty accepts any).
func EnsureWinFsp(msi, version string) error {
	systemRoot := os.Getenv("SystemRoot")
	if systemRoot == "" {
		systemRoot = `C:\Windows`
	}
	return WinFspInstaller{
		MSI:        msi,
		Version:    version,
		StagingDir: filepath.Join(systemRoot, "Temp"),
		Installed:  winFspInstalled,
		RunMsiexec: runMsiexec,
	}.Ensure()
}
