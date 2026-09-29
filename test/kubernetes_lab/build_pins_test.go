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
	match := regexp.MustCompile(`(?m)^  SEAWEEDFS_COMMIT: ([0-9a-f]{40})$`).FindStringSubmatch(workflow)
	if len(match) != 2 {
		t.Fatal("workflow requires one full source revision")
	}
	if match[1] != "8ecd3e03f9fb7b4361cce12cd439520bfef00ca1" {
		t.Fatal("workflow does not select the memory-fix qualification source")
	}
	for _, file := range []string{"Dockerfile", "Dockerfile.Windows", "Dockerfile.dev"} {
		t.Run(file, func(t *testing.T) {
			source := read("cmd/seaweedfs-mount/" + file)
			for _, required := range []string{
				"golang:1.26.0",
				"ARG SEAWEEDFS_COMMIT=" + match[1],
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
