package driver

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/seaweedfs/seaweedfs-csi-driver/pkg/k8s"
	"github.com/seaweedfs/seaweedfs-csi-driver/pkg/mountmanager"
)

func readMountRoots(t *testing.T, file string) map[string][][]containerMount {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var roots mountRootsFile
	if err := json.Unmarshal(data, &roots); err != nil {
		t.Fatal(err)
	}
	return roots.Publishes
}

func TestPublishRecordsWherePodsMountTheVolume(t *testing.T) {
	socketDir := t.TempDir()
	lookups := map[string][][]k8s.VolumeMount{
		"ci/a": {{{Path: `C:\data`}}},
		"ci/b": {{{Path: `C:\data`, SubPath: "sub"}}, {{Path: `C:\other`}}},
	}
	ns := &NodeServer{
		Driver: &SeaweedFsDriver{volumeSocketDir: socketDir},
		podVolumeMountsFn: func(ctx context.Context, volumeID, namespace, name string) ([][]k8s.VolumeMount, error) {
			if volumeID != "/buckets/pvc-1" {
				t.Fatalf("volume %q", volumeID)
			}
			if mounts, ok := lookups[namespace+"/"+name]; ok {
				return mounts, nil
			}
			return nil, errors.New("pod not found")
		},
	}
	pod := func(name string) map[string]string {
		return map[string]string{podNameKey: name, podNamespaceKey: "ci"}
	}
	file := mountmanager.MountRootsPath(socketDir, "/buckets/pvc-1")

	ns.recordMountRoots(context.Background(), "/buckets/pvc-1", `c:\pods\a\mount`, pod("a"))
	ns.recordMountRoots(context.Background(), "/buckets/pvc-1", `c:\pods\b\mount`, pod("b"))
	want := map[string][][]containerMount{
		`c:\pods\a\mount`: {{{Path: `C:\data`}}},
		`c:\pods\b\mount`: {{{Path: `C:\data`, SubPath: "sub"}}, {{Path: `C:\other`}}},
	}
	if got := readMountRoots(t, file); !reflect.DeepEqual(got, want) {
		t.Fatalf("roots = %#v, want %#v", got, want)
	}

	// An unknown pod cannot be ruled out as a creator, so it records a
	// container without mounts, which the mount treats as "never map".
	ns.recordMountRoots(context.Background(), "/buckets/pvc-1", `c:\pods\c\mount`, pod("c"))
	ns.recordMountRoots(context.Background(), "/buckets/pvc-1", `c:\pods\d\mount`, nil)
	got := readMountRoots(t, file)
	for _, target := range []string{`c:\pods\c\mount`, `c:\pods\d\mount`} {
		if views := got[target]; len(views) != 1 || len(views[0]) != 0 {
			t.Fatalf("%s must record one container without mounts: %#v", target, views)
		}
	}

	for _, target := range []string{`c:\pods\a\mount`, `c:\pods\c\mount`, `c:\pods\d\mount`} {
		ns.forgetMountRoots("/buckets/pvc-1", target)
	}
	if got := readMountRoots(t, file); !reflect.DeepEqual(got, map[string][][]containerMount{`c:\pods\b\mount`: want[`c:\pods\b\mount`]}) {
		t.Fatalf("after unpublish roots = %#v", got)
	}
	ns.forgetMountRoots("/buckets/pvc-1", `c:\pods\b\mount`)
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("the last unpublish must remove the roots file: %v", err)
	}
}

func TestMountRootsAreOffWithoutPodLookup(t *testing.T) {
	socketDir := t.TempDir()
	ns := &NodeServer{Driver: &SeaweedFsDriver{volumeSocketDir: socketDir}}
	ns.recordMountRoots(context.Background(), "/buckets/pvc-1", `/pods/a/mount`, map[string]string{podNameKey: "a", podNamespaceKey: "ci"})
	if _, err := os.Stat(mountmanager.MountRootsPath(socketDir, "/buckets/pvc-1")); !os.IsNotExist(err) {
		t.Fatalf("no roots file without a pod lookup: %v", err)
	}
}

func TestMountArgsNameTheRootsFileOnlyWhereRecorded(t *testing.T) {
	args := strings.Join(platformMountArgs(`C:\sockets`, "/buckets/pvc-1"), " ")
	if mountRootsRecorded != strings.Contains(args, "-winfspMountRoots="+mountmanager.MountRootsPath(`C:\sockets`, "/buckets/pvc-1")) {
		t.Fatalf("mount args %q (roots recorded: %v)", args, mountRootsRecorded)
	}
	if err := ValidateMountArgs([]string{"-winfspMountRoots=x"}); err == nil {
		t.Fatal("winfspMountRoots is managed by the CSI driver")
	}
}
