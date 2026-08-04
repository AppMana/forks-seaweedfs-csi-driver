package driver

import (
	"slices"
	"strings"
	"testing"
)

func TestBuildMountArgsIncludesInitialCollectionQuota(t *testing.T) {
	mounter := &mountServiceMounter{
		driver:   &SeaweedFsDriver{},
		volumeID: "/buckets/pvc-1234",
		volContext: map[string]string{
			volumeCapacityKey: "5368709120",
		},
	}

	args, err := mounter.buildMountArgs(
		"/staging",
		"/cache",
		"/socket",
		[]string{"filer:8888"},
	)
	if err != nil {
		t.Fatalf("buildMountArgs: %v", err)
	}
	if !slices.Contains(args, "-collectionQuotaMB=5120") {
		t.Fatalf("mount args do not contain initial quota: %v", args)
	}
}

func TestInitialCollectionQuotaMBRoundsUp(t *testing.T) {
	if got, want := initialCollectionQuotaMB("1048577"), "2"; got != want {
		t.Fatalf("initialCollectionQuotaMB = %q, want %q", got, want)
	}
}

func TestInitialCollectionQuotaMBDisablesOneByteSentinel(t *testing.T) {
	if got := initialCollectionQuotaMB("1"); got != "" {
		t.Fatalf("initialCollectionQuotaMB = %q, want empty", got)
	}
}

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

func TestBuildMountArgsWinFspOptions(t *testing.T) {
	m := &mountServiceMounter{
		driver: &SeaweedFsDriver{
			ConcurrentReaders: 128,
			ConcurrentWriters: 128,
		},
		volumeID: "/buckets/test-volume",
		volContext: map[string]string{
			"winfspOptions": "FileInfoTimeout=60000,DirInfoTimeout=60000",
		},
	}

	args, err := m.buildMountArgs("/mnt/target", "/var/cache/seaweedfs/abc", "unix:///tmp/sock", []string{"localhost:8888"})
	if err != nil {
		t.Fatalf("buildMountArgs: %v", err)
	}

	want := "-winfspOptions=FileInfoTimeout=60000,DirInfoTimeout=60000"
	for _, arg := range args {
		if arg == want {
			return
		}
	}
	t.Fatalf("expected %q in args, got %v", want, args)
}
