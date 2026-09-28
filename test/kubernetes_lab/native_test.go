package kubernetes_lab

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"strings"
	"testing"
	"time"

	core "k8s.io/api/core/v1"
)

func psLiteral(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }

func assertFixture(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/mnt/qualification"); err != nil {
		t.Fatal("offline fixture media missing", err)
	}
	out, err := kubectl(nil, "get", "nodes", "-o", "json")
	var nodes core.NodeList
	if err != nil || json.Unmarshal(out, &nodes) != nil || len(nodes.Items) != 2 {
		t.Fatalf("refusing non-fixture cluster: %v %s", err, out)
	}
	for _, n := range nodes.Items {
		want, known := map[string]string{"linux": "192.0.2.10", "windows": "192.0.2.20"}[n.Name]
		if !known {
			t.Fatal("unexpected node", n.Name)
		}
		found := false
		for _, a := range n.Status.Addresses {
			if a.Type == core.NodeInternalIP && a.Address == want {
				found = true
			}
		}
		if !found {
			t.Fatal("unexpected node address", n.Name)
		}
	}
}

// Reuse the canonical suite's dynamic inventory, policy selection and strict
// PASS/no-SKIP oracle. Do not dot-source its standalone mount bootstrap.
func nativeSuiteScript(root, filerRoot, phase string) string {
	return `$text=[Text.Encoding]::UTF8.GetString([Convert]::FromBase64String([Console]::In.ReadToEnd()));
$tokens=$null; $errors=$null;
$ast=[Management.Automation.Language.Parser]::ParseInput($text,[ref]$tokens,[ref]$errors);
if($errors.Count){throw 'native source parse failed'};
foreach($name in @('Assert','Invoke-NativeMountedSuite')) {
 $functions=@($ast.FindAll({param($n) $n -is [Management.Automation.Language.FunctionDefinitionAst] -and $n.Name -eq $name},$true));
 if($functions.Count -ne 1){throw "expected exactly one function $name"};
 Invoke-Expression $functions[0].Extent.Text
};
$WinFspTestExe='C:\tools\winfsp-csi.test.exe';
$ExpectedWinFspDll='C:\tools\winfsp-x64.dll';
$BasicPermissions=$false; $script:failures=0;
$logDir=Join-Path $env:TEMP ('csi-native-'+[Guid]::NewGuid().ToString('N'));
New-Item -ItemType Directory $logDir | Out-Null;
Invoke-NativeMountedSuite -mnt ` + psLiteral(root) + ` -Phase ` + psLiteral(phase) + ` -FilerEndpoint '192.0.2.10:8888' -FilerRootPrefix ` + psLiteral(filerRoot) + `;
if($script:failures){throw "native suite assertions failed: $script:failures"};
Write-Output ` + psLiteral("CSI_NATIVE_COMPLETE:"+phase)
}

func nativeComplete(output, phase string) bool {
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) == "CSI_NATIVE_COMPLETE:"+phase {
			return true
		}
	}
	return false
}

func TestNativeCompletionEvidence(t *testing.T) {
	for _, tc := range []struct {
		output string
		want   bool
	}{{"PASS\n", false}, {"echo CSI_NATIVE_COMPLETE:write\n", false}, {"CSI_NATIVE_COMPLETE:verify\n", false}, {"CSI_NATIVE_COMPLETE:write\r\n", true}} {
		if got := nativeComplete(tc.output, "write"); got != tc.want {
			t.Fatalf("output %q: got %v want %v", tc.output, got, tc.want)
		}
	}
}

func TestNativeSuiteTransport(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("PowerShell required for the real script transport contract")
	}
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			source := `throw 'standalone bootstrap must never execute'
function Assert([bool]$cond, [string]$what) { if(!$cond){$script:failures++} }
function Invoke-NativeMountedSuite([string]$mnt, [string]$Phase, [string]$FilerEndpoint, [string]$FilerRootPrefix) {
 if($mnt -ne "C:\data\quote'root" -or $Phase -ne 'write' -or $FilerEndpoint -ne '192.0.2.10:8888' -or $FilerRootPrefix -ne "/buckets/pvc/quote'root"){throw 'lost scoped arguments'}
 if($BasicPermissions -or $WinFspTestExe -ne 'C:\tools\winfsp-csi.test.exe' -or $ExpectedWinFspDll -ne 'C:\tools\winfsp-x64.dll'){throw 'wrong CSI policy or tooling'}
 Assert $` + fmt.Sprint(!fail) + ` 'simulated native outcome'
}`
			args := ps(nativeSuiteScript(`C:\data\quote'root`, "/buckets/pvc/quote'root", "write"))
			cmd := exec.Command(pwsh, args[1:]...)
			cmd.Env = append(os.Environ(), "TEMP="+t.TempDir())
			cmd.Stdin = strings.NewReader(base64.StdEncoding.EncodeToString([]byte(source)))
			out, err := cmd.CombinedOutput()
			if (err != nil) != fail || nativeComplete(string(out), "write") == fail {
				t.Fatalf("failure=%v: %v\n%s", fail, err, out)
			}
		})
	}
}

func runCSINative(t *testing.T, root, filerRoot, phase string) {
	t.Helper()
	source, err := os.ReadFile(*smokeSource)
	if err != nil {
		t.Fatal(err)
	}
	out, err := kubectlWithTimeout(25*time.Minute, []byte(base64.StdEncoding.EncodeToString(source)), append([]string{"exec", "-i", "-n", ns, "client-windows", "--"}, ps(nativeSuiteScript(root, filerRoot, phase))...)...)
	t.Logf("CSI native phase %q:\n%s", phase, out)
	if err != nil || !nativeComplete(string(out), phase) {
		t.Fatalf("native phase %q failed or lacked completion evidence: %v", phase, err)
	}
}

func csiFilerRoot(t *testing.T) string {
	t.Helper()
	out, err := kubectl(nil, "get", "pvc", "shared", "-n", ns, "-o", "json")
	var pvc core.PersistentVolumeClaim
	if err != nil || json.Unmarshal(out, &pvc) != nil || pvc.Spec.VolumeName == "" {
		t.Fatalf("cannot resolve lab PVC: %v %s", err, out)
	}
	out, err = kubectl(nil, "get", "pv", pvc.Spec.VolumeName, "-o", "json")
	var pv core.PersistentVolume
	if err != nil || json.Unmarshal(out, &pv) != nil || pv.Spec.CSI == nil || pv.Spec.CSI.Driver != driver || !strings.HasPrefix(pv.Spec.CSI.VolumeHandle, "/buckets/") {
		t.Fatalf("unexpected lab PV: %v %s", err, out)
	}
	return pv.Spec.CSI.VolumeHandle
}

// This targeted entry point exercises the existing CSI mount without rebuilding
// or restarting the lab. Persistence across a reboot is gated by the full test.
func TestCSINativeWinFsp(t *testing.T) {
	if !*live {
		t.Skip("requires disposable mixed-platform Labcontainers Kubernetes fixture")
	}
	assertFixture(t)
	out, err := kubectl(nil, "wait", "-n", ns, "pod/client-windows", "--for=condition=Ready", "--timeout=45s")
	if err != nil {
		t.Fatalf("existing CSI workload is not ready: %v %s", err, out)
	}
	token := fmt.Sprint(time.Now().UnixNano())
	root := `C:\data\qualification-` + token + `\native`
	runCSINative(t, root, path.Join(csiFilerRoot(t), "qualification-"+token, "native"), "")
	fmt.Println("CSI_NATIVE_QUALIFICATION_COMPLETE")
}
