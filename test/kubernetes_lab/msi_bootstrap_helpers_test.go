package kubernetes_lab

// The MSI bootstrap is only linked into the opt-in qualification executable.

import (
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"strings"
	"testing"
	"time"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var candidateMSISHA256 = flag.String("csi-candidate-msi-sha256", "", "opt-in fresh lab MSI install from LCQUAL candidate.msi before candidate qualification")

func msiBootstrapScript(manifest candidateManifest, msiSHA, nativePath, nativeSHA string) (string, error) {
	for _, value := range []string{msiSHA, nativeSHA, manifest.DriverSHA256, manifest.DLLSHA256} {
		if len(value) != 64 {
			return "", fmt.Errorf("MSI bootstrap requires full SHA-256 pins")
		}
		if _, err := hex.DecodeString(value); err != nil {
			return "", err
		}
	}
	if !manifest.LabOnly {
		return "", fmt.Errorf("MSI bootstrap is lab-only")
	}
	if len(manifest.CertificateThumbprint) != 40 {
		return "", fmt.Errorf("missing signer pin")
	}
	if _, err := hex.DecodeString(manifest.CertificateThumbprint); err != nil {
		return "", err
	}
	return `
if(!(Test-Path 'C:\LabQualification\k0s.exe')){throw 'isolated Kubernetes lab prerequisite missing'}
if(Test-Path 'HKLM:\SOFTWARE\WOW6432Node\WinFsp'){throw 'fresh MSI bootstrap refuses an existing installation'}
if(@(Get-Process weed -ErrorAction SilentlyContinue).Count){throw 'mount processes already running'}
$media=@(Get-Volume -FileSystemLabel LCQUAL)
if($media.Count -ne 1){throw 'expected one offline qualification volume'}
$root=$media[0].DriveLetter+':\'
function Assert-Pin($path,$expected) {
 if((Get-FileHash -Algorithm SHA256 -LiteralPath $path).Hash -ine $expected){throw "input hash mismatch: $path"}
}
$msi=Join-Path $root 'candidate.msi'
$native=Join-Path $root 'winfsp-csi-candidate.test.exe'
Assert-Pin $msi ` + psLiteral(msiSHA) + `
Assert-Pin $native ` + psLiteral(nativeSHA) + `
$cert=New-Object Security.Cryptography.X509Certificates.X509Certificate2((Join-Path $root 'candidate.cer'))
if($cert.Thumbprint -ine ` + psLiteral(manifest.CertificateThumbprint) + `){throw 'candidate certificate mismatch'}
if($cert.Subject -ine 'CN=AppMana WinFsp MSI LAB ONLY'){throw 'not an MSI lab certificate'}
$now=[DateTime]::UtcNow
if($now -lt $cert.NotBefore.ToUniversalTime() -or $now -ge $cert.NotAfter.ToUniversalTime()){throw 'certificate outside validity period'}
foreach($store in @('Root','TrustedPublisher')){
 $s=New-Object Security.Cryptography.X509Certificates.X509Store($store,'LocalMachine')
 $s.Open('ReadWrite'); try{$s.Add($cert)}finally{$s.Close()}
}
& bcdedit.exe /set testsigning on
if($LASTEXITCODE -ne 0){throw 'cannot enable lab test signing'}
New-Item -ItemType Directory -Force C:\LabInputs,C:\LabQualification\msi | Out-Null
Copy-Item -LiteralPath $msi C:\LabQualification\msi\candidate.msi
Assert-Pin C:\LabQualification\msi\candidate.msi ` + psLiteral(msiSHA) + `
$p=Start-Process msiexec.exe -Wait -PassThru -ArgumentList '/i','C:\LabQualification\msi\candidate.msi','/qn','/norestart','/l*v','C:\LabQualification\msi\install.log','APPMANA_LAB_ONLY=1','INSTALLLEVEL=1000'
if($p.ExitCode -notin @(0,3010)){Get-Content C:\LabQualification\msi\install.log -Tail 80; throw "MSI installation exit $($p.ExitCode)"}
$install=(Get-ItemProperty 'HKLM:\SOFTWARE\WOW6432Node\WinFsp').InstallDir
Assert-Pin (Join-Path $install 'bin\winfsp-x64.dll') ` + psLiteral(manifest.DLLSHA256) + `
Assert-Pin (Join-Path $install 'bin\winfsp-x64.sys') ` + psLiteral(manifest.DriverSHA256) + `
Copy-Item -LiteralPath $native -Destination ` + psLiteral(nativePath) + `
Assert-Pin ` + psLiteral(nativePath) + ` ` + psLiteral(nativeSHA) + `
Write-Output 'CSI_MSI_INSTALLED_REBOOT_REQUIRED'
`, nil
}

func bootstrapPodResult(p *core.Pod) (bool, error) {
	for _, c := range append(append([]core.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...) {
		if exit := c.State.Terminated; exit != nil && exit.ExitCode != 0 {
			return false, fmt.Errorf("bootstrap pod %s container %s failed: exit=%d reason=%s message=%s", p.Name, c.Name, exit.ExitCode, exit.Reason, exit.Message)
		}
	}
	if p.Status.Phase == core.PodFailed {
		return false, fmt.Errorf("bootstrap pod %s failed: %s %s", p.Name, p.Status.Reason, p.Status.Message)
	}
	return p.Status.Phase == core.PodSucceeded, nil
}

func waitBootstrapPod(t *testing.T, name string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		out, err := kubectlWithTimeout(15*time.Second, nil, "get", "pod", name, "-n", ns, "-o", "json")
		var p core.Pod
		if err == nil {
			err = json.Unmarshal(out, &p)
		}
		if err == nil {
			done, failure := bootstrapPodResult(&p)
			if failure != nil {
				t.Fatal(failure)
			}
			if done {
				return
			}
		}
		last = err
		time.Sleep(time.Second)
	}
	t.Fatalf("bootstrap pod %s did not succeed within %s (last observation error: %v)", name, timeout, last)
}

func bootstrapCandidateMSI(t *testing.T, manifest candidateManifest) {
	t.Helper()
	nativePath, err := candidateNativeInput(*nativeTestExecutable, *candidateNativeTestSHA256)
	if err != nil {
		t.Fatal(err)
	}
	script, err := msiBootstrapScript(manifest, *candidateMSISHA256, nativePath, *candidateNativeTestSHA256)
	if err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) []byte {
		t.Helper()
		out, err := kubectl(nil, args...)
		t.Logf("MSI bootstrap %v: %s", args, out)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	apply := func(object any) {
		t.Helper()
		b, err := json.Marshal(object)
		if err != nil {
			t.Fatal(err)
		}
		out, err := kubectl(b, "apply", "-f", "-")
		if err != nil {
			t.Fatalf("bootstrap apply: %v %s", err, out)
		}
	}
	apply(core.Namespace{TypeMeta: meta.TypeMeta{APIVersion: "v1", Kind: "Namespace"}, ObjectMeta: meta.ObjectMeta{Name: ns}})
	makePod := func(name, script string) core.Pod {
		p := basePod(name, "windows")
		p.Spec.HostNetwork = true
		p.Spec.RestartPolicy = core.RestartPolicyNever
		p.Spec.SecurityContext = &core.PodSecurityContext{WindowsOptions: &core.WindowsSecurityContextOptions{HostProcess: ptr(true), RunAsUserName: ptr(`NT AUTHORITY\SYSTEM`)}}
		p.Spec.Containers = []core.Container{container("bootstrap", windowsImage, ps(script))}
		return p
	}
	// Keep failure evidence until the owning Labcontainers session is collected.
	defer func() {
		for _, name := range []string{"candidate-msi-install", "candidate-msi-reboot"} {
			out, _ := kubectl(nil, "logs", "-n", ns, name)
			t.Logf("%s: %s", name, out)
		}
	}()
	apply(makePod("candidate-msi-install", script))
	waitBootstrapPod(t, "candidate-msi-install", 5*time.Minute)
	if !containsExactLine(string(run("logs", "-n", ns, "candidate-msi-install")), "CSI_MSI_INSTALLED_REBOOT_REQUIRED") {
		t.Fatal("missing install evidence")
	}
	before := strings.TrimSpace(string(run("get", "node", "windows", "-o", "jsonpath={.status.nodeInfo.bootID}")))
	if before == "" {
		t.Fatal("missing pre-reboot identity")
	}
	apply(makePod("candidate-msi-reboot", `& shutdown.exe /r /t 10 /f; if($LASTEXITCODE -ne 0){throw 'reboot request failed'}`))
	waitBootstrapPod(t, "candidate-msi-reboot", 2*time.Minute)
	deadline := time.Now().Add(6 * time.Minute)
	for time.Now().Before(deadline) {
		out, err := kubectlWithTimeout(20*time.Second, nil, "get", "node", "windows", "-o", "json")
		var n core.Node
		if err == nil && json.Unmarshal(out, &n) == nil && n.Status.NodeInfo.BootID != "" && n.Status.NodeInfo.BootID != before {
			for _, c := range n.Status.Conditions {
				if c.Type == core.NodeReady && c.Status == core.ConditionTrue {
					t.Log("CSI_MSI_NEW_BOOT_READY")
					return
				}
			}
		}
		time.Sleep(5 * time.Second)
	}
	t.Fatal("MSI bootstrap did not return ready with a new boot identity")
}
