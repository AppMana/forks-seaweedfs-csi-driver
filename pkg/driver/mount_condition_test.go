//go:build linux

package driver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
)

func TestNodeAdvertisesVolumeCondition(t *testing.T) {
	ns := newNodeServerWithFakes(t, newFakeMountState())
	resp, err := ns.NodeGetCapabilities(context.Background(), &csi.NodeGetCapabilitiesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range resp.GetCapabilities() {
		if c.GetRpc().GetType() == csi.NodeServiceCapability_RPC_VOLUME_CONDITION {
			return
		}
	}
	t.Fatal("VOLUME_CONDITION node capability is not advertised")
}

// stageHungVolume stages vol-1 whose staging probe blocks past the check
// timeout until the mount manager has stopped the weed process; after that
// the mount is dead, as a killed daemon's FUSE connection is.
func stageHungVolume(t *testing.T, state *fakeMountState, ns *NodeServer) string {
	t.Helper()
	ns.healthCheckTimeout = 20 * time.Millisecond
	stagingPath := filepath.Join(t.TempDir(), "staging")
	volCtx := map[string]string{"collection": "c"}
	vol, err := ns.stageNewVolume("vol-1", stagingPath, volCtx, false)
	if err != nil {
		t.Fatalf("stageNewVolume: %v", err)
	}
	vol.volContext = volCtx
	ns.volumes.Store("vol-1", vol)
	ns.isHealthyFn = func(path string) bool {
		if path != stagingPath {
			return true
		}
		state.mu.Lock()
		stopped := state.unstageCalls > 0
		state.mu.Unlock()
		if stopped {
			return false
		}
		time.Sleep(100 * time.Millisecond)
		return true
	}
	return stagingPath
}

// A probe that keeps timing out longer than weed mount takes to answer any
// FUSE request is a dead mount, not a busy one: it goes through recovery.
func TestHealthMonitorRecoversMountHungPastRequestBound(t *testing.T) {
	state := newFakeMountState()
	ns := newNodeServerWithFakes(t, state)
	stageHungVolume(t, state, ns)
	ns.hungMountAfter = 50 * time.Millisecond

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ns.checkAndRecoverVolumes()
		ns.recoveryWg.Wait()
		state.mu.Lock()
		staged := state.stageCalls
		state.mu.Unlock()
		if staged == 2 {
			break
		}
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if state.unstageCalls != 1 {
		t.Errorf("expected the hung weed mount to be stopped once, got %d", state.unstageCalls)
	}
	if state.stageCalls != 2 {
		t.Errorf("expected the hung mount to be re-staged, got %d stage calls", state.stageCalls)
	}
}

// Fewer than defaultUnhealthyThreshold slow probes never flag a mount,
// however long they took: one long legitimate operation is not a hang.
func TestHealthMonitorDoesNotFlagFewSlowProbes(t *testing.T) {
	state := newFakeMountState()
	ns := newNodeServerWithFakes(t, state)
	stageHungVolume(t, state, ns)
	ns.hungMountAfter = time.Nanosecond

	for i := 0; i < defaultUnhealthyThreshold-1; i++ {
		ns.checkAndRecoverVolumes()
		ns.recoveryWg.Wait()
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if state.unstageCalls != 0 || state.stageCalls != 1 {
		t.Errorf("slow mount was torn down after %d probes: unstage=%d stage=%d", defaultUnhealthyThreshold-1, state.unstageCalls, state.stageCalls)
	}
}

func TestNodeGetVolumeStatsReportsVolumeCondition(t *testing.T) {
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

	stats := func() *csi.NodeGetVolumeStatsResponse {
		t.Helper()
		resp, err := ns.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
			VolumeId:   "vol-1",
			VolumePath: stagingPath,
		})
		if err != nil {
			t.Fatalf("NodeGetVolumeStats: %v", err)
		}
		return resp
	}

	ns.checkAndRecoverVolumes()
	ns.recoveryWg.Wait()
	if c := stats().GetVolumeCondition(); c == nil || c.GetAbnormal() {
		t.Fatalf("healthy volume condition = %v, want present and normal", c)
	}

	// The mount dies and recovery cannot replace it: the manager refuses
	// to stop the old process.
	state.healthy.Store(false)
	state.mu.Lock()
	state.unstageErr = errors.New("manager unavailable")
	state.mu.Unlock()
	for i := 0; i < defaultUnhealthyThreshold; i++ {
		ns.checkAndRecoverVolumes()
		ns.recoveryWg.Wait()
	}

	c := stats().GetVolumeCondition()
	if !c.GetAbnormal() || c.GetMessage() == "" {
		t.Fatalf("persistently dead volume condition = %v, want abnormal with a message", c)
	}
}

// A mount that answers every probe, later than the check timeout but
// within the request bound of weed mount, is busy, not hung, however many
// consecutive checks time out. Each sweep here starts after the previous
// probe answered, as production sweeps do: a check that times out at 30 s
// still holds its slot when the next 30 s tick fires, so that tick is
// skipped and the following check issues a fresh probe.
func TestHealthMonitorDoesNotFlagMountAnsweringWithinRequestBound(t *testing.T) {
	state := newFakeMountState()
	ns := newNodeServerWithFakes(t, state)
	ns.healthCheckTimeout = 20 * time.Millisecond
	ns.hungMountAfter = 60 * time.Millisecond
	stagingPath := filepath.Join(t.TempDir(), "staging")
	volCtx := map[string]string{"collection": "c"}
	vol, err := ns.stageNewVolume("vol-1", stagingPath, volCtx, false)
	if err != nil {
		t.Fatalf("stageNewVolume: %v", err)
	}
	vol.volContext = volCtx
	ns.volumes.Store("vol-1", vol)
	answered := make(chan struct{}, 64)
	ns.isHealthyFn = func(path string) bool {
		if path == stagingPath {
			time.Sleep(30 * time.Millisecond)
			answered <- struct{}{}
		}
		return true
	}

	for i := 0; i < 3*defaultUnhealthyThreshold; i++ {
		ns.checkAndRecoverVolumes()
		ns.recoveryWg.Wait()
		<-answered
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if state.unstageCalls != 0 || state.stageCalls != 1 {
		t.Errorf("mount answering within the request bound was torn down: unstage=%d stage=%d", state.unstageCalls, state.stageCalls)
	}
	if message, abnormal := vol.abnormalMessage(); abnormal {
		t.Errorf("mount answering within the request bound reported abnormal: %s", message)
	}
}

// A volume rebuilt after a node-plugin restart has no unmounter, yet the
// mount service still runs its hung weed mount. Recovery must stop that
// process: otherwise the service answers the re-stage with "already
// mounted" and pods are re-bound onto the bare staging directory.
func TestHealthMonitorStopsHungMountOfRebuiltVolume(t *testing.T) {
	state := newFakeMountState()
	state.modelService = true
	state.serving = true
	ns := newNodeServerWithFakes(t, state)
	ns.healthCheckTimeout = 20 * time.Millisecond
	ns.hungMountAfter = 50 * time.Millisecond
	stagingPath := filepath.Join(t.TempDir(), "staging")
	if err := os.MkdirAll(stagingPath, 0755); err != nil {
		t.Fatal(err)
	}
	vol := ns.rebuildVolumeFromStaging("vol-1", stagingPath)
	vol.volContext = map[string]string{}
	ns.volumes.Store("vol-1", vol)
	// The hung weed mount answers nothing until it is stopped; its FUSE
	// connection then fails every request.
	stopped := make(chan struct{})
	var stopOnce sync.Once
	t.Cleanup(func() { stopOnce.Do(func() { close(stopped) }) })
	ns.volumeUnmounterFn = func(string) (Unmounter, error) {
		return stopUnmounter{state: state, stop: func() { stopOnce.Do(func() { close(stopped) }) }}, nil
	}
	ns.isHealthyFn = func(path string) bool {
		<-stopped
		return false
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ns.checkAndRecoverVolumes()
		ns.recoveryWg.Wait()
		state.mu.Lock()
		done := state.detachCalls > 0
		state.mu.Unlock()
		if done {
			break
		}
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if state.detachCalls != 1 {
		t.Fatalf("hung mount was not recovered: detach=%d", state.detachCalls)
	}
	if state.unstageCalls != 1 || state.stageCalls != 1 {
		t.Errorf("hung weed mount kept serving through recovery: unstage=%d fresh mounts=%d", state.unstageCalls, state.stageCalls)
	}
}

type stopUnmounter struct {
	state *fakeMountState
	stop  func()
}

func (u stopUnmounter) Unmount() error {
	if err := (&stateUnmounter{state: u.state}).Unmount(); err != nil {
		return err
	}
	u.stop()
	return nil
}
