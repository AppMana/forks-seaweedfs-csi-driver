package kubernetes_lab

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestOfflineMediaWindowsNames(t *testing.T) {
	for _, tool := range []string{"xorriso", "isoinfo", "bash"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s required for real ISO regression", tool)
		}
	}
	root := t.TempDir()
	input := filepath.Join(root, "inputs")
	if err := os.Mkdir(input, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"k0s", "k0s.exe", "mount-smoke.ps1", "linux-csi.tar", "linux-registrar.tar", "linux-provisioner.tar", "linux-attacher.tar", "linux-resizer.tar", "windows-csi.tar", "windows-registrar.tar", "windows-pause.tar", "windows-workload.tar"} {
		if err := os.WriteFile(filepath.Join(input, name), []byte("filename fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, joliet := range []bool{false, true} {
		name := "rockridge-only"
		if joliet {
			name = "windows-visible"
		}
		t.Run(name, func(t *testing.T) {
			iso := filepath.Join(root, name+".iso")
			args := []string{"-as", "mkisofs", "-R", "-V", "LCQUAL", "-o", iso}
			if joliet {
				args = append(args, "-J", "-joliet-long")
			}
			args = append(args, input)
			if out, err := exec.Command("xorriso", args...).CombinedOutput(); err != nil {
				t.Fatalf("ISO build: %v %s", err, out)
			}
			out, err := exec.Command("bash", "verify-media.sh", iso).CombinedOutput()
			if (err == nil) != joliet {
				t.Fatalf("joliet=%v error=%v output=%s", joliet, err, out)
			}
		})
	}
}
