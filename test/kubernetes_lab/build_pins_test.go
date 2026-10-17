package kubernetes_lab

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
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
		"SEAWEEDFS_COMMIT":         "a2dcf66436aa72ca53e4524f62d85875c458426a",
		"SEAWEEDFS_WINDOWS_COMMIT": "6a570503378e408c57c74f36efda148c5950595c",
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
			// The builder must satisfy the go directive of the weed source it
			// builds; the golang images run with GOTOOLCHAIN=local.
			builder := "golang:1.26.6-alpine"
			if file == "Dockerfile.Windows" {
				pin = pins["SEAWEEDFS_WINDOWS_COMMIT"]
				builder = "golang:1.26.6 "
			}
			for _, required := range []string{
				builder,
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

// Every image builder toolchain must satisfy the go directive of go.mod:
// the builds run with GOTOOLCHAIN=local, so an older image fails the build.
func TestBuilderGoSatisfiesGoMod(t *testing.T) {
	mod, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^go (\d+)\.(\d+)(?:\.(\d+))?$`).FindStringSubmatch(string(mod))
	if m == nil {
		t.Fatal("go.mod has no go directive")
	}
	want := [3]int{atoi(t, m[1]), atoi(t, m[2]), atoi(t, m[3])}
	files, err := filepath.Glob("../../cmd/*/Dockerfile*")
	if err != nil || len(files) == 0 {
		t.Fatalf("no Dockerfiles found: %v", err)
	}
	image := regexp.MustCompile(`golang:(\d+)\.(\d+)(?:\.(\d+))?`)
	for _, file := range files {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, tag := range image.FindAllStringSubmatch(string(b), -1) {
			got := [3]int{atoi(t, tag[1]), atoi(t, tag[2]), atoi(t, tag[3])}
			// A tag without a patch level is the newest patch release.
			older := got[0] < want[0] || got[0] == want[0] && (got[1] < want[1] || got[1] == want[1] && tag[3] != "" && got[2] < want[2])
			if older {
				t.Errorf("%s builds with %s, older than go %s in go.mod", strings.TrimPrefix(file, "../../"), tag[0], strings.TrimPrefix(m[0], "go "))
			}
		}
	}
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	if s == "" {
		return 0
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// The stock signed WinFsp driver cannot classify absolute symlinks into a
// directory-mounted volume; the released user-mode DLL from
// AppMana/forks-winfsp-fixes does. cgofuse loads winfsp-x64.dll from the
// executable's directory before the installed one, so the deployed (default)
// image must place exactly the pinned release next to weed.exe.
func TestWindowsImageShipsPinnedWinFspDLL(t *testing.T) {
	b, err := os.ReadFile("../../cmd/seaweedfs-mount/Dockerfile.Windows")
	if err != nil {
		t.Fatal(err)
	}
	source := string(b)
	for _, required := range []string{
		"ARG WINFSP_DLL_URL=https://github.com/AppMana/forks-winfsp-fixes/releases/download/appmana-dll-2.1-066843359/winfsp-x64.dll",
		"ARG WINFSP_DLL_SHA256=082f0cedc800a7ea29f726b9d3116358d20278c0de64f21ccb1c3e14d68c9d9e",
		`echo "${WINFSP_DLL_SHA256}  /out/winfsp-x64.dll" | sha256sum -c -`,
	} {
		if !strings.Contains(source, required) {
			t.Errorf("missing %q", required)
		}
	}
	stages := strings.Split(source, "\nFROM ")
	last := stages[len(stages)-1]
	if !strings.HasPrefix(last, "stock AS default") || !strings.Contains(last, "COPY --from=payload /out/winfsp-x64.dll /winfsp-x64.dll") {
		t.Fatalf("the default image must add the released DLL beside /weed.exe:\n%s", last)
	}
}
