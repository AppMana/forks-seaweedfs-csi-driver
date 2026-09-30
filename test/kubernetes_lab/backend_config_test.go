package kubernetes_lab

import (
	"strings"
	"testing"

	core "k8s.io/api/core/v1"
)

func TestCSIBackendUsesCandidateSeaweedFS(t *testing.T) {
	old := *splitImages["mount-linux"]
	t.Cleanup(func() { *splitImages["mount-linux"] = old })
	want := "example.test/candidate@sha256:" + strings.Repeat("a", 64)
	*splitImages["mount-linux"] = want
	seen := 0
	for _, object := range objects() {
		pod, ok := object.(core.Pod)
		if !ok || (pod.Name != "backend" && pod.Name != "capacity") {
			continue
		}
		seen++
		c := pod.Spec.Containers[0]
		if c.Image != want || len(c.Command) != 1 || c.Command[0] != "/usr/bin/weed" || c.ImagePullPolicy != core.PullNever {
			t.Errorf("%s serves older/unpinned bytes: %+v", pod.Name, c)
		}
	}
	if seen != 2 {
		t.Fatalf("expected backend and capacity, got %d", seen)
	}
}

// The native Windows suite exercises 255 UTF-16-unit components, including
// names larger than the filer's default 255 UTF-8-byte budget. Configure the
// serving filer, not the client or a path-only rule (rename uses the global
// limit too). The mount's own platform-specific name checks stay enabled.
func TestCSIBackendWindowsFilenameBudget(t *testing.T) {
	const setting = "WEED_FILER_OPTIONS_MAX_FILE_NAME_LENGTH"
	found := 0
	for _, object := range objects() {
		pod, ok := object.(core.Pod)
		if !ok {
			continue
		}
		for _, c := range pod.Spec.Containers {
			for _, env := range c.Env {
				if env.Name != setting {
					continue
				}
				if pod.Name != "backend" || c.Name != "weed" {
					t.Fatalf("filer filename policy unexpectedly configured on %s/%s", pod.Name, c.Name)
				}
				if env.Value != "1020" || env.ValueFrom != nil {
					t.Fatalf("explicit Windows-compatible filer byte budget required, got %+v", env)
				}
				found++
			}
		}
	}
	if found != 1 {
		t.Fatalf("expected one serving filer with explicit 1020-byte filename budget, got %d", found)
	}
}
