package kubernetes_lab

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	core "k8s.io/api/core/v1"
)

// Keep CSI and the consumer alive: restarting either would hide the manager's
// lost-state bug. The existing mixed-workload verifier reads, never reseeds.
func runLinuxSupervisorRecovery(t *testing.T, root, token string, run func(...string) []byte) {
	t.Helper()
	assertFixture(t)
	getPod := func(name string) core.Pod {
		var pod core.Pod
		if err := json.Unmarshal(run("get", "pod", "-n", ns, name, "-o", "json"), &pod); err != nil {
			t.Fatal(err)
		}
		return pod
	}
	getRole := func(role string) core.Pod {
		var pods core.PodList
		if err := json.Unmarshal(run("get", "pods", "-n", ns, "-l", "app="+role+"-linux", "-o", "json"), &pods); err != nil {
			t.Fatal(err)
		}
		if len(pods.Items) != 1 {
			t.Fatalf("expected one %s-linux pod, got %d", role, len(pods.Items))
		}
		return pods.Items[0]
	}
	client, driver, supervisor := getPod(clientName("linux")), getRole("node"), getRole("mount")
	run("delete", "pod", "-n", ns, supervisor.Name, "--wait=true", "--timeout=120s")
	run("rollout", "status", "-n", ns, "daemonset/mount-linux", "--timeout=120s")
	if getRole("mount").UID == supervisor.UID {
		t.Fatal("mount supervisor was not replaced")
	}
	deadline := time.Now().Add(3 * time.Minute)
	for {
		out, err := kubectl(nil, "exec", "-n", ns, client.Name, "--", "/usr/local/bin/mixed-linux", root+"/mixed", "linux", "verify-final", token)
		t.Logf("Linux manager-only restart readback: %s", out)
		if err == nil && strings.Contains(string(out), "MIXED_COMPLETE:"+token+":linux:verify-final") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("manager-only restart failed to recover original dataset: %v", err)
		}
		time.Sleep(5 * time.Second)
	}
	for _, before := range []core.Pod{client, driver} {
		after := getPod(before.Name)
		if after.UID != before.UID || len(after.Status.ContainerStatuses) != len(before.Status.ContainerStatuses) {
			t.Fatalf("%s was replaced during recovery", before.Name)
		}
		for i, previous := range before.Status.ContainerStatuses {
			current := after.Status.ContainerStatuses[i]
			if current.Name != previous.Name || current.ContainerID != previous.ContainerID || current.RestartCount != previous.RestartCount {
				t.Fatalf("%s container changed during recovery", before.Name)
			}
		}
	}
	t.Log("CSI_LINUX_MANAGER_ONLY_RECOVERY_COMPLETE")
}
