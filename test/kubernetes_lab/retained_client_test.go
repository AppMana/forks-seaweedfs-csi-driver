package kubernetes_lab

import (
	"encoding/json"
	"testing"
)

// Recreate only a disposable consumer after an explicit mount upgrade.
// Never seed data, recreate the PVC, or modify the mount implementation here.
func TestRetainedWindowsClient(t *testing.T) {
	if !*live {
		t.Skip("requires the existing isolated CSI fixture")
	}
	assertFixture(t)
	if out, err := kubectl(nil, "get", "pvc", "shared", "-n", ns); err != nil {
		t.Fatalf("original PVC unavailable: %s: %v", out, err)
	}
	body, err := json.Marshal(clientPod("windows"))
	if err != nil {
		t.Fatal(err)
	}
	if out, err := kubectl(body, "apply", "-f", "-"); err != nil {
		t.Fatalf("apply original consumer spec: %s: %v", out, err)
	}
	if out, err := kubectl(nil, "wait", "-n", ns, "--for=condition=Ready", "pod/"+clientName("windows"), "--timeout=180s"); err != nil {
		t.Fatalf("consumer readiness: %s: %v", out, err)
	}
}
