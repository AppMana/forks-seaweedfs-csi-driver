package kubernetes_lab

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestKubectlKeepsDiagnosticsOutOfCompletionEvidence(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("controller-side Unix transport")
	}
	for _, tc := range []struct {
		name, script, stdout string
		failure              bool
	}{
		{"interleaved-success", "printf 'CLIXML-without-newline' >&2\nsleep 0.05\nprintf 'CSI_NATIVE_COMPLETE:write\\n'", "CSI_NATIVE_COMPLETE:write\n", false},
		{"stderr-is-not-proof", "printf 'CSI_NATIVE_COMPLETE:write\\n' >&2", "", false},
		{"failure-retains-diagnostic", "printf 'partial-output\\n'\nprintf 'installer failed' >&2\nexit 17", "partial-output\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "k0s"), []byte("#!/bin/sh\n"+tc.script+"\n"), 0755); err != nil {
				t.Fatal(err)
			}
			out, err := kubectlOutput(exec.Command(filepath.Join(dir, "k0s"), "kubectl", "exec", "fixture"))
			if string(out) != tc.stdout || (err != nil) != tc.failure {
				t.Fatalf("stdout=%q want=%q err=%v", out, tc.stdout, err)
			}
			if tc.failure && !strings.Contains(err.Error(), "installer failed") {
				t.Fatal("lost failure diagnostic", err)
			}
		})
	}
}
