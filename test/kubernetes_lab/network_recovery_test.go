package kubernetes_lab

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	core "k8s.io/api/core/v1"
)

type windowsNetworkSnapshot struct {
	NetworkID   string
	EndpointID  string
	NamespaceID string
}

// Match the native Labcontainers matrix names. Never infer the expected mode
// from the observed HNS type: that would accept a misconfigured fixture.
type csiCNI string

var selectedCNI csiCNI = "calico-vxlan"

func init() {
	flag.Var(&selectedCNI, "csi-cni", "expected matrix CNI: calico-vxlan or calico-bgp")
}

func (c *csiCNI) String() string { return string(*c) }
func (c *csiCNI) Set(value string) error {
	if value != "calico-vxlan" && value != "calico-bgp" {
		return fmt.Errorf("unsupported CSI CNI %q: require calico-vxlan or calico-bgp", value)
	}
	*c = csiCNI(value)
	return nil
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
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	args := append([]string{"kubectl", "exec", "-n", ns, "daemonset/mount-windows", "-c", "plugin", "--"}, ps(windowsNetworkSnapshotScript(id))...)
	out, err = structuredCommandOutput(exec.CommandContext(ctx, "/usr/local/bin/k0s", args...))
	t.Logf("Windows sandbox network evidence: %s", out)
	var snapshot windowsNetworkSnapshot
	if err != nil || json.Unmarshal(out, &snapshot) != nil || snapshot.NetworkID == "" || snapshot.EndpointID == "" || snapshot.NamespaceID == "" {
		t.Fatalf("invalid or unhealthy sandbox network: %v %s", err, out)
	}
	return snapshot
}

func structuredCommandOutput(cmd *exec.Cmd) ([]byte, error) {
	// PowerShell module autoload emits CLIXML progress on stderr even when the
	// observation succeeds. Do not parse stderr as JSON or strip arbitrary lines
	// from stdout; a malformed stdout must still fail the gate.
	out, err := cmd.Output()
	if failure, ok := err.(*exec.ExitError); ok {
		return out, fmt.Errorf("%w: %s", err, failure.Stderr)
	}
	return out, err
}

func TestStructuredObservationSeparatesPowerShellProgress(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("PowerShell required for observation transport contract")
	}
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			script := `[Console]::Error.WriteLine('#< CLIXML'); [Console]::Error.WriteLine('<Objs>progress diagnostic</Objs>'); '{"NetworkID":"network","EndpointID":"endpoint","NamespaceID":"namespace"}'`
			if fail {
				script += "; exit 23"
			}
			out, err := structuredCommandOutput(exec.Command(pwsh, "-NoProfile", "-NonInteractive", "-Command", script))
			if fail {
				if err == nil || !strings.Contains(err.Error(), "progress diagnostic") {
					t.Fatalf("failed observation lost diagnostics: %v %s", err, out)
				}
				return
			}
			var got windowsNetworkSnapshot
			if err != nil || json.Unmarshal(out, &got) != nil || got.NetworkID != "network" {
				t.Fatalf("progress contaminated structured observation: %v %s", err, out)
			}
		})
	}
}

func windowsNetworkSnapshotScript(id string) string {
	networkType := "Overlay"
	if selectedCNI == "calico-bgp" {
		networkType = "L2Bridge"
	}
	return `$sandbox=` + psLiteral(id) + `;
$networks=@(Get-HnsNetwork | Where-Object {$_.Name -eq 'Calico' -and $_.Type -eq ` + psLiteral(networkType) + `});
if($networks.Count -ne 1){throw ` + psLiteral("expected one Calico "+networkType+" network") + `};
$endpoints=@(Get-HnsEndpoint | Where-Object {$_.Name -eq ($sandbox+'_Calico')});
if($endpoints.Count -ne 1){throw 'current sandbox endpoint is missing or ambiguous'};
if($endpoints[0].VirtualNetwork -ne $networks[0].ID){throw 'sandbox endpoint belongs to wrong network'};
$namespaces=@(Get-HnsNamespace | Where-Object {$_.Containers -contains $sandbox});
if($namespaces.Count -ne 1 -or $namespaces[0].IsDefault){throw 'current sandbox namespace is missing, ambiguous or default'};
$refs=@($namespaces[0].ResourceList | Where-Object {$_.Type -eq 'Endpoint'});
if($refs.Count -ne 1 -or $refs[0].Data.Id -ne $endpoints[0].ID){throw 'sandbox namespace contains a dangling or unexpected endpoint'};
[pscustomobject]@{NetworkID=$networks[0].ID;EndpointID=$endpoints[0].ID;NamespaceID=$namespaces[0].ID} | ConvertTo-Json -Compress`
}

// Execute the production observation script against modeled HNS responses.
// These contracts do not substitute for the real reboot gate: they ensure the
// gate cannot accept the dangling namespace observed in the retained lab.
func TestWindowsNetworkSnapshotRejectsDanglingState(t *testing.T) {
	old := selectedCNI
	selectedCNI = "calico-vxlan"
	t.Cleanup(func() { selectedCNI = old })
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("PowerShell required for HNS observation script contracts")
	}
	fixture := `$ErrorActionPreference='Stop';
$network=[pscustomobject]@{Name='Calico';Type='Overlay';ID='network'};
$endpoint=[pscustomobject]@{Name='sandbox_Calico';ID='endpoint';VirtualNetwork='network'};
$namespace=[pscustomobject]@{ID='namespace';IsDefault=$false;Containers=@('sandbox');ResourceList=@([pscustomobject]@{Type='Endpoint';Data=[pscustomobject]@{Id='endpoint'}})};
function Get-HnsNetwork { $network }
function Get-HnsEndpoint { $endpoint }
function Get-HnsNamespace { $namespace }
`
	for _, tc := range []struct {
		name, mutate, want string
	}{
		{"healthy", "", ""},
		{"missing_endpoint", "$endpoint=$null;", "current sandbox endpoint is missing"},
		{"wrong_network", "$endpoint.VirtualNetwork='deleted-network';", "belongs to wrong network"},
		{"dangling_reference", "$namespace.ResourceList[0].Data.Id='deleted-endpoint';", "dangling or unexpected endpoint"},
		{"extra_reference", "$namespace.ResourceList+= $namespace.ResourceList[0];", "dangling or unexpected endpoint"},
		{"default_namespace", "$namespace.IsDefault=$true;", "missing, ambiguous or default"},
		{"wrong_sandbox", "$namespace.Containers=@('old-sandbox');", "missing, ambiguous or default"},
		{"missing_network", "$network=$null;", "expected one Calico Overlay network"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := exec.Command(pwsh, "-NoProfile", "-NonInteractive", "-Command", fixture+tc.mutate+windowsNetworkSnapshotScript("sandbox")).CombinedOutput()
			if tc.want != "" {
				if err == nil || !strings.Contains(string(out), tc.want) {
					t.Fatalf("wanted rejection %q: err=%v output=%s", tc.want, err, out)
				}
				return
			}
			var got windowsNetworkSnapshot
			if err != nil || json.Unmarshal(out, &got) != nil || got != (windowsNetworkSnapshot{"network", "endpoint", "namespace"}) {
				t.Fatalf("healthy observation: err=%v snapshot=%+v output=%s", err, got, out)
			}
		})
	}
}

func TestWindowsNetworkSnapshotExplicitCNI(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("PowerShell required for network flavor contracts")
	}
	selection := flag.Lookup("csi-cni")
	if selection == nil {
		t.Fatal("CSI recovery has no explicit CNI selection; BGP L2bridge cannot be qualified")
	}
	old := selection.Value.String()
	t.Cleanup(func() { _ = selection.Value.Set(old) })
	for _, tc := range []struct {
		cni, network string
		accepted     bool
	}{
		{"calico-vxlan", "Overlay", true},
		{"calico-bgp", "L2Bridge", true},
		{"calico-bgp", "Overlay", false},
		{"calico-vxlan", "L2Bridge", false},
	} {
		t.Run(tc.cni+"/"+tc.network, func(t *testing.T) {
			if err := selection.Value.Set(tc.cni); err != nil {
				t.Fatal(err)
			}
			fixture := `$ErrorActionPreference='Stop';
function Get-HnsNetwork { [pscustomobject]@{Name='Calico';Type=` + psLiteral(tc.network) + `;ID='network'} }
function Get-HnsEndpoint { [pscustomobject]@{Name='sandbox_Calico';ID='endpoint';VirtualNetwork='network'} }
function Get-HnsNamespace { [pscustomobject]@{ID='namespace';IsDefault=$false;Containers=@('sandbox');ResourceList=@([pscustomobject]@{Type='Endpoint';Data=[pscustomobject]@{Id='endpoint'}})} }
`
			out, err := exec.Command(pwsh, "-NoProfile", "-NonInteractive", "-Command", fixture+windowsNetworkSnapshotScript("sandbox")).CombinedOutput()
			if !tc.accepted {
				if err == nil || !strings.Contains(string(out), "expected one Calico") {
					t.Fatalf("wrong network accepted: %v %s", err, out)
				}
				return
			}
			var got windowsNetworkSnapshot
			if err != nil || json.Unmarshal(out, &got) != nil || got != (windowsNetworkSnapshot{"network", "endpoint", "namespace"}) {
				t.Fatalf("valid network rejected: %v %s", err, out)
			}
		})
	}
	for _, invalid := range []string{"", "bogus", "kuberouter", "Calico-BGP"} {
		if err := selection.Value.Set(invalid); err == nil {
			t.Fatalf("invalid CNI %q accepted", invalid)
		}
	}
}

func assertNetworkPreserved(t *testing.T, before, after windowsNetworkSnapshot) {
	t.Helper()
	if !strings.EqualFold(before.NetworkID, after.NetworkID) {
		t.Fatalf("healthy %s network was destroyed across reboot: before=%+v after=%+v", selectedCNI, before, after)
	}
	// A runtime may replace its sandbox across a reboot. captureWindowsNetwork
	// independently requires the resulting namespace's endpoint to exist; do not
	// incorrectly demand immutable container IDs across legitimate replacement.
	t.Logf("network recovery: before=%+v after=%+v", before, after)
}
