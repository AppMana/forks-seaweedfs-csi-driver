//go:build linux

package mountmanager

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// holdOpen records in the fake proc tree that process pid holds path open.
func holdOpen(t *testing.T, procRoot string, pid int, path string) {
	t.Helper()
	fdDir := filepath.Join(procRoot, strconv.Itoa(pid), "fd")
	if err := os.MkdirAll(fdDir, 0o755); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(fdDir)
	if err := os.Symlink(path, filepath.Join(fdDir, strconv.Itoa(3+len(entries)))); err != nil {
		t.Fatal(err)
	}
}

func instanceDir(t *testing.T, cacheDir, name string) string {
	t.Helper()
	dir := filepath.Join(cacheDir, name, "meta")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "LOCK"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(cacheDir, name)
}

// weed mount names its instance cache directory after its version, so a
// mount restarted on a new weed leaves the old instance directory behind.
// Starting the mount removes every instance directory of the volume that
// no process holds open, and keeps the one the new weed mount uses and any
// another running process holds.
func TestMountRemovesUnheldInstanceCaches(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	procRoot := filepath.Join(root, "proc")
	stale := instanceDir(t, cacheDir, "11111111")
	live := instanceDir(t, cacheDir, "22222222")
	other := instanceDir(t, cacheDir, "33333333")
	holdOpen(t, procRoot, 200, filepath.Join(other, "meta", "LOCK"))

	process := &weedMountProcess{exited: make(chan struct{}), done: make(chan struct{})}
	t.Cleanup(func() { close(process.exited); close(process.done) })
	origStart := startWeedMount
	t.Cleanup(func() { startWeedMount = origStart })
	startWeedMount = func(command string, args []string, target, volumeID string) (*weedMountProcess, error) {
		// The new weed mount has opened its instance directory by the
		// time its mount is up.
		holdOpen(t, procRoot, 100, filepath.Join(live, "meta", "LOCK"))
		return process, nil
	}

	m := NewManager(Config{WeedBinary: "weed"})
	m.procRoot = procRoot
	if _, err := m.Mount(&MountRequest{
		VolumeID:    "vol-1",
		TargetPath:  filepath.Join(root, "target"),
		CacheDir:    cacheDir,
		LocalSocket: filepath.Join(root, "vol-1.sock"),
		MountArgs:   []string{"mount"},
	}); err != nil {
		t.Fatalf("Mount: %v", err)
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("unheld instance cache %s was kept (stat err %v)", stale, err)
	}
	for _, dir := range []string{live, other} {
		if _, err := os.Stat(filepath.Join(dir, "meta", "LOCK")); err != nil {
			t.Errorf("held instance cache %s was removed: %v", dir, err)
		}
	}
}

// When the holders cannot be determined, nothing is removed.
func TestRemoveUnheldInstanceCachesKeepsAllWithoutProc(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	stale := instanceDir(t, cacheDir, "11111111")
	removeUnheldInstanceCaches(cacheDir, filepath.Join(root, "no-proc"))
	if _, err := os.Stat(stale); err != nil {
		t.Errorf("instance cache removed without knowing its holders: %v", err)
	}
}
