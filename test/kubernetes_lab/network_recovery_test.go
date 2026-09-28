package kubernetes_lab

import (
	"encoding/json"
	"strings"
	"testing"

	core "k8s.io/api/core/v1"
)

type windowsNetworkSnapshot struct {
	NetworkID   string
	EndpointID  string
	NamespaceID string
}

// Inspect only this test's sandbox. An unrelated retained failed namespace is
// not silently cleaned up, nor allowed to stand in for this workload's health.
func captureWindowsNetwork(t *testing.T) windowsNetworkSnapshot {
	t.Helper()
	out, err := kubectl(nil, "get", "pod", clientName("windows"), "-n", ns, "-o", "json")
	var pod core.Pod
	if err != nil || json.Unmarshal(out, &pod) != nil {
		t.Fatalf("read workload sandbox: %v %s", err, out)
	}
	id := pod.Annotations["cni.projectcalico.org/containerID"]
	if id == "" {
		t.Fatal("workload has no Calico sandbox identity")
	}
	script := `$sandbox=` + psLiteral(id) + `;
$networks=@(Get-HnsNetwork | Where-Object {$_.Name -eq 'Calico' -and $_.Type -eq 'Overlay'});
if($networks.Count -ne 1){throw 'expected one Calico Overlay network'};
$endpoints=@(Get-HnsEndpoint | Where-Object {$_.Name -eq ($sandbox+'_Calico')});
if($endpoints.Count -ne 1){throw 'current sandbox endpoint is missing or ambiguous'};
if($endpoints[0].VirtualNetwork -ne $networks[0].ID){throw 'sandbox endpoint belongs to wrong network'};
$namespaces=@(Get-HnsNamespace | Where-Object {$_.Containers -contains $sandbox});
if($namespaces.Count -ne 1 -or $namespaces[0].IsDefault){throw 'current sandbox namespace is missing, ambiguous or default'};
$refs=@($namespaces[0].ResourceList | Where-Object {$_.Type -eq 'Endpoint'});
if($refs.Count -ne 1 -or $refs[0].Data.Id -ne $endpoints[0].ID){throw 'sandbox namespace contains a dangling or unexpected endpoint'};
[pscustomobject]@{NetworkID=$networks[0].ID;EndpointID=$endpoints[0].ID;NamespaceID=$namespaces[0].ID} | ConvertTo-Json -Compress`
	out, err = kubectl(nil, append([]string{"exec", "-n", ns, "daemonset/mount-windows", "-c", "plugin", "--"}, ps(script)...)...)
	t.Logf("Windows sandbox network evidence: %s", out)
	var snapshot windowsNetworkSnapshot
	if err != nil || json.Unmarshal(out, &snapshot) != nil || snapshot.NetworkID == "" || snapshot.EndpointID == "" || snapshot.NamespaceID == "" {
		t.Fatalf("invalid or unhealthy sandbox network: %v %s", err, out)
	}
	return snapshot
}

func assertNetworkPreserved(t *testing.T, before, after windowsNetworkSnapshot) {
	t.Helper()
	if !strings.EqualFold(before.NetworkID, after.NetworkID) {
		t.Fatalf("healthy Overlay was destroyed across reboot: before=%+v after=%+v", before, after)
	}
	// A runtime may replace its sandbox across a reboot. captureWindowsNetwork
	// independently requires the resulting namespace's endpoint to exist; do not
	// incorrectly demand immutable container IDs across legitimate replacement.
	t.Logf("network recovery: before=%+v after=%+v", before, after)
}
