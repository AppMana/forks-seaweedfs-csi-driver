package kubernetes_lab

import (
	"testing"

	core "k8s.io/api/core/v1"
)

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
