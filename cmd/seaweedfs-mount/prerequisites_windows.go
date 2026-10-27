//go:build windows

package main

import (
	"flag"
	"fmt"

	"github.com/seaweedfs/seaweedfs-csi-driver/pkg/mountmanager"
)

var (
	winfspMSI     = flag.String("winfspMSI", "", "WinFsp MSI installed on the host when it lacks WinFsp; default winfsp.msi beside this executable")
	winfspVersion = flag.String("winfspVersion", mountmanager.PinnedWinFspVersion, "WinFsp version the host must run, the one the image's DLL is built for; empty accepts any installed version (lab kernel-driver candidates only)")
)

// ensureHostPrerequisites installs WinFsp before any mount is accepted.
func ensureHostPrerequisites() error {
	msi := *winfspMSI
	if msi == "" {
		var err error
		if msi, err = mountmanager.DefaultWinFspMSI(); err != nil {
			return fmt.Errorf("locating the WinFsp MSI: %w", err)
		}
	}
	return mountmanager.EnsureWinFsp(msi, *winfspVersion)
}
