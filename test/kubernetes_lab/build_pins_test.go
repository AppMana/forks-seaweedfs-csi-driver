package kubernetes_lab

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestBuildImagesNeverOverwriteReleasedTags(t *testing.T) {
	b, err := os.ReadFile("../../.github/workflows/build-images.yml")
	if err != nil {
		t.Fatal(err)
	}
	want := "  IMAGE_TAG: candidate-${{ github.sha }}-${{ github.run_id }}-${{ github.run_attempt }}"
	if !strings.Contains(string(b), want) {
		t.Fatal("builds must use source/run/attempt-specific candidate tags, not overwrite deployed release tags")
	}
}

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
		"SEAWEEDFS_COMMIT":         "0f3d7bec5208503e974c7077fb60c6df407acca4",
		"SEAWEEDFS_WINDOWS_COMMIT": "4cc1474e2b287245f53c9fa5e2c94822e09ce5a7",
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
				"ARG GO_FUSE_COMMIT=6ead27e20708423a00718812de796241659ad387",
				"git checkout ${GO_FUSE_COMMIT}",
				"-tags 5BytesOffset",
			} {
				if !strings.Contains(source, required) {
					t.Errorf("missing %q", required)
				}
			}
			if file == "Dockerfile.Windows" {
				return
			}
			// The fork's release recipe: static, and stamped with the exact
			// source so `weed version` names what is running.
			for _, required := range []string{
				"CGO_ENABLED=0",
				"version.COMMIT=${SEAWEEDFS_COMMIT}",
			} {
				if !strings.Contains(source, required) {
					t.Errorf("missing %q", required)
				}
			}
		})
	}
}
