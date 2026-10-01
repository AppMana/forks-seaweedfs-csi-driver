//go:build linux

package driver

import (
	"os"
	"path/filepath"
	"testing"

	mount "k8s.io/mount-utils"
)

func TestLinuxStaleStagingCleanupPreservesUnmountedData(t *testing.T) {
	previous := mountutil
	mountutil = mount.NewFakeMounter(nil)
	t.Cleanup(func() { mountutil = previous })
	root := t.TempDir()
	staging := filepath.Join(root, "staging")
	if err := os.Mkdir(staging, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(staging, "intact-local-data")
	if err := os.WriteFile(file, []byte("keep me"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := cleanupStaleStagingPath(staging); err == nil {
		t.Error("unmounted does not mean disposable: must refuse nonempty staging directory")
	}
	if got, err := os.ReadFile(file); err != nil || string(got) != "keep me" {
		t.Fatalf("staging cleanup deleted intact data: %q, %v", got, err)
	}
}

func TestLinuxStaleStagingCleanupEmptyAndMissingAreIdempotent(t *testing.T) {
	previous := mountutil
	mountutil = mount.NewFakeMounter(nil)
	t.Cleanup(func() { mountutil = previous })
	staging := filepath.Join(t.TempDir(), "staging")
	if err := os.Mkdir(staging, 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := cleanupStaleStagingPath(staging); err != nil {
			t.Fatal(err)
		}
	}
}
