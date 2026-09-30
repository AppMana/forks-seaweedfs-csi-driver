package kubernetes_lab

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Packaging changes must not silently restore an older, unqualified weed
// binary. This is a source contract, not a substitute for image qualification.
func TestMountBuildPinsAgree(t *testing.T) {
	read := func(path string) string {
		t.Helper()
		b, err := os.ReadFile("../../" + path)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	workflow := read(".github/workflows/build-images.yml")
	pins := map[string]string{
		"SEAWEEDFS_COMMIT":         "dd2b9ef98d38488121808765148306c365e341b1",
		"SEAWEEDFS_WINDOWS_COMMIT": "ce25e03a121b7b975df26a2c485e77f98ea139ca",
	}
	for key, want := range pins {
		match := regexp.MustCompile(`(?m)^  ` + key + `: ([0-9a-f]{40})$`).FindStringSubmatch(workflow)
		if len(match) != 2 || match[1] != want {
			t.Errorf("workflow %s must select source %s", key, want)
		}
	}
	if !strings.Contains(workflow, `--build-arg SEAWEEDFS_COMMIT="$SEAWEEDFS_WINDOWS_COMMIT"`) {
		t.Error("Windows source pin is not passed to the build")
	}
	for _, file := range []string{"Dockerfile", "Dockerfile.Windows", "Dockerfile.dev"} {
		t.Run(file, func(t *testing.T) {
			source := read("cmd/seaweedfs-mount/" + file)
			pin := pins["SEAWEEDFS_COMMIT"]
			if file == "Dockerfile.Windows" {
				pin = pins["SEAWEEDFS_WINDOWS_COMMIT"]
			}
			for _, required := range []string{
				"golang:1.26.0",
				"ARG SEAWEEDFS_COMMIT=" + pin,
				"ARG SEAWEEDFS_REPO=https://github.com/AppMana/forks-seaweedfs",
				"ARG GO_FUSE_COMMIT=1bdeec4d57d1e9ee85d4938f36f2ed876dd7bd5e",
				"git checkout ${GO_FUSE_COMMIT}",
				"-tags 5BytesOffset",
			} {
				if !strings.Contains(source, required) {
					t.Errorf("missing %q", required)
				}
			}
		})
	}
}
