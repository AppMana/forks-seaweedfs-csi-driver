package kubernetes_lab

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitTrustScript(root string) string {
	// Process-scoped protected configuration reaches Git and its LFS children.
	// Reset any inherited trust list, then allow exactly the disposable repo.
	repo := strings.ReplaceAll(root, `\`, "/") + "/git-lfs-temp-metadata"
	return `$env:GIT_CONFIG_COUNT='2'; $env:GIT_CONFIG_KEY_0='safe.directory'; $env:GIT_CONFIG_VALUE_0=''; $env:GIT_CONFIG_KEY_1='safe.directory'; $env:GIT_CONFIG_VALUE_1='` + strings.ReplaceAll(repo, "'", "''") + `'`
}

// Exercise Git's real ownership check locally, not a mocked success response.
// The CSI mount belongs to SYSTEM while the ordinary Windows pod runs as
// ContainerAdministrator. Trust only this run's repository, never all mounts.
func TestGitTrustIsRepositoryScoped(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("PowerShell required")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("Git required")
	}
	root := t.TempDir()
	repo := filepath.Join(root, "git-lfs-temp-metadata")
	other := filepath.Join(root, "unrelated")
	env := append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+filepath.Join(root, "absent-config"), "GIT_CONFIG_COUNT=0")
	for _, path := range []string{repo, other} {
		cmd := exec.Command("git", "init", path)
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("init: %v %s", err, out)
		}
	}
	env = append(env, "GIT_TEST_ASSUME_DIFFERENT_OWNER=1")
	check := exec.Command("git", "-C", repo, "rev-parse", "--git-dir")
	check.Env = env
	if out, err := check.CombinedOutput(); err == nil || !strings.Contains(string(out), "dubious ownership") {
		t.Fatalf("ownership reproduction not active: %v %s", err, out)
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	script := gitTrustScript(root) + "; & git -C " + quote(repo) + " rev-parse --git-dir; if($LASTEXITCODE -ne 0){throw 'intended repository rejected'}; & git -C " + quote(other) + " rev-parse --git-dir; if($LASTEXITCODE -eq 0){throw 'unrelated repository trusted'}; exit 0"
	args := ps(script)
	cmd := exec.Command(pwsh, args[1:]...)
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("scoped trust: %v %s", err, out)
	}
}
