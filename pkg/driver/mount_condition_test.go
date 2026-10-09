//go:build linux

package driver

import (
	"context"
	"errors"
	"path/filepath"
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
