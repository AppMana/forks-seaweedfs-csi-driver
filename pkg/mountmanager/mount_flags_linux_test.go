//go:build linux

package mountmanager

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/mount-utils"
)

// fakeWeedHelp is `weed mount -h` output in the format weed prints it: the
// Go flag package's PrintDefaults under an example line. It deliberately
// lacks -wholeCacheCapacityMB, like the weed builds that predate it.
const fakeWeedHelp = `Example: weed mount -filer=localhost:8888 -dir=/some/dir
Default Usage:
  -cacheCapacityMB int
    	file chunk read cache capacity in MB (default 128)
  -cacheDir string
    	local cache directory for file chunks and meta data (default "/tmp")
  -collection string
    	collection to create the files
  -dir string
    	mount weed filer to this directory (default ".")
  -dirAutoCreate
    	auto create the directory to mount to
  -filer string
    	comma-separated weed filer location (default "localhost:8888")
  -filer.path string
    	mount this remote path from filer server (default "/")
  -localSocket string
    	default to /tmp/seaweedfs-mount-<mount_dir_hash>.sock
  -readOnly
    	read only
  -umask string
    	octal umask, e.g., 022, 0111 (default "022")
`

// fakeWeedScript stands in for the weed binary. `weed version` and
// `weed mount -h` answer like weed does; `weed mount ...` records its
// arguments, rejects flags the help text does not list exactly as Go's flag
// package does (usage error, exit 2), and otherwise behaves as a running
// mount, or exits at once with the configured failure.
const fakeWeedScript = `#!/bin/sh
dir=$(dirname "$0")
if [ "$1" = "version" ]; then
  echo "version 8000GB 4.00 fakecommit linux amd64"
  exit 0
fi
if [ "$1" = "mount" ] && [ "$2" = "-h" ]; then
  cat "$dir/help" >&2
  exit 2
fi
printf '%s\n' "$@" > "$dir/args"
seen_mount=
for a in "$@"; do
  if [ -z "$seen_mount" ]; then
    [ "$a" = "mount" ] && seen_mount=1
    continue
  fi
  name=${a#-}
  name=${name#-}
  name=${name%%=*}
  if ! grep -q -e "^  -$name\$" -e "^  -$name " "$dir/help"; then
    echo "flag provided but not defined: -$name" >&2
    cat "$dir/help" >&2
    exit 2
  fi
done
if [ -f "$dir/fail" ]; then
  cat "$dir/fail" >&2
  exit 1
fi
touch "$dir/running"
exec sleep 60
`

type fakeWeed struct {
	dir    string
	binary string
}

func newFakeWeed(t *testing.T, help string) *fakeWeed {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "help"), []byte(help), 0644); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "weed")
	if err := os.WriteFile(binary, []byte(fakeWeedScript), 0755); err != nil {
		t.Fatal(err)
	}
	return &fakeWeed{dir: dir, binary: binary}
}

// args returns the arguments the last `weed mount` was started with, or nil
// if none was started.
func (w *fakeWeed) args(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(w.dir, "args"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

// mountWhenRunning plays FUSE: once the fake weed mount is running it makes
// target a mount point, as the kernel does when weed mount comes up.
func (w *fakeWeed) mountWhenRunning(t *testing.T, fake *mount.FakeMounter, target string) {
	t.Helper()
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(10 * time.Millisecond):
			}
			if _, err := os.Stat(filepath.Join(w.dir, "running")); err == nil {
				_ = fake.Mount("fuse", target, "fuse.seaweedfs", nil)
				return
			}
		}
	}()
}

func newFlagSkewRequest(t *testing.T, extra ...string) *MountRequest {
	t.Helper()
	root := t.TempDir()
	target := filepath.Join(root, "staging")
	args := []string{
		"-logtostderr=true",
		"mount",
		"-dirAutoCreate=true",
		"-umask=000",
		"-dir=" + target,
		"-localSocket=" + filepath.Join(root, "mount.sock"),
		"-cacheDir=" + filepath.Join(root, "cache"),
		"-collection=pvc-1",
		"-filer=filer:8888",
		"-filer.path=/buckets/pvc-1",
		"-cacheCapacityMB=128",
	}
	return &MountRequest{
		VolumeID:    "pvc-1",
		TargetPath:  target,
		CacheDir:    filepath.Join(root, "cache"),
		LocalSocket: filepath.Join(root, "mount.sock"),
		MountArgs:   append(args, extra...),
	}
}

func newFlagSkewManager(t *testing.T, weed *fakeWeed) (*Manager, *mount.FakeMounter) {
	t.Helper()
	fake := mount.NewFakeMounter(nil)
	restore := withFakeMounter("", fake, func(string) error { return nil })
	t.Cleanup(restore)
	return NewManager(Config{WeedBinary: weed.binary}), fake
}

// TestMountDropsOptionalFlagUnknownToWeed reproduces appmana-030 on
// 2026-10-27: a node plugin whose template passes
// -wholeCacheCapacityMB, against a mount service whose weed predates that
// flag. The mount service must drop the cache tuning flag and mount.
func TestMountDropsOptionalFlagUnknownToWeed(t *testing.T) {
	weed := newFakeWeed(t, fakeWeedHelp)
	m, fake := newFlagSkewManager(t, weed)
	req := newFlagSkewRequest(t, "-wholeCacheCapacityMB=51200")
	weed.mountWhenRunning(t, fake, req.TargetPath)

	if _, err := m.Mount(req); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	t.Cleanup(func() { _, _ = m.Unmount(&UnmountRequest{VolumeID: req.VolumeID}) })

	args := weed.args(t)
	for _, arg := range args {
		if strings.HasPrefix(arg, "-wholeCacheCapacityMB") {
			t.Fatalf("weed mount started with a flag it does not define: %v", args)
		}
	}
	for _, want := range []string{"-logtostderr=true", "mount", "-filer=filer:8888", "-filer.path=/buckets/pvc-1", "-cacheCapacityMB=128"} {
		found := false
		for _, arg := range args {
			if arg == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("weed mount args lost %q: %v", want, args)
		}
	}
}

// TestMountFailsFastOnRequiredFlagUnknownToWeed: a flag the mount cannot
// be correct without must not be dropped. The mount fails at once with an
// error naming the flag, and weed mount is never started.
func TestMountFailsFastOnRequiredFlagUnknownToWeed(t *testing.T) {
	help := strings.Replace(fakeWeedHelp, "  -filer.path string\n    \tmount this remote path from filer server (default \"/\")\n", "", 1)
	weed := newFakeWeed(t, help)
	m, _ := newFlagSkewManager(t, weed)
	req := newFlagSkewRequest(t)

	start := time.Now()
	_, err := m.Mount(req)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Mount succeeded although weed does not define -filer.path")
	}
	if !strings.Contains(err.Error(), "-filer.path") {
		t.Fatalf("error does not name the missing flag: %v", err)
	}
	if strings.Contains(err.Error(), "timeout waiting for mount") {
		t.Fatalf("required-flag failure surfaced as a mount timeout: %v", err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("required-flag failure took %v", elapsed)
	}
	if args := weed.args(t); args != nil {
		t.Fatalf("weed mount was started: %v", args)
	}
}

// TestMountFailsFastOnRequestRequiredFlagUnknownToWeed: a flag the node
// plugin marks required in the request is not dropped either, so a newer
// node plugin's required flag holds against an older mount service.
func TestMountFailsFastOnRequestRequiredFlagUnknownToWeed(t *testing.T) {
	weed := newFakeWeed(t, fakeWeedHelp)
	m, _ := newFlagSkewManager(t, weed)
	req := newFlagSkewRequest(t, "-wholeCacheCapacityMB=51200")
	req.RequiredFlags = []string{"wholeCacheCapacityMB"}

	_, err := m.Mount(req)
	if err == nil {
		t.Fatal("Mount succeeded although weed does not define a required flag")
	}
	if !strings.Contains(err.Error(), "-wholeCacheCapacityMB") {
		t.Fatalf("error does not name the missing flag: %v", err)
	}
	if args := weed.args(t); args != nil {
		t.Fatalf("weed mount was started: %v", args)
	}
}

// TestParseWeedMountHelp reads flag names from weed's real help format:
// typed and boolean flags, dotted names, and usage lines that mention
// other flags.
func TestParseWeedMountHelp(t *testing.T) {
	help := fakeWeedHelp + "  -map.uid string\n    \tmap local uid to uid on filer, comma-separated <local_uid>:<filer_uid>\n    \t-not.a.flag in usage text\n"
	defined := parseWeedMountHelp([]byte(help))
	for _, name := range []string{"cacheCapacityMB", "dirAutoCreate", "filer.path", "readOnly", "map.uid"} {
		if _, ok := defined[name]; !ok {
			t.Errorf("%s not parsed from help", name)
		}
	}
	for _, name := range []string{"not.a.flag", "filer=localhost:8888", "wholeCacheCapacityMB"} {
		if _, ok := defined[name]; ok {
			t.Errorf("%s parsed as a flag", name)
		}
	}
	if len(defined) != 11 {
		t.Errorf("parsed %d flags, want 11: %v", len(defined), defined)
	}
}

// TestMountReportsImmediateWeedExitPromptly: a weed mount that exits
// before the mount comes up is reported with its exit status and output as
// soon as it exits, not after the mount wait times out.
func TestMountReportsImmediateWeedExitPromptly(t *testing.T) {
	weed := newFakeWeed(t, fakeWeedHelp)
	if err := os.WriteFile(filepath.Join(weed.dir, "fail"), []byte("F1027 mount.go:1] filer filer:8888 is unreachable\n"), 0644); err != nil {
		t.Fatal(err)
	}
	m, _ := newFlagSkewManager(t, weed)
	req := newFlagSkewRequest(t)

	start := time.Now()
	_, err := m.Mount(req)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Mount succeeded although weed mount exited")
	}
	if strings.Contains(err.Error(), "timeout waiting for mount") {
		t.Fatalf("weed exit surfaced as a mount timeout after %v: %v", elapsed, err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("weed exit reported after %v: %v", elapsed, err)
	}
	for _, want := range []string{"exit status 1", "filer filer:8888 is unreachable"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error lacks %q: %v", want, err)
		}
	}
}
