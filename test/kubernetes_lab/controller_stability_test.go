package kubernetes_lab

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func controllerStates(p core.Pod) (map[string]core.ContainerStatus, error) {
	if p.UID == "" || p.DeletionTimestamp != nil || p.Status.Phase != core.PodRunning {
		return nil, fmt.Errorf("controller missing identity, terminating, or not Running")
	}
	states := map[string]core.ContainerStatus{}
	for _, s := range p.Status.ContainerStatuses {
		if _, duplicate := states[s.Name]; duplicate || !s.Ready || s.State.Running == nil || s.ContainerID == "" {
			return nil, fmt.Errorf("controller container %q has incomplete/unhealthy status", s.Name)
		}
		states[s.Name] = s
	}
	for _, name := range []string{"driver", "provisioner", "attacher", "resizer"} {
		if _, ok := states[name]; !ok {
			return nil, fmt.Errorf("missing controller container %q", name)
		}
	}
	return states, nil
}

func controllerStayedStable(before, after core.Pod) error {
	a, err := controllerStates(before)
	if err != nil {
		return err
	}
	b, err := controllerStates(after)
	if err != nil {
		return err
	}
	if before.UID != after.UID || len(a) != len(b) {
		return fmt.Errorf("controller pod or container inventory changed")
	}
	for name, old := range a {
		now, ok := b[name]
		if !ok || old.RestartCount != now.RestartCount || old.ContainerID != now.ContainerID {
			return fmt.Errorf("controller %s changed: restarts %d -> %d, container %q -> %q", name, old.RestartCount, now.RestartCount, old.ContainerID, now.ContainerID)
		}
	}
	return nil
}

// The controller is on Linux and is never an intentional fault target in the
// Windows reboot scenario. A recovered Ready state must not hide lease-loss
// exits. Retained runs log prior restart counts and reject any new restarts.
func watchCSIController(t *testing.T) func() {
	t.Helper()
	read := func() core.Pod {
		t.Helper()
		out, err := kubectlWithTimeout(time.Minute, nil, "get", "pod/controller", "-n", ns, "-o", "json")
		var p core.Pod
		if err != nil || json.Unmarshal(out, &p) != nil {
			t.Fatalf("controller snapshot: %v %s", err, out)
		}
		states, err := controllerStates(p)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("CSI controller snapshot UID=%s containers=%+v", p.UID, states)
		return p
	}
	if out, err := kubectlWithTimeout(time.Minute, nil, "wait", "-n", ns, "pod/controller", "--for=condition=Ready", "--timeout=45s"); err != nil {
		t.Fatalf("controller readiness: %v %s", err, out)
	}
	before := read()
	return func() {
		t.Helper()
		if err := controllerStayedStable(before, read()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestControllerStabilityRejectsRecoveredFailures(t *testing.T) {
	p := core.Pod{ObjectMeta: meta.ObjectMeta{UID: "original"}, Status: core.PodStatus{Phase: core.PodRunning}}
	for _, name := range []string{"driver", "provisioner", "attacher", "resizer"} {
		p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, core.ContainerStatus{Name: name, Ready: true, ContainerID: "containerd://" + name, State: core.ContainerState{Running: &core.ContainerStateRunning{}}})
	}
	for _, tc := range []struct {
		name      string
		mutate    func(*core.Pod)
		wantError bool
	}{
		{"unchanged", func(*core.Pod) {}, false},
		{"ready after lease loss", func(p *core.Pod) { p.Status.ContainerStatuses[2].RestartCount++ }, true},
		{"replacement pod", func(p *core.Pod) { p.UID = "replacement" }, true},
		{"replacement container", func(p *core.Pod) { p.Status.ContainerStatuses[2].ContainerID = "replacement" }, true},
		{"missing status", func(p *core.Pod) { p.Status.ContainerStatuses = p.Status.ContainerStatuses[:3] }, true},
		{"not ready", func(p *core.Pod) { p.Status.ContainerStatuses[0].Ready = false }, true},
		{"not running", func(p *core.Pod) { p.Status.ContainerStatuses[0].State.Running = nil }, true},
		{"terminating", func(p *core.Pod) { now := meta.Now(); p.DeletionTimestamp = &now }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			after := p.DeepCopy()
			tc.mutate(after)
			if err := controllerStayedStable(p, *after); (err != nil) != tc.wantError {
				t.Fatalf("stability error=%v, wantError=%v", err, tc.wantError)
			}
		})
	}
}
