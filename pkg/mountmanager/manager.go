package mountmanager

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/glog"
)

// Manager owns weed mount processes and exposes helpers to start and stop them.
type Manager struct {
	weedBinary string

	// procRoot is where process file descriptors are read from.
	procRoot string

	mu     sync.Mutex
	mounts map[string]*mountEntry
	locks  *keyMutex

	// flags caches which flags weedBinary's mount command defines; the
	// binary does not change while the mount service runs.
	flagsMu sync.Mutex
	flags   *weedMountFlags
}

// Config configures a Manager instance.
type Config struct {
	WeedBinary string
}

// NewManager returns a Manager ready to accept mount requests.
func NewManager(cfg Config) *Manager {
	binary := cfg.WeedBinary
	if binary == "" {
		binary = DefaultWeedBinary
	}
	binary = resolveWeedBinary(binary)
	return &Manager{
		weedBinary: binary,
		procRoot:   "/proc",
		mounts:     make(map[string]*mountEntry),
		locks:      newKeyMutex(),
	}
}

// mountFlags returns the flags weed mount defines, or nil if they cannot be
// read, in which case the mount arguments pass through unfiltered and a
// weed mount that rejects one fails with its own usage error.
func (m *Manager) mountFlags() *weedMountFlags {
	m.flagsMu.Lock()
	defer m.flagsMu.Unlock()
	if m.flags != nil {
		return m.flags
	}
	flags, err := probeWeedMountFlags(m.weedBinary)
	if err != nil {
		glog.Warningf("cannot read the flags weed mount defines; passing mount arguments through unfiltered: %v", err)
		return nil
	}
	glog.Infof("weed mount (%s) defines %d flags", flags.version, len(flags.defined))
	m.flags = flags
	return flags
}

// Mount starts a weed mount process using the provided request.
func (m *Manager) Mount(req *MountRequest) (*MountResponse, error) {
	if req == nil {
		return nil, errors.New("mount request is nil")
	}
	if err := validateMountRequest(req); err != nil {
		return nil, err
	}

	lock := m.locks.get(req.VolumeID)
	lock.Lock()
	defer lock.Unlock()

	if entry := m.getMount(req.VolumeID); entry != nil {
		// If the previous weed mount process has died, the entry is
		// stale and the FUSE mount is dead. Tear down the stale entry
		// so we can start a fresh process; otherwise we would falsely
		// report "already mounted" and a CSI-driver recovery would
		// silently bind-mount onto a dead path. See seaweedfs/seaweedfs-csi-driver#261.
		select {
		case <-entry.process.exited:
			glog.Warningf("volume %s previous weed mount process has exited; replacing stale entry with a fresh mount", req.VolumeID)
			// Wait for wait()'s post-exit cleanup (FUSE unmount) to finish
			// so ensureTargetClean below sees a quiescent path.
			<-entry.process.done
			m.removeMount(req.VolumeID)
		default:
			if entry.targetPath == req.TargetPath {
				glog.Infof("volume %s already mounted at %s", req.VolumeID, req.TargetPath)
				return &MountResponse{LocalSocket: entry.localSocket}, nil
			}
			return nil, fmt.Errorf("volume %s already mounted at %s", req.VolumeID, entry.targetPath)
		}
	}

	entry, err := m.startMount(req)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	m.mounts[req.VolumeID] = entry
	m.mu.Unlock()

	// Proactively clear the entry once the weed mount process exits,
	// even if Unmount is never called. Without this, a process that
	// crashes on its own (e.g. backend errors) leaves a stale entry
	// that fools later Mount calls into reporting success without
	// starting a new process.
	go m.watchProcessExit(req.VolumeID, entry)

	glog.Infof("started weed mount process for volume %s at %s", req.VolumeID, req.TargetPath)
	return &MountResponse{LocalSocket: entry.localSocket}, nil
}

// watchProcessExit removes the entry for volumeID once its weed mount
// process has finished exiting. It is safe to run concurrently with
// Unmount: if Unmount has already removed the entry (or replaced it
// with a fresh mount), the identity check ensures we leave the new
// entry alone.
func (m *Manager) watchProcessExit(volumeID string, entry *mountEntry) {
	<-entry.process.done
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.mounts[volumeID]; ok && existing == entry {
		delete(m.mounts, volumeID)
		// See removeMount: do not delete the per-volume lock — a
		// concurrent Mount/Unmount may still be holding it.
		glog.Infof("removed mount entry for volume %s after weed mount process exited (target: %s)", volumeID, entry.targetPath)
	}
}

// Unmount terminates the weed mount process associated with the provided request.
func (m *Manager) Unmount(req *UnmountRequest) (*UnmountResponse, error) {
	if req == nil {
		return nil, errors.New("unmount request is nil")
	}
	if req.VolumeID == "" {
		return nil, errors.New("volumeId is required")
	}

	lock := m.locks.get(req.VolumeID)
	lock.Lock()
	defer lock.Unlock()

	// Use getMount first to check if mounted, only remove from state after cleanup succeeds
	entry := m.getMount(req.VolumeID)
	if entry == nil {
		glog.Infof("volume %s not mounted", req.VolumeID)
		return &UnmountResponse{}, nil
	}

	// Note: We don't explicitly unmount here because weedMountProcess.wait()
	// handles the unmount when the process terminates (either gracefully or forcefully).
	// This centralizes unmount logic and avoids potential race conditions.
	if err := entry.process.stop(); err != nil {
		return nil, err
	}

	// Remove cache dir only after process has been successfully stopped
	if err := os.RemoveAll(entry.cacheDir); err != nil {
		glog.Warningf("failed to remove cache dir %s for volume %s: %v", entry.cacheDir, req.VolumeID, err)
	}

	// Only remove from state after all cleanup operations succeeded
	m.removeMount(req.VolumeID)

	glog.Infof("stopped weed mount process for volume %s at %s", req.VolumeID, entry.targetPath)
	return &UnmountResponse{}, nil
}

func (m *Manager) getMount(volumeID string) *mountEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mounts[volumeID]
}

func (m *Manager) removeMount(volumeID string) *mountEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry := m.mounts[volumeID]
	delete(m.mounts, volumeID)
	// Intentionally do NOT delete the per-volume lock here. If a caller
	// is still holding the lock from m.locks.get(volumeID), deleting it
	// would let a concurrent caller receive a brand-new lock from the
	// next m.locks.get() and enter the critical section in parallel —
	// splitting Mount/Unmount serialization for the same volume. The
	// lock map grows by one entry per ever-mounted volume, which is
	// negligible for the manager's lifetime.
	return entry
}

func (m *Manager) startMount(req *MountRequest) (*mountEntry, error) {
	args := req.MountArgs
	if len(args) == 0 {
		return nil, errors.New("mountArgs is required")
	}
	// The node plugin assembles the arguments and may be a newer build than
	// this service's weed: drop optional flags weed does not define and
	// refuse required ones, before touching the target.
	if flags := m.mountFlags(); flags != nil {
		filtered, err := filterMountArgs(req.VolumeID, args, flags, req.RequiredFlags)
		if err != nil {
			return nil, err
		}
		args = filtered
	}

	targetPath := req.TargetPath
	if err := ensureTargetClean(targetPath); err != nil {
		return nil, err
	}

	cacheDir := req.CacheDir
	if cacheDir == "" {
		return nil, errors.New("cacheDir is required")
	}
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return nil, fmt.Errorf("creating cache dir: %w", err)
	}

	localSocket := req.LocalSocket
	if localSocket == "" {
		return nil, errors.New("localSocket is required")
	}

	process, err := startWeedMount(m.weedBinary, args, targetPath, req.VolumeID)
	if err != nil {
		return nil, err
	}
	removeUnheldInstanceCaches(cacheDir, m.procRoot)

	return &mountEntry{
		volumeID:    req.VolumeID,
		targetPath:  targetPath,
		cacheDir:    cacheDir,
		localSocket: localSocket,
		process:     process,
	}, nil
}

func validateMountRequest(req *MountRequest) error {
	if req.VolumeID == "" {
		return errors.New("volumeId is required")
	}
	if req.TargetPath == "" {
		return errors.New("targetPath is required")
	}
	if req.CacheDir == "" {
		return errors.New("cacheDir is required")
	}
	if req.LocalSocket == "" {
		return errors.New("localSocket is required")
	}
	if len(req.MountArgs) == 0 {
		return errors.New("mountArgs is required")
	}
	return nil
}

type mountEntry struct {
	volumeID    string
	targetPath  string
	cacheDir    string
	localSocket string
	process     *weedMountProcess
}

type weedMountProcess struct {
	cmd    *exec.Cmd
	target string
	// exited is closed as soon as cmd.Wait() returns, so callers can
	// detect that the weed mount process is gone without waiting for
	// the post-exit FUSE unmount step.
	exited chan struct{}
	// done is closed after wait() finishes its full cleanup (including
	// the cleanup of the dead mount point at the target).
	done chan struct{}

	// output logs the process's stdout and stderr and keeps their first
	// and last lines; exitErr is cmd.Wait's result. Both are complete once
	// exited is closed.
	output  *processOutput
	stdout  *lineWriter
	stderr  *lineWriter
	exitErr error

	// platformProcess holds per-OS supervision state (e.g. the Windows
	// job object handle). It is a zero-size struct on Linux.
	platformProcess
}

var startWeedMount = startWeedMountProcess

// mountWaitTimeout bounds how long a running weed mount may take to bring
// the mount up.
const mountWaitTimeout = 10 * time.Second

// outputWaitDelay bounds how long Wait keeps reading output after weed
// mount exits, should anything it started still hold its stdout or stderr.
const outputWaitDelay = 2 * time.Second

func startWeedMountProcess(command string, args []string, target string, volumeID string) (*weedMountProcess, error) {
	cmd := exec.Command(command, args...)
	configureCmd(cmd)

	// Log stdout/stderr with the volume ID prefix, and keep the lines that
	// explain an exit before the mount comes up.
	output := &processOutput{volumeID: volumeID}
	stdout := &lineWriter{output: output, stream: "stdout"}
	stderr := &lineWriter{output: output, stream: "stderr"}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.WaitDelay = outputWaitDelay

	glog.V(0).Infof("[%s] Starting weed mount: %s %s", volumeID, command, strings.Join(args, " "))

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting weed mount: %w", err)
	}

	process := &weedMountProcess{
		cmd:    cmd,
		target: target,
		exited: make(chan struct{}),
		done:   make(chan struct{}),
		output: output,
		stdout: stdout,
		stderr: stderr,
	}

	if err := process.afterStart(); err != nil {
		glog.Warningf("[%s] post-start process supervision setup failed: %v", volumeID, err)
	}

	go process.wait()

	if err := waitForMount(target, mountWaitTimeout, process.exited); err != nil {
		if errors.Is(err, errProcessExited) {
			// Report the exit now rather than waiting out the timeout;
			// wait() is already cleaning up the target.
			<-process.done
			return nil, process.exitedBeforeMount()
		}
		if stopErr := process.stop(); stopErr != nil {
			glog.Warningf("[%s] failed to stop mount process after mount wait failure: %v", volumeID, stopErr)
		}
		return nil, err
	}

	return process, nil
}

// errProcessExited is returned by waitForMount when the weed mount process
// exits before the mount comes up.
var errProcessExited = errors.New("weed mount process exited")

// exitedBeforeMount describes a weed mount process that exited before its
// mount came up: its exit status and the first and last lines it printed.
func (p *weedMountProcess) exitedBeforeMount() error {
	status := "exit status 0"
	if p.exitErr != nil {
		status = p.exitErr.Error()
	}
	return fmt.Errorf("weed mount exited before mounting %s (%s): %s", p.target, status, p.output.summary())
}

func (p *weedMountProcess) wait() {
	err := p.cmd.Wait()
	p.stdout.flush()
	p.stderr.flush()
	p.exitErr = err
	if err != nil {
		glog.Errorf("weed mount exit (pid: %d, target: %s): %v", p.cmd.Process.Pid, p.target, err)
	} else {
		glog.Infof("weed mount exit (pid: %d, target: %s)", p.cmd.Process.Pid, p.target)
	}

	// Release per-OS supervision resources now that the process is gone.
	p.releaseProcessResources()

	// Signal exit immediately so Manager.Mount can detect a dead
	// process without waiting for the post-exit unmount step.
	close(p.exited)

	// Brief delay to allow FUSE cleanup and pending I/O to complete before unmounting
	time.Sleep(100 * time.Millisecond)
	cleanupDeadMountPoint(p.target)

	close(p.done)
}

// outputKeepLines is how many of the first and of the last output lines
// processOutput keeps: the first carry a usage error or a fatal log line,
// the last a panic or the final error.
const outputKeepLines = 5

// maxLineBytes splits an output line that never ends.
const maxLineBytes = 64 * 1024

// processOutput logs a weed mount's output with its volume ID and keeps
// its first and last lines.
type processOutput struct {
	volumeID string

	mu    sync.Mutex
	head  []string
	tail  []string
	lines int
}

func (o *processOutput) add(stream, line string) {
	glog.Infof("[%s] %s: %s", o.volumeID, stream, line)
	o.mu.Lock()
	defer o.mu.Unlock()
	o.lines++
	if len(o.head) < outputKeepLines {
		o.head = append(o.head, line)
		return
	}
	o.tail = append(o.tail, line)
	if len(o.tail) > outputKeepLines {
		o.tail = o.tail[1:]
	}
}

func (o *processOutput) summary() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.lines == 0 {
		return "no output"
	}
	head := strings.Join(o.head, "\n")
	if len(o.tail) == 0 {
		return head
	}
	tail := strings.Join(o.tail, "\n")
	if omitted := o.lines - len(o.head) - len(o.tail); omitted > 0 {
		return fmt.Sprintf("%s\n... (%d lines omitted) ...\n%s", head, omitted, tail)
	}
	return head + "\n" + tail
}

// lineWriter splits one output stream into lines for processOutput.
type lineWriter struct {
	output *processOutput
	stream string

	mu  sync.Mutex
	buf []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			if len(w.buf) >= maxLineBytes {
				w.output.add(w.stream, string(w.buf))
				w.buf = w.buf[:0]
			}
			return len(p), nil
		}
		w.output.add(w.stream, strings.TrimSuffix(string(w.buf[:i]), "\r"))
		w.buf = w.buf[i+1:]
	}
}

// flush emits a final line that did not end in a newline.
func (w *lineWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) > 0 {
		w.output.add(w.stream, strings.TrimSuffix(string(w.buf), "\r"))
		w.buf = nil
	}
}
