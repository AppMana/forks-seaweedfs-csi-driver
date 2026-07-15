package driver

import (
	"strings"
	"testing"
)

// TestBuildMountArgsReaderCacheMode is a regression test for the
// readerCacheMode passthrough: a StorageClass/PVC "readerCacheMode"
// parameter (surfaced into VolumeContext) must reach weed mount as a
// -readerCacheMode=<value> flag, and when unset must be omitted entirely
// (letting weed mount's own CLI default, "auto", apply) rather than being
// passed as an empty string.
func TestBuildMountArgsReaderCacheMode(t *testing.T) {
	newMounterUnderTest := func(volumeContext map[string]string) *mountServiceMounter {
		return &mountServiceMounter{
			driver: &SeaweedFsDriver{
				ConcurrentReaders: 128,
				ConcurrentWriters: 128,
			},
			volumeID:   "/buckets/test-volume",
			volContext: volumeContext,
		}
	}

	t.Run("unset omits the flag", func(t *testing.T) {
		m := newMounterUnderTest(map[string]string{})
		args, err := m.buildMountArgs("/mnt/target", "/var/cache/seaweedfs/abc", "unix:///tmp/sock", []string{"localhost:8888"})
		if err != nil {
			t.Fatalf("buildMountArgs: %v", err)
		}
		for _, a := range args {
			if strings.HasPrefix(a, "-readerCacheMode=") {
				t.Fatalf("expected -readerCacheMode to be omitted when unset, got %q", a)
			}
		}
	})

	t.Run("StorageClass parameter reaches weed mount argv", func(t *testing.T) {
		m := newMounterUnderTest(map[string]string{"readerCacheMode": "sequential"})
		args, err := m.buildMountArgs("/mnt/target", "/var/cache/seaweedfs/abc", "unix:///tmp/sock", []string{"localhost:8888"})
		if err != nil {
			t.Fatalf("buildMountArgs: %v", err)
		}
		found := false
		for _, a := range args {
			if a == "-readerCacheMode=sequential" {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected -readerCacheMode=sequential in args, got %v", args)
		}
	})
}
