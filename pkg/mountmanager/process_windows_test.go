//go:build windows

package mountmanager

import (
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestConfigureCmdSetsProcessGroup verifies that the weed mount process
// is started in its own console process group, which is what makes its
// pid usable as a process-group id for GenerateConsoleCtrlEvent.
func TestConfigureCmdSetsProcessGroup(t *testing.T) {
	cmd := exec.Command("cmd")
	configureCmd(cmd)
	if cmd.SysProcAttr == nil {
		t.Fatal("configureCmd did not set SysProcAttr")
	}
	if cmd.SysProcAttr.CreationFlags&syscall.CREATE_NEW_PROCESS_GROUP == 0 {
		t.Fatalf("CreationFlags %#x missing CREATE_NEW_PROCESS_GROUP", cmd.SysProcAttr.CreationFlags)
	}
}

// TestStopKillsChild verifies the supervisor stop path against a real
// child process: CTRL_BREAK_EVENT first, Kill as fallback, all within
// the documented timeouts.
func TestStopKillsChild(t *testing.T) {
	cmd := exec.Command("powershell", "-NoProfile", "-Command", "Start-Sleep -Seconds 60")
	configureCmd(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting child: %v", err)
	}

	p := &weedMountProcess{
		cmd:    cmd,
		target: filepath.Join(t.TempDir(), "nonexistent-target"),
		exited: make(chan struct{}),
		done:   make(chan struct{}),
	}

	if err := p.afterStart(); err != nil {
		t.Fatalf("afterStart (job object setup): %v", err)
	}

	go p.wait()

	start := time.Now()
	if err := p.stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	elapsed := time.Since(start)

	// stop allows 5s for graceful CTRL_BREAK + 1s after Kill; anything
	// past that means the child survived.
	if elapsed > 10*time.Second {
		t.Fatalf("stop took %v, expected to finish within the supervisor timeouts", elapsed)
	}

	select {
	case <-p.done:
	case <-time.After(2 * time.Second):
		t.Fatal("process wait() did not complete after stop")
	}
}
