//go:build linux
// +build linux

package driver

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
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

// A restarted mount manager has no record of mounts created by its predecessor.
// Its idempotent Unmount returns nil, but the dead FUSE kernel mount remains.
// Model that independent state explicitly: directory cleanup is not unmount.
func TestHealthMonitorDetachesMountForgottenByRestartedManager(t *testing.T) {
	state := newFakeMountState()
	ns := newNodeServerWithFakes(t, state)
	path := filepath.Join(t.TempDir(), "staging")
	vol, err := ns.stageNewVolume("forgotten", path, map[string]string{}, false)
	if err != nil {
		t.Fatal(err)
	}
	vol.volContext = map[string]string{}
	ns.volumes.Store("forgotten", vol)
	state.healthy.Store(false)
	kernelMounted := true
	ns.detachStagingFn = func(string) error { kernelMounted = false; return nil }
	ns.cleanupStagingFn = func(string) error {
		if kernelMounted {
			return errors.New("dead kernel mount remains after manager forgot it")
		}
		return nil
	}
	ns.recoverVolume("forgotten")
	if kernelMounted || state.stageCalls != 2 {
		t.Fatalf("manager success left dead mount: kernelMounted=%v stageCalls=%d", kernelMounted, state.stageCalls)
	}
}

// The manager RPC must not authorize detaching a mount whose health changed.
func TestHealthMonitorDoesNotDetachRecoveredOrSlowMountAfterManagerRPC(t *testing.T) {
	for _, slow := range []bool{false, true} {
		t.Run(map[bool]string{false: "healthy", true: "slow"}[slow], func(t *testing.T) {
			state := newFakeMountState()
			ns := newNodeServerWithFakes(t, state)
			ns.healthCheckTimeout = 10 * time.Millisecond
			vol, err := ns.stageNewVolume("vol", filepath.Join(t.TempDir(), "stage"), map[string]string{}, false)
			if err != nil {
				t.Fatal(err)
			}
			vol.volContext = map[string]string{}
			ns.volumes.Store("vol", vol)
			var probes atomic.Int32
			release := make(chan struct{})
			defer close(release)
			ns.isHealthyFn = func(string) bool {
				if probes.Add(1) == 1 {
					return false
				}
				if slow {
					<-release
				}
				return true
			}
			ns.recoverVolume("vol")
			if state.detachCalls != 0 || state.cleanupCalls != 0 || state.stageCalls != 1 {
				t.Fatalf("mount that recovered/became slow was modified: detach=%d cleanup=%d stage=%d", state.detachCalls, state.cleanupCalls, state.stageCalls)
			}
		})
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

// A mount whose FUSE daemon is dead can still answer the statx-based
// mount-point check from cached inode attributes while every real I/O fails
// with ENOTCONN. Recovery must then detach the dead mount instead of
// aborting at the "still a mount point" guard forever.
func TestHealthMonitorRecoversDeadMountpoint(t *testing.T) {
	state := newFakeMountState()
	ns := newNodeServerWithFakes(t, state)

	root := t.TempDir()
	stagingPath := filepath.Join(root, "staging")
	volCtx := map[string]string{"collection": "c"}

	vol, err := ns.stageNewVolume("vol-1", stagingPath, volCtx, false)
	if err != nil {
		t.Fatalf("stageNewVolume: %v", err)
	}
	vol.volContext = volCtx
	// A restarted mount service has no record of the old mount process.
	vol.unmounter = nil
	ns.volumes.Store("vol-1", vol)

	origMountPoint := isLikelyNotMountPointFn
	origStatfs := statfsFn
	origLazy := lazyUnmount
	defer func() {
		isLikelyNotMountPointFn = origMountPoint
		statfsFn = origStatfs
		lazyUnmount = origLazy
	}()

	var lazyCalls int
	isLikelyNotMountPointFn = func(p string) (bool, error) {
		if p == stagingPath {
			return false, nil // still a mount point
		}
		return origMountPoint(p)
	}
	statfsFn = func(p string) error {
		if p == stagingPath {
			return syscall.ENOTCONN
		}
		return nil
	}
	lazyUnmount = func(p string) error {
		lazyCalls++
		return nil
	}
	ns.detachStagingFn = detachDeadMountPoint
	state.healthy.Store(false)

	for i := 0; i < defaultUnhealthyThreshold; i++ {
		ns.checkAndRecoverVolumes()
		ns.recoveryWg.Wait()
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if lazyCalls != 1 {
		t.Errorf("expected 1 lazy unmount of the dead staging mount, got %d", lazyCalls)
	}
	if state.cleanupCalls != 1 {
		t.Errorf("expected 1 staging cleanup, got %d", state.cleanupCalls)
	}
	if state.stageCalls != 2 {
		t.Errorf("expected 2 stage calls after recovery, got %d", state.stageCalls)
	}
}

// A mount that still answers statfs is alive: recovery must keep refusing
// to remove the staging path underneath it.
func TestHealthMonitorStillAbortsOnLiveMountpoint(t *testing.T) {
	state := newFakeMountState()
	ns := newNodeServerWithFakes(t, state)

	root := t.TempDir()
	stagingPath := filepath.Join(root, "staging")
	volCtx := map[string]string{"collection": "c"}

	vol, err := ns.stageNewVolume("vol-1", stagingPath, volCtx, false)
	if err != nil {
		t.Fatalf("stageNewVolume: %v", err)
	}
	vol.volContext = volCtx
	vol.unmounter = nil
	ns.volumes.Store("vol-1", vol)

	origMountPoint := isLikelyNotMountPointFn
	origStatfs := statfsFn
	defer func() {
		isLikelyNotMountPointFn = origMountPoint
		statfsFn = origStatfs
	}()

	isLikelyNotMountPointFn = func(p string) (bool, error) {
		if p == stagingPath {
			return false, nil
		}
		return origMountPoint(p)
	}
	statfsFn = func(p string) error { return nil }
	state.healthy.Store(false)

	for i := 0; i < defaultUnhealthyThreshold; i++ {
		ns.checkAndRecoverVolumes()
		ns.recoveryWg.Wait()
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if state.detachCalls != 0 {
		t.Errorf("expected no detach of a live mount, got %d", state.detachCalls)
	}
	if state.cleanupCalls != 0 {
		t.Errorf("expected no cleanup against a live mount, got %d", state.cleanupCalls)
	}
	if state.stageCalls != 1 {
		t.Errorf("expected no re-stage against a live mount, got %d", state.stageCalls)
	}
}

// A mount that still shows up in /proc/mounts but whose daemon hangs
// statfs is indistinguishable from a live mount: recovery must abort
// rather than detach it. Timed-out callers must also reuse the one
// still-blocked probe instead of piling up a syscall goroutine per sweep.
func TestHealthMonitorAbortsWhenStatfsProbeHangs(t *testing.T) {
	state := newFakeMountState()
	ns := newNodeServerWithFakes(t, state)

	root := t.TempDir()
	stagingPath := filepath.Join(root, "staging")
	volCtx := map[string]string{"collection": "c"}

	vol, err := ns.stageNewVolume("vol-1", stagingPath, volCtx, false)
	if err != nil {
		t.Fatalf("stageNewVolume: %v", err)
	}
	vol.volContext = volCtx
	vol.unmounter = nil
	ns.volumes.Store("vol-1", vol)

	origMountPoint := isLikelyNotMountPointFn
	origStatfs := statfsFn
	origLazy := lazyUnmount
	origTimeout := statfsProbeTimeout
	statfsProbeTimeout = 50 * time.Millisecond
	defer func() {
		isLikelyNotMountPointFn = origMountPoint
		statfsFn = origStatfs
		lazyUnmount = origLazy
		statfsProbeTimeout = origTimeout
		resetStatfsProbe(stagingPath)
	}()

	isLikelyNotMountPointFn = func(p string) (bool, error) {
		if p == stagingPath {
			return false, nil
		}
		return origMountPoint(p)
	}
	release := make(chan struct{})
	defer close(release)
	statfsStarted := make(chan struct{})
	var startOnce sync.Once
	var statfsCalls atomic.Int32
	statfsFn = func(p string) error {
		if p == stagingPath {
			statfsCalls.Add(1)
			startOnce.Do(func() { close(statfsStarted) })
			<-release
		}
		return nil
	}
	var lazyCalls int
	lazyUnmount = func(p string) error {
		lazyCalls++
		return nil
	}
	ns.detachStagingFn = detachDeadMountPoint
	state.healthy.Store(false)

	for i := 0; i < defaultUnhealthyThreshold; i++ {
		ns.checkAndRecoverVolumes()
		ns.recoveryWg.Wait()
	}

	// Once the first probe's syscall is known to be blocked, a second
	// recovery attempt must attach to it rather than spawn another statfs
	// call.
	<-statfsStarted
	for i := 0; i < defaultUnhealthyThreshold; i++ {
		ns.checkAndRecoverVolumes()
		ns.recoveryWg.Wait()
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if lazyCalls != 0 {
		t.Errorf("expected no lazy unmount against a hung mount, got %d", lazyCalls)
	}
	if state.cleanupCalls != 0 {
		t.Errorf("expected no cleanup against a hung mount, got %d", state.cleanupCalls)
	}
	if state.stageCalls != 1 {
		t.Errorf("expected no re-stage against a hung mount, got %d", state.stageCalls)
	}
	if got := statfsCalls.Load(); got != 1 {
		t.Errorf("expected the second sweep to reuse the in-flight probe, got %d statfs syscalls", got)
	}
}

// Unstage removes the mount at the staging path. A statfs probe still
// blocked against that old mount must be invalidated; otherwise a mount
// re-staged at the same path inherits the blocked probe and never
// reports healthy.
func TestUnstageResetsBlockedStatfsProbe(t *testing.T) {
	state := newFakeMountState()
	ns := newNodeServerWithFakes(t, state)

	stagingPath := filepath.Join(t.TempDir(), "staging")
	vol, err := ns.stageNewVolume("vol-1", stagingPath, map[string]string{}, false)
	if err != nil {
		t.Fatalf("stageNewVolume: %v", err)
	}

	origStatfs := statfsFn
	origTimeout := statfsProbeTimeout
	statfsProbeTimeout = 50 * time.Millisecond
	defer func() {
		statfsFn = origStatfs
		statfsProbeTimeout = origTimeout
		resetStatfsProbe(stagingPath)
	}()

	release := make(chan struct{})
	defer close(release)
	var statfsCalls atomic.Int32
	statfsFn = func(p string) error {
		if p == stagingPath {
			statfsCalls.Add(1)
			<-release
		}
		return nil
	}

	// The first probe blocks in the syscall and stays registered.
	if _, probed := probeStatfs(stagingPath); probed {
		t.Fatal("expected the blocked probe to time out")
	}

	if err := vol.Unstage(stagingPath); err != nil {
		t.Fatalf("Unstage: %v", err)
	}

	// A probe after unstage must issue a fresh syscall rather than
	// attach to the orphaned one.
	if _, probed := probeStatfs(stagingPath); probed {
		t.Fatal("expected the new probe to time out as well")
	}
	if got := statfsCalls.Load(); got != 2 {
		t.Errorf("expected a fresh statfs syscall after unstage reset the probe, got %d", got)
	}
}
