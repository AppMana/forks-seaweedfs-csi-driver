package kubernetes_lab

import (
	"encoding/json"
	"flag"
	"testing"

	apps "k8s.io/api/apps/v1"
)

var windowsUpgradePhase = flag.String("csi-windows-upgrade-phase", "", "explicit retained-lab mount maintenance: quiesce or restore; never changes backend/PVC")

// Keep MSI changes outside the data test. Quiesce ordinary consumers before
// stopping the mount service; restore only the mount DaemonSet with exact image
// pins. The existing persistence verifier must then use the original dataset.
func TestRetainedWindowsMountUpgrade(t *testing.T) {
	if !*live {
		t.Skip("requires isolated retained Labcontainers fixture")
	}
	if *windowsUpgradePhase != "quiesce" && *windowsUpgradePhase != "restore" {
		t.Fatal("explicit quiesce or restore required")
	}
	if !existingVolumePattern.MatchString(*existingFilerRoot) {
		t.Fatal("original PVC volume handle required")
	}
	assertFixture(t)
	if csiFilerRoot(t) != *existingFilerRoot {
		t.Fatal("original PVC replaced")
	}
	run := func(body []byte, args ...string) []byte {
		t.Helper()
		out, err := kubectl(body, args...)
		t.Logf("%v\n%s", args, out)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	var ds apps.DaemonSet
	if err := json.Unmarshal(run(nil, "get", "daemonset/mount-windows", "-n", ns, "-o", "json"), &ds); err != nil {
		t.Fatal(err)
	}
	if *windowsUpgradePhase == "quiesce" {
		run(nil, "delete", "pod/"+clientName("windows"), "-n", ns, "--wait=true", "--timeout=180s")
		// No node in the fixture has this impossible node-name combination.
		// Keep the original OS selector and all configuration for inspection.
		if ds.Spec.Template.Spec.NodeSelector == nil {
			ds.Spec.Template.Spec.NodeSelector = map[string]string{}
		}
		ds.Spec.Template.Spec.NodeSelector["kubernetes.io/hostname"] = "qualification-paused"
	} else {
		if *dllOnlySHA256 == "" || *splitImages["mount-windows"] == "" {
			t.Fatal("DLL-only image and hash required for this upgrade")
		}
		if err := validateSplitImages(); err != nil {
			t.Fatal(err)
		}
		if _, _, err := nonCandidateAttestation(); err != nil {
			t.Fatal(err)
		}
		ds.Spec = daemon(nodePodForQualification("windows", true, false)).Spec
	}
	body, err := json.Marshal(ds)
	if err != nil {
		t.Fatal(err)
	}
	run(body, "replace", "-f", "-")
	if *windowsUpgradePhase == "quiesce" {
		run(nil, "wait", "pod", "-n", ns, "-l", "app=mount-windows", "--for=delete", "--timeout=180s")
	} else {
		run(nil, "rollout", "status", "daemonset/mount-windows", "-n", ns, "--timeout=180s")
	}
}
