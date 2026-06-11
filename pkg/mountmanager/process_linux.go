//go:build linux

package mountmanager

import (
	"errors"
	"os/exec"
	"syscall"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/glog"
)

// platformProcess holds per-OS process supervision state. There is none
// on Linux.
type platformProcess struct{}

// resolveWeedBinary returns the weed binary path as configured.
func resolveWeedBinary(binary string) string {
	return binary
}

// configureCmd applies per-OS process attributes before the weed mount
// process is started. No-op on Linux.
func configureCmd(cmd *exec.Cmd) {}

// afterStart performs per-OS supervision setup once the weed mount
// process is running. No-op on Linux.
func (p *weedMountProcess) afterStart() error {
	return nil
}

// releaseProcessResources releases per-OS supervision resources after
// the weed mount process has exited. No-op on Linux.
func (p *weedMountProcess) releaseProcessResources() {}

// stop terminates the weed mount process: SIGTERM, then Kill after a
// grace period.
func (p *weedMountProcess) stop() error {
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		glog.Warningf("sending SIGTERM to weed mount failed: %v", err)
	}

	select {
	case <-p.done:
		return nil
	case <-time.After(5 * time.Second):
	}

	if err := p.cmd.Process.Kill(); err != nil {
		glog.Warningf("killing weed mount failed: %v", err)
	}

	select {
	case <-p.done:
		return nil
	case <-time.After(1 * time.Second):
		return errors.New("timed out waiting for weed mount to stop")
	}
}
