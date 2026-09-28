package kubernetes_lab

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuntimePinChecker(t *testing.T) {
	const revision = "56e537c59dcb051ae6dba677a557db2483b6fefc"
	const version = "v0.2.0-alpha.2.0.20260923233400-56e537c59dcb"
	dir := t.TempDir()
	for name, body := range map[string]string{
		"go": `case "$1" in
list) printf '%s\n' "$TEST_SDK";;
version) printf '\tmod\tgithub.com/appmana/labcontainers\t%s\n\tbuild\tvcs.revision=%s\n\tbuild\tvcs.modified=%s\n' "$TEST_MODULE" "$TEST_REVISION" "$TEST_DIRTY";;
*) exit 9;; esac`,
		"docker": `for arg; do image=$arg; done
if [ "$image" = windows:fixture ]; then printf '%s %s\n' "$TEST_WINDOWS_REVISION" "$TEST_WINDOWS_HASH"; else printf '%s %s\n' "$TEST_REVISION" "$TEST_HELPER_HASH"; fi`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nset -eu\n"+body+"\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, key, value string
		allowed          bool
	}{
		{"matched", "", "", true},
		{"old daemon", "TEST_MODULE", "v0.2.0-alpha.2", false},
		{"dirty daemon", "TEST_DIRTY", "true", false},
		{"wrong daemon source", "TEST_REVISION", strings.Repeat("a", 40), false},
		{"replaced SDK", "TEST_SDK", "REPLACED", false},
		{"wrong Windows helper", "TEST_WINDOWS_REVISION", strings.Repeat("b", 40), false},
		{"different helper bytes", "TEST_WINDOWS_HASH", strings.Repeat("b", 64), false},
		{"missing helper hash", "TEST_HELPER_HASH", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			values := map[string]string{"TEST_SDK": version, "TEST_MODULE": version, "TEST_REVISION": revision,
				"TEST_DIRTY": "false", "TEST_WINDOWS_REVISION": revision, "TEST_HELPER_HASH": strings.Repeat("a", 64), "TEST_WINDOWS_HASH": strings.Repeat("a", 64)}
			if tc.key != "" {
				values[tc.key] = tc.value
			}
			cmd := exec.Command("bash", "verify-runtime.sh")
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "CALICO_LAB_MODULE="+dir,
				"LABCONTAINERS_LABD="+filepath.Join(dir, "go"), "LABCONTAINERS_VM_IMAGE=linux:fixture", "LABCONTAINERS_WINDOWS_IMAGE=windows:fixture")
			for key, value := range values {
				cmd.Env = append(cmd.Env, key+"="+value)
			}
			out, err := cmd.CombinedOutput()
			if (err == nil) != tc.allowed || strings.Contains(string(out), "CSI_RUNTIME_PINS_VERIFIED") != tc.allowed {
				t.Fatalf("allowed=%v error=%v output=%s", tc.allowed, err, out)
			}
		})
	}
}

func TestLauncherRejectsMismatchedRuntimeBeforeLaunch(t *testing.T) {
	repo, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	commands := map[string]string{
		"git":     `printf '%s\n' "$PIN_TEST_REPO"`,
		"isoinfo": `for f in k0s k0s.exe mount-smoke.ps1 linux-csi.tar linux-registrar.tar linux-provisioner.tar linux-attacher.tar linux-resizer.tar windows-csi.tar windows-registrar.tar windows-pause.tar windows-workload.tar; do printf '/%s\n' "$f"; done`,
		"go": `case "$1" in
list) printf 'v0.2.0-alpha.2.0.20260923233400-56e537c59dcb\n';;
version) printf '\tmod\tgithub.com/appmana/labcontainers\tv0.2.0-alpha.2\n\tbuild\tvcs.revision=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n\tbuild\tvcs.modified=false\n';;
test) if [ "$2" = '-c' ]; then printf 'fixture' > "$4"; else touch "$PIN_TEST_LAUNCHED"; printf 'CSI_QUALIFICATION_COMPLETE\n'; fi;;
*) exit 7;;
esac`,
		"docker": `printf 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n'`,
	}
	for name, body := range commands {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nset -eu\n"+body+"\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	marker := filepath.Join(dir, "launched")
	cmd := exec.Command("bash", "run.sh")
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "PIN_TEST_REPO="+repo,
		"PIN_TEST_LAUNCHED="+marker, "CALICO_LAB_MODULE="+dir, "CSI_LAB_ARTIFACTS="+filepath.Join(dir, "artifacts"),
		"LABCONTAINERS_STATE_DIR="+filepath.Join(dir, "state"),
		"LABCONTAINERS_CALICO_MEDIA="+filepath.Join(dir, "media.iso"), "LABCONTAINERS_CALICO_MEDIA_SHA256=fixture",
		"LABCONTAINERS_VM_IMAGE=linux:fixture", "LABCONTAINERS_WINDOWS_IMAGE=windows:fixture", "LABCONTAINERS_LABD="+filepath.Join(bin, "go"))
	out, runErr := cmd.CombinedOutput()
	if _, err := os.Stat(marker); !os.IsNotExist(err) || runErr == nil {
		t.Fatalf("mismatched daemon reached live launch: exit=%v marker=%v output=%s", runErr, err, out)
	}
}
