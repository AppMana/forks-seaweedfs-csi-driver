package kubernetes_lab

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLauncherSelectsCandidateBundle(t *testing.T) {
	dir := t.TempDir()
	for _, sub := range []string{"bin", "test/kubernetes_lab"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range map[string]string{
		"bin/git":                               `printf '%s\n' "$CNI_TEST_ROOT"`,
		"bin/go":                                `if [ "$2" = '-c' ]; then printf 'fixture' > "$4"; else printf '%s' "$LABCONTAINERS_KUBERNETES_WORKLOAD_ARGS" > "$CNI_TEST_ROOT/args.json"; printf '%s\n' "$LABCONTAINERS_KUBERNETES_WORKLOAD_SUCCESS"; fi`,
		"test/kubernetes_lab/verify-runtime.sh": "exit 0",
		"test/kubernetes_lab/verify-media.sh":   "exit 0",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nset -eu\n"+body+"\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	// Stub only bundle validation here; its fail-closed behavior has executable
	// Python tests. Exercise the real shell launch path and success-marker gate.
	body := `import json, sys
assert sys.argv[1:] == ['--launch-bundle', 'candidate-bundle', '--cni', 'calico-bgp']
print(json.dumps({'args':['-test.run=^TestCSICandidateWinFsp$', '-csi-cni=calico-bgp'], 'success':'CSI_CANDIDATE_DRIVER_QUALIFICATION_COMPLETE'}))
`
	if err := os.WriteFile(filepath.Join(dir, "test/kubernetes_lab/stage_split_media.py"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "run.sh")
	cmd.Env = append(os.Environ(), "PATH="+filepath.Join(dir, "bin")+":"+os.Getenv("PATH"), "CNI_TEST_ROOT="+dir,
		"CALICO_LAB_MODULE="+dir, "CSI_LAB_ARTIFACTS="+filepath.Join(dir, "artifacts"), "LABCONTAINERS_STATE_DIR="+filepath.Join(dir, "state"),
		"LABCONTAINERS_CALICO_MEDIA=fixture", "LABCONTAINERS_CALICO_MEDIA_SHA256=fixture", "LABCONTAINERS_LABD=fixture",
		"LABCONTAINERS_VM_IMAGE=fixture", "LABCONTAINERS_WINDOWS_IMAGE=fixture", "LABCONTAINERS_KUBERNETES_CNI=calico-bgp",
		"CSI_CANDIDATE_BUNDLE=candidate-bundle")
	out, err := cmd.CombinedOutput()
	data, readErr := os.ReadFile(filepath.Join(dir, "args.json"))
	if err != nil || readErr != nil || !strings.Contains(string(data), "^TestCSICandidateWinFsp$") || strings.Contains(string(data), "TestCSIStockWinFsp") {
		t.Fatalf("candidate bundle selected wrong consumer: %v %v args=%s output=%s", err, readErr, data, out)
	}
}

func TestLauncherForwardsExplicitCNI(t *testing.T) {
	for _, cni := range []string{"calico-vxlan", "calico-bgp", "bogus"} {
		t.Run(cni, func(t *testing.T) {
			dir := t.TempDir()
			for _, sub := range []string{"bin", "test/kubernetes_lab"} {
				if err := os.MkdirAll(filepath.Join(dir, sub), 0700); err != nil {
					t.Fatal(err)
				}
			}
			for name, body := range map[string]string{
				"bin/git":                               `printf '%s\n' "$CNI_TEST_ROOT"`,
				"bin/go":                                `if [ "$2" = '-c' ]; then printf 'fixture' > "$4"; else printf '%s' "$LABCONTAINERS_KUBERNETES_WORKLOAD_ARGS" > "$CNI_TEST_ROOT/args.json"; printf 'CSI_QUALIFICATION_COMPLETE\n'; fi`,
				"test/kubernetes_lab/verify-runtime.sh": "exit 0",
				"test/kubernetes_lab/verify-media.sh":   "exit 0",
			} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nset -eu\n"+body+"\n"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command("bash", "run.sh")
			cmd.Env = append(os.Environ(), "PATH="+filepath.Join(dir, "bin")+":"+os.Getenv("PATH"), "CNI_TEST_ROOT="+dir,
				"CALICO_LAB_MODULE="+dir, "CSI_LAB_ARTIFACTS="+filepath.Join(dir, "artifacts"), "LABCONTAINERS_STATE_DIR="+filepath.Join(dir, "state"),
				"LABCONTAINERS_CALICO_MEDIA=fixture", "LABCONTAINERS_CALICO_MEDIA_SHA256=fixture", "LABCONTAINERS_LABD=fixture",
				"LABCONTAINERS_VM_IMAGE=fixture", "LABCONTAINERS_WINDOWS_IMAGE=fixture", "LABCONTAINERS_KUBERNETES_CNI="+cni)
			out, err := cmd.CombinedOutput()
			data, readErr := os.ReadFile(filepath.Join(dir, "args.json"))
			if cni == "bogus" {
				if err == nil || !os.IsNotExist(readErr) {
					t.Fatalf("invalid CNI reached launch: %v %v %s", err, readErr, out)
				}
				return
			}
			var args []string
			if err != nil || readErr != nil || json.Unmarshal(data, &args) != nil {
				t.Fatalf("launch failed: %v %v %s", err, readErr, out)
			}
			found := false
			for _, arg := range args {
				if arg == "-csi-cni="+cni {
					found = true
				}
			}
			if !found {
				t.Fatalf("selected CNI %s not forwarded to CSI oracle: %s", cni, data)
			}
		})
	}
}

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

func TestLauncherRecoveryAndDLLPins(t *testing.T) {
	for _, tc := range []struct {
		name, mode, dll, native, candidate string
		allowed                            bool
	}{
		{"recovery DLL", "recovery", strings.Repeat("a", 64), strings.Repeat("b", 64), "", true},
		{"stock recovery", "recovery", "", "", "", true},
		{"partial DLL pins", "recovery", strings.Repeat("a", 64), "", "", false},
		{"bad DLL pin", "recovery", "bad", strings.Repeat("b", 64), "", false},
		{"kernel bundle conflict", "recovery", "", "", "candidate", false},
		{"unknown mode", "shortcut", "", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, sub := range []string{"bin", "test/kubernetes_lab"} {
				if err := os.MkdirAll(filepath.Join(dir, sub), 0700); err != nil {
					t.Fatal(err)
				}
			}
			for name, body := range map[string]string{
				"bin/git":                               `printf '%s\n' "$CNI_TEST_ROOT"`,
				"bin/go":                                `if [ "$2" = '-c' ]; then printf fixture > "$4"; else printf '%s' "$LABCONTAINERS_KUBERNETES_WORKLOAD_ARGS" > "$CNI_TEST_ROOT/args.json"; printf '%s\n' "$LABCONTAINERS_KUBERNETES_WORKLOAD_SUCCESS"; printf 'RETAINED_KUBERNETES_CRASH_CONSUMER_COMPLETE\n'; fi`,
				"test/kubernetes_lab/verify-runtime.sh": "exit 0",
				"test/kubernetes_lab/verify-media.sh":   "exit 0",
			} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nset -eu\n"+body+"\n"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command("bash", "run.sh")
			cmd.Env = append(os.Environ(), "PATH="+filepath.Join(dir, "bin")+":"+os.Getenv("PATH"), "CNI_TEST_ROOT="+dir,
				"CALICO_LAB_MODULE="+dir, "CSI_LAB_ARTIFACTS="+filepath.Join(dir, "artifacts"), "LABCONTAINERS_STATE_DIR="+filepath.Join(dir, "state"),
				"LABCONTAINERS_CALICO_MEDIA=fixture", "LABCONTAINERS_CALICO_MEDIA_SHA256=fixture", "LABCONTAINERS_LABD=fixture",
				"LABCONTAINERS_VM_IMAGE=fixture", "LABCONTAINERS_WINDOWS_IMAGE=fixture", "LABCONTAINERS_KUBERNETES_CNI=calico-bgp",
				"CSI_QUALIFICATION_MODE="+tc.mode, "CSI_WINFSP_DLL_SHA256="+tc.dll, "CSI_NATIVE_TEST_SHA256="+tc.native,
				"CSI_CANDIDATE_BUNDLE="+tc.candidate, "LABCONTAINERS_KUBERNETES_CRASH_VERIFY=1")
			for _, key := range []string{"CSI_DRIVER_LINUX_IMAGE", "CSI_DRIVER_WINDOWS_IMAGE", "CSI_MOUNT_LINUX_IMAGE", "CSI_MOUNT_WINDOWS_IMAGE"} {
				cmd.Env = append(cmd.Env, key+"=example.test/image@sha256:"+strings.Repeat("c", 64))
			}
			out, err := cmd.CombinedOutput()
			data, readErr := os.ReadFile(filepath.Join(dir, "args.json"))
			if !tc.allowed {
				if err == nil || !os.IsNotExist(readErr) {
					t.Fatalf("invalid configuration launched: %v %s", err, out)
				}
				return
			}
			var args []string
			if err != nil || readErr != nil || json.Unmarshal(data, &args) != nil {
				t.Fatalf("launch: %v %v %s", err, readErr, out)
			}
			for _, required := range []string{"-test.run=^TestCSIRecoveryQualification$", "-csi-emit-crash-plan"} {
				if !strings.Contains(string(data), required) {
					t.Fatalf("missing %s: %s", required, data)
				}
			}
			if tc.dll != "" && (!strings.Contains(string(data), "-csi-winfsp-dll-sha256="+tc.dll) || !strings.Contains(string(data), "-csi-candidate-native-test-sha256="+tc.native)) {
				t.Fatalf("lost DLL/native pins: %s", data)
			}
			if !strings.Contains(string(out), "CSI_RECOVERY_QUALIFICATION_COMPLETE") || strings.Contains(string(out), "CSI_QUALIFICATION_COMPLETE") {
				t.Fatalf("wrong qualification claim: %s", out)
			}
		})
	}
}
