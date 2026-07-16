//go:build windows

package mountmanager

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/seaweedfs/seaweedfs/weed/glog"
	"golang.org/x/sys/windows"
)

// platformProcess holds per-OS process supervision state: the
// kill-on-close job object the weed mount process is assigned to, so
// weed.exe cannot outlive the mount server.
type platformProcess struct {
	job windows.Handle
}

// resolveWeedBinary normalizes the configured weed binary for Windows:
// it appends ".exe" when missing and, for relative paths inside a
// HostProcess container, roots the path at the container sandbox mount
// point where the image content is unpacked.
func resolveWeedBinary(binary string) string {
	if !strings.EqualFold(filepath.Ext(binary), ".exe") {
		binary += ".exe"
	}
	if filepath.IsAbs(binary) {
		return binary
	}
	if sandbox := os.Getenv("CONTAINER_SANDBOX_MOUNT_POINT"); sandbox != "" {
		return filepath.Join(sandbox, binary)
	}
	return binary
}

// defaultVolumePrefix is the WinFsp network-FS UNC prefix the supervisor
// defaults for spawned weed mounts. HCS cannot attach the container
// filters to LOCAL WinFsp volumes (winfsp/winfsp#498), so pods can only
// consume CSI volumes through the network-FS path; for the CSI
// supervisor (which exists only to serve pods) network mode is the only
// working mode and therefore the default. Override with the
// WEED_WINFSP_VOLUME_PREFIX env on the DaemonSet; set it to "local" to
// force local directory mounts (debugging only — pods will not start).
const defaultVolumePrefix = `\seaweedfs`

// configureCmd places the weed mount process in its own console process
// group so stop() can deliver CTRL_BREAK_EVENT to it (and only it). The
// Go runtime in weed.exe maps both CTRL_C_EVENT and CTRL_BREAK_EVENT to
// os.Interrupt, on which weed unmounts cleanly. It also defaults the
// WinFsp network-FS mode for the child (see defaultVolumePrefix).
func configureCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
	}
	prefix, set := os.LookupEnv("WEED_WINFSP_VOLUME_PREFIX")
	if !set {
		prefix = defaultVolumePrefix
	}
	if !strings.EqualFold(prefix, "local") {
		cmd.Env = append(os.Environ(), "WEED_WINFSP_VOLUME_PREFIX="+prefix)
	}
	// SEAWEEDFS_WINFSP_OPTIONS lets the DaemonSet tune WinFsp -o options
	// (FileInfoTimeout etc.) without an image rebuild.
	if opts := os.Getenv("SEAWEEDFS_WINFSP_OPTIONS"); opts != "" {
		cmd.Args = append(cmd.Args, "-winfspOptions="+opts)
	}
	cmd.Args = append(cmd.Args, winFspCaseSensitivityArgs(os.Getenv)...)
}

// afterStart assigns the freshly started weed mount process to a
// kill-on-close job object. If the mount server dies without running
// its shutdown path, the OS closes the job handle and terminates
// weed.exe, instead of leaving an orphaned WinFsp mount behind.
func (p *weedMountProcess) afterStart() error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("creating job object: %w", err)
	}

	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return fmt.Errorf("configuring job object: %w", err)
	}

	proc, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		uint32(p.cmd.Process.Pid),
	)
	if err != nil {
		_ = windows.CloseHandle(job)
		return fmt.Errorf("opening weed mount process: %w", err)
	}
	defer func() { _ = windows.CloseHandle(proc) }()

	if err := windows.AssignProcessToJobObject(job, proc); err != nil {
		_ = windows.CloseHandle(job)
		return fmt.Errorf("assigning weed mount process to job object: %w", err)
	}

	p.job = job
	return nil
}

// releaseProcessResources closes the job object handle after the weed
// mount process has exited. Closing it earlier would kill the process
// because of JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE.
func (p *weedMountProcess) releaseProcessResources() {
	if p.job != 0 {
		_ = windows.CloseHandle(p.job)
		p.job = 0
	}
}

// stop terminates the weed mount process: CTRL_BREAK_EVENT to the
// process group (the pid is a valid process-group id because the
// process was started with CREATE_NEW_PROCESS_GROUP), then Kill after a
// grace period. weed.exe treats the console event as os.Interrupt and
// unmounts cleanly.
func (p *weedMountProcess) stop() error {
	pid := p.cmd.Process.Pid
	if err := windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(pid)); err != nil {
		glog.Warningf("sending CTRL_BREAK_EVENT to weed mount (pid: %d) failed: %v", pid, err)
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
