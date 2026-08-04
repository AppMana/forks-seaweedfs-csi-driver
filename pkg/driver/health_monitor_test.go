//go:build linux
// +build linux

package driver

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeMountState tracks the behavior of a fake FUSE mount across a
// simulated crash-and-recover lifecycle. It is used to verify that the
// health monitor:
//  1. Detects a dead FUSE mount,
//  2. Re-stages with a fresh mounter, and
//  3. Re-binds all previously published paths.
type fakeMountState struct {
	mu sync.Mutex

	// healthy controls the default return value of isHealthyFn. Flip to
	// false to simulate the FUSE daemon dying.
	healthy atomic.Bool

	// pathHealth overrides per-path health. A path present in the map
	// uses its explicit value regardless of the healthy flag; absent
	// paths fall back to healthy.Load(). Lets tests simulate a single
	// unhealthy publish bind mount without taking down staging too.
	pathHealth sync.Map // map[string]bool

	stageCalls       int
	unstageCalls     int
	cleanupCalls     int
	detachCalls      int
	unmountCalls     int
	bindMountCalls   int
	bindMountTargets []string

	// unstageErr, when non-nil, is returned from stateUnmounter.Unmount.
	// Lets tests exercise the recovery path's response to a manager
	// teardown failure.
	unstageErr error
}

func newFakeMountState() *fakeMountState {
	s := &fakeMountState{}
	s.healthy.Store(true)
	return s
}

// isHealthy returns the effective health of the given path. If the path
// has been explicitly registered via setPathHealth, that value wins;
// otherwise the global healthy flag is used.
func (s *fakeMountState) isHealthy(path string) bool {
	if v, ok := s.pathHealth.Load(path); ok {
		return v.(bool)
	}
	return s.healthy.Load()
}

func (s *fakeMountState) setPathHealth(path string, healthy bool) {
	s.pathHealth.Store(path, healthy)
}

// newMounter returns a Mounter that records Stage calls in this state.
func (s *fakeMountState) newMounter() Mounter {
	return &stateMounter{state: s}
}

type stateMounter struct{ state *fakeMountState }

func (m *stateMounter) Mount(target string) (Unmounter, error) {
	m.state.mu.Lock()
	m.state.stageCalls++
	m.state.mu.Unlock()
	// Create the target so any downstream checkMount sees a directory.
	if err := os.MkdirAll(target, 0755); err != nil {
		return nil, err
	}
	return &stateUnmounter{state: m.state}, nil
}

type stateUnmounter struct{ state *fakeMountState }

func (u *stateUnmounter) Unmount() error {
	u.state.mu.Lock()
	u.state.unstageCalls++
	err := u.state.unstageErr
	u.state.mu.Unlock()
	return err
}

// newNodeServerWithFakes wires a NodeServer to a fakeMountState, bypassing
// all real mount-service and mountutil interactions. The health monitor is
// not started — tests drive checkAndRecoverVolumes directly for determinism.
func newNodeServerWithFakes(t *testing.T, state *fakeMountState) *NodeServer {
	t.Helper()

	ns := &NodeServer{
		Driver:        &SeaweedFsDriver{},
		volumeMutexes: NewKeyMutex(),
		stopCh:        make(chan struct{}),
		mounterFactory: func(volumeID string, readOnly bool, driver *SeaweedFsDriver, volContext map[string]string) (Mounter, error) {
			return state.newMounter(), nil
		},
		capacityFn: func(volumeID string) (int64, error) {
			return 0, errors.New("no capacity in tests")
		},
		isHealthyFn: func(path string) bool {
			return state.isHealthy(path)
		},
		cleanupStagingFn: func(path string) error {
			state.mu.Lock()
			state.cleanupCalls++
			state.mu.Unlock()
			// Remove the staging directory so the next Mount recreates it,
			// mirroring real cleanup behavior.
			return os.RemoveAll(path)
		},
		detachStagingFn: func(path string) error {
			state.mu.Lock()
			state.detachCalls++
			state.mu.Unlock()
			return nil
		},
		unmountFn: func(path string) error {
			state.mu.Lock()
			state.unmountCalls++
			state.mu.Unlock()
			return nil
		},
		bindMountFn: func(source, target string, readOnly bool) error {
			state.mu.Lock()
			state.bindMountCalls++
			state.bindMountTargets = append(state.bindMountTargets, target)
			state.mu.Unlock()
			// Create the target directory so checkMount on it returns false,
			// letting Publish proceed.
			return os.MkdirAll(target, 0755)
		},
	}
	return ns
}

// TestHealthMonitorDetachesReconstructedDeadMount covers recovery after the
// mount daemon restarts independently from the CSI node plugin. The volume is
// reconstructed from kubelet state and therefore has no manager unmounter;
// the confirmed-dead kernel FUSE mount must be detached before re-staging.
func TestHealthMonitorDetachesReconstructedDeadMount(t *testing.T) {
	state := newFakeMountState()
	ns := newNodeServerWithFakes(t, state)

	stagingPath := filepath.Join(t.TempDir(), "staging")
	if err := os.MkdirAll(stagingPath, 0755); err != nil {
		t.Fatalf("mkdir staging: %v", err)
	}
	vol := ns.rebuildVolumeFromStaging("vol-1", stagingPath)
	vol.volContext = map[string]string{}
	ns.volumes.Store("vol-1", vol)
	state.healthy.Store(false)

	for i := 0; i < defaultUnhealthyThreshold; i++ {
		ns.checkAndRecoverVolumes()
		ns.recoveryWg.Wait()
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if state.detachCalls != 1 {
		t.Fatalf("expected one dead staging detach, got %d", state.detachCalls)
	}
	if state.stageCalls != 1 {
		t.Fatalf("expected reconstructed volume to be re-staged once, got %d", state.stageCalls)
	}
}

// TestHealthMonitorRecoversStaleMount is the integration test for
// seaweedfs/seaweedfs-csi-driver#253. It walks through the full CSI
// lifecycle — stage → publish → simulated FUSE crash → recovery — and
// verifies that after recovery the volume has a fresh mount and all
// publish paths are re-bound.
func TestHealthMonitorRecoversStaleMount(t *testing.T) {
	state := newFakeMountState()
	ns := newNodeServerWithFakes(t, state)

	root := t.TempDir()
	stagingPath := filepath.Join(root, "staging")
	publishA := filepath.Join(root, "podA", "mount")
	publishB := filepath.Join(root, "podB", "mount")

	volCtx := map[string]string{"collection": "c"}

	// --- Stage ---
	vol, err := ns.stageNewVolume("vol-1", stagingPath, volCtx, false)
	if err != nil {
		t.Fatalf("stageNewVolume: %v", err)
	}
	vol.volContext = volCtx
	vol.readOnly = false
	ns.volumes.Store("vol-1", vol)

	if state.stageCalls != 1 {
		t.Fatalf("expected 1 stage call, got %d", state.stageCalls)
	}

	// --- Publish to two pods ---
	if err := vol.Publish(stagingPath, publishA, false); err != nil {
		t.Fatalf("publish A: %v", err)
	}
	vol.AddPublishPath(publishA, false)

	if err := vol.Publish(stagingPath, publishB, true); err != nil {
		t.Fatalf("publish B: %v", err)
	}
	vol.AddPublishPath(publishB, true)

	if state.bindMountCalls != 2 {
		t.Fatalf("expected 2 bind mount calls after publish, got %d", state.bindMountCalls)
	}

	// --- Simulate FUSE crash ---
	state.healthy.Store(false)

	// Sanity: health monitor should now consider the mount unhealthy.
	if ns.isHealthyFn(stagingPath) {
		t.Fatal("fake should report unhealthy after crash")
	}

	// --- Trigger health check cycles (recovery fires only after
	// defaultUnhealthyThreshold consecutive failures) ---
	for i := 0; i < defaultUnhealthyThreshold; i++ {
		ns.checkAndRecoverVolumes()
		ns.recoveryWg.Wait()
	}

	// --- Verify recovery actions ---
	state.mu.Lock()
	defer state.mu.Unlock()

	// A second stage call means the FUSE mount was re-created.
	if state.stageCalls != 2 {
		t.Errorf("expected 2 stage calls after recovery, got %d", state.stageCalls)
	}
	// Staging was cleaned up before re-stage.
	if state.cleanupCalls != 1 {
		t.Errorf("expected 1 staging cleanup, got %d", state.cleanupCalls)
	}
	// Both stale bind mounts were unmounted.
	if state.unmountCalls != 2 {
		t.Errorf("expected 2 bind unmounts, got %d", state.unmountCalls)
	}
	// Both publish paths were re-bound (total 4: 2 initial + 2 recovery).
	if state.bindMountCalls != 4 {
		t.Errorf("expected 4 total bind mounts (2 initial + 2 recovery), got %d", state.bindMountCalls)
	}
	// Recovery must tear down the previous FUSE mount via the volume's
	// unmounter so the mount manager clears its in-memory state. Without
	// this, the manager would falsely report "already mounted" on the
	// follow-up Mount call and the recovery would silently bind onto a
	// dead path. Regression test for seaweedfs/seaweedfs-csi-driver#261.
	if state.unstageCalls != 1 {
		t.Errorf("expected 1 unstage (mount-manager teardown) during recovery, got %d", state.unstageCalls)
	}

	// --- Verify the replacement volume is tracked correctly ---
	got, ok := ns.volumes.Load("vol-1")
	if !ok {
		t.Fatal("volume missing from map after recovery")
	}
	newVol := got.(*Volume)
	if newVol == vol {
		t.Error("expected a replacement Volume instance after recovery")
	}
	if newVol.volContext["collection"] != "c" {
		t.Error("volContext not propagated to recovered volume")
	}

	// Both publish paths should be re-tracked on the new volume.
	seen := map[string]bool{}
	newVol.publishPaths.Range(func(k, v interface{}) bool {
		seen[k.(string)] = true
		return true
	})
	if !seen[publishA] || !seen[publishB] {
		t.Errorf("expected publish paths %q and %q tracked, got %v", publishA, publishB, seen)
	}
}

// TestHealthMonitorAbortsOnUnmounterError verifies the recovery path
// bails out when the volume's unmounter (which talks to the mount
// manager) returns an error. Continuing into cleanup/re-stage with the
// manager still holding a stale entry would either fall back into the
// "already mounted" no-op or, with PR #262 not yet in place, risk a
// host-level RemoveAll on a still-live FUSE mount.
func TestHealthMonitorAbortsOnUnmounterError(t *testing.T) {
	state := newFakeMountState()
	ns := newNodeServerWithFakes(t, state)

	stagingPath := filepath.Join(t.TempDir(), "staging")
	volCtx := map[string]string{"collection": "c"}

	vol, err := ns.stageNewVolume("vol-1", stagingPath, volCtx, false)
	if err != nil {
		t.Fatalf("stageNewVolume: %v", err)
	}
	vol.volContext = volCtx
	ns.volumes.Store("vol-1", vol)

	state.mu.Lock()
	state.unstageErr = errors.New("simulated manager unmount failure")
	state.mu.Unlock()
	state.healthy.Store(false)

	for i := 0; i < defaultUnhealthyThreshold; i++ {
		ns.checkAndRecoverVolumes()
		ns.recoveryWg.Wait()
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if state.unstageCalls != 1 {
		t.Errorf("expected the unmounter to be invoked exactly once, got %d", state.unstageCalls)
	}
	// Recovery must abort: no host cleanup, no re-stage, no re-bind.
	if state.cleanupCalls != 0 {
		t.Errorf("expected no staging cleanup after manager-unmount failure, got %d", state.cleanupCalls)
	}
	if state.stageCalls != 1 {
		t.Errorf("expected no re-stage after manager-unmount failure, got %d", state.stageCalls)
	}
	// Original volume must remain in the map so the next sweep can retry.
	got, ok := ns.volumes.Load("vol-1")
	if !ok {
		t.Fatal("volume removed from map after aborted recovery")
	}
	if got.(*Volume) != vol {
		t.Error("expected original volume preserved after aborted recovery")
	}
}

// Pins the invariant: publish binds must not be torn down until
// re-staging has succeeded, so a failed re-stage leaves the (broken)
// binds in place rather than leaving kubelet seeing empty publish paths.
func TestHealthMonitorPreservesPublishesOnReStageFailure(t *testing.T) {
	state := newFakeMountState()
	ns := newNodeServerWithFakes(t, state)

	root := t.TempDir()
	stagingPath := filepath.Join(root, "staging")
	publishPath := filepath.Join(root, "pod", "mount")
	volCtx := map[string]string{"collection": "c"}

	vol, err := ns.stageNewVolume("vol-1", stagingPath, volCtx, false)
	if err != nil {
		t.Fatalf("stageNewVolume: %v", err)
	}
	vol.volContext = volCtx
	if err := vol.Publish(stagingPath, publishPath, false); err != nil {
		t.Fatalf("publish: %v", err)
	}
	vol.AddPublishPath(publishPath, false)
	ns.volumes.Store("vol-1", vol)

	bindMountsBefore := state.bindMountCalls
	unmountsBefore := state.unmountCalls

	// Swap in a failing factory so the recovery's stageNewVolume errors
	// (the first factory call above already succeeded).
	wantErr := errors.New("simulated re-stage failure")
	ns.mounterFactory = func(volumeID string, readOnly bool, driver *SeaweedFsDriver, volContext map[string]string) (Mounter, error) {
		return nil, wantErr
	}

	state.healthy.Store(false)

	for i := 0; i < defaultUnhealthyThreshold; i++ {
		ns.checkAndRecoverVolumes()
		ns.recoveryWg.Wait()
	}

	state.mu.Lock()
	defer state.mu.Unlock()

	if state.unstageCalls != 1 {
		t.Errorf("expected 1 manager unmount during failed recovery, got %d", state.unstageCalls)
	}
	if state.cleanupCalls != 1 {
		t.Errorf("expected 1 staging cleanup during failed recovery, got %d", state.cleanupCalls)
	}
	if state.unmountCalls != unmountsBefore {
		t.Errorf("publish bind mounts must not be unmounted when re-stage fails, got %d unmounts (expected %d)", state.unmountCalls, unmountsBefore)
	}
	if state.bindMountCalls != bindMountsBefore {
		t.Errorf("expected no new bind mounts when re-stage fails, got %d (was %d)", state.bindMountCalls, bindMountsBefore)
	}
	got, ok := ns.volumes.Load("vol-1")
	if !ok {
		t.Fatal("volume removed from map after failed recovery")
	}
	if got.(*Volume) != vol {
		t.Error("expected original volume preserved after failed recovery")
	}
}

// TestHealthMonitorSkipsHealthyVolumes verifies the monitor does not
// disrupt volumes whose FUSE mount is still alive.
func TestHealthMonitorSkipsHealthyVolumes(t *testing.T) {
	state := newFakeMountState()
	ns := newNodeServerWithFakes(t, state)

	stagingPath := filepath.Join(t.TempDir(), "staging")
	vol, err := ns.stageNewVolume("vol-1", stagingPath, map[string]string{}, false)
	if err != nil {
		t.Fatalf("stageNewVolume: %v", err)
	}
	vol.volContext = map[string]string{}
	ns.volumes.Store("vol-1", vol)

	// Healthy throughout — one recovery sweep should be a no-op.
	for i := 0; i < defaultUnhealthyThreshold; i++ {
		ns.checkAndRecoverVolumes()
		ns.recoveryWg.Wait()
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if state.stageCalls != 1 {
		t.Errorf("expected 1 stage call (no recovery), got %d", state.stageCalls)
	}
	if state.cleanupCalls != 0 {
		t.Errorf("expected 0 cleanup calls, got %d", state.cleanupCalls)
	}
}

// TestHealthMonitorSkipsVolumesWithoutContext verifies that volumes
// rebuilt from an existing mount (no volContext) are left alone — they
// cannot be auto-recovered and must be re-staged by kubelet.
func TestHealthMonitorSkipsVolumesWithoutContext(t *testing.T) {
	state := newFakeMountState()
	ns := newNodeServerWithFakes(t, state)

	stagingPath := filepath.Join(t.TempDir(), "staging")
	if err := os.MkdirAll(stagingPath, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Volume has a StagedPath but no volContext — mimics the rebuild path.
	vol := &Volume{
		VolumeId:   "vol-1",
		StagedPath: stagingPath,
		driver:     ns.Driver,
	}
	ns.volumes.Store("vol-1", vol)

	state.healthy.Store(false)
	for i := 0; i < defaultUnhealthyThreshold; i++ {
		ns.checkAndRecoverVolumes()
		ns.recoveryWg.Wait()
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if state.stageCalls != 0 {
		t.Errorf("expected no stage calls for context-less volume, got %d", state.stageCalls)
	}
	if state.cleanupCalls != 0 {
		t.Errorf("expected no cleanup calls, got %d", state.cleanupCalls)
	}
}

// TestHealthMonitorDeduplicatesInFlightRecovery verifies that a second
// sweep arriving while a recovery for the same volume is still in
// flight does not spawn a duplicate goroutine. This is the regression
// test for the gemini-code-assist concern about goroutine pile-up when
// a FUSE-related syscall hangs during recovery.
func TestHealthMonitorDeduplicatesInFlightRecovery(t *testing.T) {
	state := newFakeMountState()
	ns := newNodeServerWithFakes(t, state)

	// Pre-populate the in-flight set directly so any sweep for "vol-1"
	// is expected to skip. We do not delete it, so even after the
	// sweep the slot remains "busy" — mimicking a hung recovery.
	ns.activeRecoveries.Store("vol-1", struct{}{})

	stagingPath := filepath.Join(t.TempDir(), "staging")
	vol, err := ns.stageNewVolume("vol-1", stagingPath, map[string]string{}, false)
	if err != nil {
		t.Fatalf("stageNewVolume: %v", err)
	}
	ns.volumes.Store("vol-1", vol)

	// Flip staging to unhealthy — normally this would trigger full recovery.
	state.healthy.Store(false)

	before := state.stageCalls
	for i := 0; i < defaultUnhealthyThreshold; i++ {
		ns.checkAndRecoverVolumes()
		ns.recoveryWg.Wait()
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	// No new stage call should have happened because the sweep found
	// an in-flight marker and bailed out.
	if state.stageCalls != before {
		t.Errorf("expected no new stage calls while recovery is in flight, got %d new", state.stageCalls-before)
	}
}

// TestHealthMonitorDoesNotRecoverSlowButAliveMount is the regression
// test for the production ENOTCONN incident on appmana-022/025. A FUSE
// staging mount that is *alive but slow* — its os.ReadDir blocked behind
// queued filer I/O while a GPU workload does large sequential reads —
// must never be torn down. The pre-fix code collapsed a health-probe
// timeout into "dead" and, after the consecutive-failure threshold,
// ran full recovery: it unmounted the live publish bind mount out from
// under the running pod, which is exactly what handed that pod
// "Transport endpoint is not connected" on its first write.
//
// The mount here is not dead: the probe never returns a negative, it
// simply blocks past the health-check timeout. Recovery (staging
// cleanup, re-stage, publish unmount/re-bind) must NOT fire.
func TestHealthMonitorDoesNotRecoverSlowButAliveMount(t *testing.T) {
	state := newFakeMountState()
	ns := newNodeServerWithFakes(t, state)

	// Tight timeout so "slow" is reached in milliseconds, not the
	// production 30s.
	ns.healthCheckTimeout = 50 * time.Millisecond

	root := t.TempDir()
	stagingPath := filepath.Join(root, "staging")
	publishPath := filepath.Join(root, "pod", "mount")

	volCtx := map[string]string{"collection": "c"}

	vol, err := ns.stageNewVolume("vol-1", stagingPath, volCtx, false)
	if err != nil {
		t.Fatalf("stageNewVolume: %v", err)
	}
	vol.volContext = volCtx
	vol.readOnly = false
	ns.volumes.Store("vol-1", vol)

	if err := vol.Publish(stagingPath, publishPath, false); err != nil {
		t.Fatalf("publish: %v", err)
	}
	vol.AddPublishPath(publishPath, false)

	// The probe for the staging path BLOCKS past the timeout (alive but
	// busy). Publish paths answer healthy immediately. Crucially the
	// probe never returns false — the mount is not dead, only slow.
	ns.isHealthyFn = func(path string) bool {
		if path == stagingPath {
			time.Sleep(200 * time.Millisecond)
			return true
		}
		return true
	}

	// Drive well past the consecutive-failure threshold. A slow mount
	// must be recovered on none of these ticks.
	for i := 0; i < defaultUnhealthyThreshold+2; i++ {
		ns.checkAndRecoverVolumes()
		ns.recoveryWg.Wait()
	}

	state.mu.Lock()
	defer state.mu.Unlock()

	if state.stageCalls != 1 {
		t.Errorf("slow-but-alive mount was re-staged: expected 1 stage call, got %d", state.stageCalls)
	}
	if state.cleanupCalls != 0 {
		t.Errorf("slow-but-alive mount had its staging path cleaned up: expected 0, got %d", state.cleanupCalls)
	}
	if state.unmountCalls != 0 {
		t.Errorf("recovery unmounted the live publish path out from under the pod (root cause of ENOTCONN): expected 0 unmounts, got %d", state.unmountCalls)
	}
	if state.unstageCalls != 0 {
		t.Errorf("recovery tore down the live FUSE mount via the manager: expected 0, got %d", state.unstageCalls)
	}
}

// TestHealthMonitorRetriesFailedPublishes verifies the second-chance
// publish retry path: if staging is healthy but a previously recovered
// volume has a publish bind mount that never came back up, the next
// sweep re-binds it without re-staging the whole volume.
//
// This covers the gemini-code-assist feedback that "retry on next
// sweep" was not actually happening because the monitor only looked at
// staging health.
func TestHealthMonitorRetriesFailedPublishes(t *testing.T) {
	state := newFakeMountState()
	ns := newNodeServerWithFakes(t, state)

	root := t.TempDir()
	stagingPath := filepath.Join(root, "staging")
	publishPath := filepath.Join(root, "pod", "mount")

	vol, err := ns.stageNewVolume("vol-1", stagingPath, map[string]string{}, false)
	if err != nil {
		t.Fatalf("stageNewVolume: %v", err)
	}
	ns.volumes.Store("vol-1", vol)

	// Publish once successfully so the path is tracked, but then mark
	// it unhealthy — this mimics the state after a partial recovery
	// where stageNewVolume succeeded but newVol.Publish failed for this
	// target.
	if err := vol.Publish(stagingPath, publishPath, false); err != nil {
		t.Fatalf("publish: %v", err)
	}
	vol.AddPublishPath(publishPath, false)

	initialBind := state.bindMountCalls
	state.setPathHealth(publishPath, false)

	// Staging is healthy, publish is not → retryPublishPaths runs on the
	// first tick (the consecutive-failure threshold gates only staging
	// recovery, not publish re-binds).
	ns.checkAndRecoverVolumes()
	ns.recoveryWg.Wait()

	state.mu.Lock()
	defer state.mu.Unlock()

	// No re-staging: still 1 stage call total.
	if state.stageCalls != 1 {
		t.Errorf("expected 1 stage call (retry should not re-stage), got %d", state.stageCalls)
	}
	if state.cleanupCalls != 0 {
		t.Errorf("expected 0 staging cleanup calls, got %d", state.cleanupCalls)
	}
	// The publish path was re-bound: bindMountCalls went up by 1.
	if state.bindMountCalls != initialBind+1 {
		t.Errorf("expected %d bind mounts after retry, got %d", initialBind+1, state.bindMountCalls)
	}
}
