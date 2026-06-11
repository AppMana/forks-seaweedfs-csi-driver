//go:build windows

package driver

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCheckMountMissingPath verifies that checkMount reports a missing
// target as not mounted WITHOUT creating the directory: on Windows the
// publish step replaces the target with a symlink and the staging step
// requires the WinFsp mount point to not pre-exist.
func TestCheckMountMissingPath(t *testing.T) {
	target := filepath.Join(t.TempDir(), "missing")

	mounted, err := checkMount(target)
	if err != nil {
		t.Fatalf("checkMount(%s) returned error: %v", target, err)
	}
	if mounted {
		t.Fatalf("checkMount(%s) = true, want false for missing path", target)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("checkMount must not create the missing target, Lstat err: %v", err)
	}
}

// TestCheckMountPlainDir verifies that an ordinary directory is not
// considered mounted.
func TestCheckMountPlainDir(t *testing.T) {
	target := t.TempDir()

	mounted, err := checkMount(target)
	if err != nil {
		t.Fatalf("checkMount(%s) returned error: %v", target, err)
	}
	if mounted {
		t.Fatalf("checkMount(%s) = true, want false for plain directory", target)
	}
}

// TestCheckMountSymlink verifies that a directory symlink (a reparse
// point, as produced by NodePublishVolume) is considered mounted.
func TestCheckMountSymlink(t *testing.T) {
	tmp := t.TempDir()
	source := filepath.Join(tmp, "source")
	if err := os.Mkdir(source, 0755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(tmp, "target")
	if err := os.Symlink(source, target); err != nil {
		t.Skipf("cannot create symlink (requires admin or developer mode): %v", err)
	}

	mounted, err := checkMount(target)
	if err != nil {
		t.Fatalf("checkMount(%s) returned error: %v", target, err)
	}
	if !mounted {
		t.Fatalf("checkMount(%s) = false, want true for symlink", target)
	}
}

// TestCleanupTolerateMissing verifies that all cleanup helpers tolerate
// the path not existing.
func TestCleanupTolerateMissing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")

	if err := cleanupStaleStagingPath(missing); err != nil {
		t.Errorf("cleanupStaleStagingPath(%s) = %v, want nil", missing, err)
	}
	if err := cleanupCorruptedStagingPath(missing); err != nil {
		t.Errorf("cleanupCorruptedStagingPath(%s) = %v, want nil", missing, err)
	}
	if err := cleanupMountPoint(missing); err != nil {
		t.Errorf("cleanupMountPoint(%s) = %v, want nil", missing, err)
	}
	if err := unmountVolume(missing); err != nil {
		t.Errorf("unmountVolume(%s) = %v, want nil", missing, err)
	}
}

// TestCleanupRemovesEmptyDirNonRecursively verifies cleanup removes an
// empty directory but refuses (errors) on a non-empty one instead of
// recursing.
func TestCleanupRemovesEmptyDirNonRecursively(t *testing.T) {
	tmp := t.TempDir()

	empty := filepath.Join(tmp, "empty")
	if err := os.Mkdir(empty, 0755); err != nil {
		t.Fatal(err)
	}
	if err := cleanupStaleStagingPath(empty); err != nil {
		t.Fatalf("cleanupStaleStagingPath(%s) = %v, want nil", empty, err)
	}
	if _, err := os.Lstat(empty); !os.IsNotExist(err) {
		t.Fatalf("empty directory was not removed, Lstat err: %v", err)
	}

	nonEmpty := filepath.Join(tmp, "nonempty")
	if err := os.Mkdir(nonEmpty, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nonEmpty, "data"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := cleanupStaleStagingPath(nonEmpty); err == nil {
		t.Fatal("cleanupStaleStagingPath on a non-empty directory must fail rather than recurse")
	}
	if _, err := os.Stat(filepath.Join(nonEmpty, "data")); err != nil {
		t.Fatalf("cleanup must not delete directory contents: %v", err)
	}
}

// TestDefaultBindMountReplacesEmptyDir verifies that publish replaces
// kubelet's pre-created empty target directory with a symlink pointing
// at the staging source.
func TestDefaultBindMountReplacesEmptyDir(t *testing.T) {
	tmp := t.TempDir()
	source := filepath.Join(tmp, "source")
	if err := os.Mkdir(source, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "hello.txt"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(tmp, "target")
	if err := os.Mkdir(target, 0755); err != nil {
		t.Fatal(err)
	}

	if err := defaultBindMount(source, target, false); err != nil {
		t.Fatalf("defaultBindMount(%s, %s) = %v", source, target, err)
	}

	fi, err := os.Lstat(target)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("target %s is not a symlink after defaultBindMount", target)
	}

	resolved, err := os.Readlink(target)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != source {
		t.Fatalf("symlink resolves to %s, want %s", resolved, source)
	}

	data, err := os.ReadFile(filepath.Join(target, "hello.txt"))
	if err != nil {
		t.Fatalf("reading through publish symlink: %v", err)
	}
	if string(data) != "hello" {
		t.Fatalf("read %q through publish symlink, want %q", data, "hello")
	}

	// Mounted target must be reported as such, and unpublish must remove
	// only the symlink, not the source contents.
	if mounted, err := checkMount(target); err != nil || !mounted {
		t.Fatalf("checkMount(%s) = %v, %v; want true, nil", target, mounted, err)
	}
	if err := cleanupMountPoint(target); err != nil {
		t.Fatalf("cleanupMountPoint(%s) = %v", target, err)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("publish symlink was not removed, Lstat err: %v", err)
	}
	if _, err := os.Stat(filepath.Join(source, "hello.txt")); err != nil {
		t.Fatalf("source contents must survive unpublish: %v", err)
	}
}
