package mountmanager

import (
	"bufio"
	"errors"
	"fmt"
	"io"
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

	mu     sync.Mutex
	mounts map[string]*mountEntry
	locks  *keyMutex
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
		mounts:     make(map[string]*mountEntry),
		locks:      newKeyMutex(),
	}
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

	args := req.MountArgs
	if len(args) == 0 {
		return nil, errors.New("mountArgs is required")
	}

	process, err := startWeedMountProcess(m.weedBinary, args, targetPath, req.VolumeID)
	if err != nil {
		return nil, err
	}

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

	// platformProcess holds per-OS supervision state (e.g. the Windows
	// job object handle). It is a zero-size struct on Linux.
	platformProcess
}

func startWeedMountProcess(command string, args []string, target string, volumeID string) (*weedMountProcess, error) {
	cmd := exec.Command(command, args...)
	configureCmd(cmd)

	// Capture stdout/stderr and log with volume ID prefix for better debugging
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("creating stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("creating stderr pipe: %w", err)
	}

	glog.V(0).Infof("[%s] Starting weed mount: %s %s", volumeID, command, strings.Join(args, " "))

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting weed mount: %w", err)
	}

	// Forward stdout/stderr with volume ID prefix for better debugging
	go forwardLogs(stdoutPipe, volumeID, "stdout")
	go forwardLogs(stderrPipe, volumeID, "stderr")

	process := &weedMountProcess{
		cmd:    cmd,
		target: target,
		exited: make(chan struct{}),
		done:   make(chan struct{}),
	}

	if err := process.afterStart(); err != nil {
		glog.Warningf("[%s] post-start process supervision setup failed: %v", volumeID, err)
	}

	go process.wait()

	if err := waitForMount(target, 10*time.Second); err != nil {
		if stopErr := process.stop(); stopErr != nil {
			glog.Warningf("[%s] failed to stop mount process after mount wait failure: %v", volumeID, stopErr)
		}
		return nil, err
	}

	return process, nil
}

func (p *weedMountProcess) wait() {
	if err := p.cmd.Wait(); err != nil {
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

// forwardLogs reads from a pipe and logs each line with a volume ID prefix.
func forwardLogs(pipe io.ReadCloser, volumeID string, stream string) {
	scanner := bufio.NewScanner(pipe)
	for scanner.Scan() {
		glog.Infof("[%s] %s: %s", volumeID, stream, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		glog.Warningf("[%s] error reading %s: %v", volumeID, stream, err)
	}
}
