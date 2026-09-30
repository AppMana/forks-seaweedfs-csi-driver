package kubernetes_lab

import (
	"flag"
	"os/exec"
	"strings"
	"testing"

	core "k8s.io/api/core/v1"
)

func TestBootstrapPodFailureIsTerminal(t *testing.T) {
	for _, tc := range []struct {
		phase        core.PodPhase
		exit         *int32
		done, failed bool
	}{
		{core.PodPending, nil, false, false},
		{core.PodRunning, nil, false, false},
		{core.PodSucceeded, ptr(int32(0)), true, false},
		{core.PodFailed, nil, false, true},
		{core.PodFailed, ptr(int32(1)), false, true},
		{core.PodRunning, ptr(int32(17)), false, true},
	} {
		p := &core.Pod{Status: core.PodStatus{Phase: tc.phase}}
		if tc.exit != nil {
			p.Status.ContainerStatuses = []core.ContainerStatus{{Name: "bootstrap", State: core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: *tc.exit}}}}
		}
		done, err := bootstrapPodResult(p)
		if done != tc.done || (err != nil) != tc.failed {
			t.Fatalf("phase=%s exit=%v: done=%v err=%v", tc.phase, tc.exit, done, err)
		}
	}
}

func TestMSIBootstrapRequiresExplicitPinnedOptIn(t *testing.T) {
	f := flag.Lookup("csi-candidate-msi-sha256")
	if f == nil {
		t.Fatal("missing actual MSI bootstrap lane")
	}
	if f.DefValue != "" {
		t.Fatal("MSI bootstrap must not run implicitly")
	}
}

func TestMSIBootstrapCertificatePolicy(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("PowerShell required")
	}
	manifest := candidateManifest{LabOnly: true, CertificateThumbprint: strings.Repeat("a", 40), DriverSHA256: strings.Repeat("a", 64), DLLSHA256: strings.Repeat("b", 64)}
	script, err := msiBootstrapScript(manifest, strings.Repeat("c", 64), `C:\LabInputs\candidate.test.exe`, strings.Repeat("d", 64))
	if err != nil {
		t.Fatal(err)
	}
	start, end := strings.Index(script, "if($cert.Thumbprint"), strings.Index(script, "foreach($store")
	if start < 0 || end <= start {
		t.Fatal("certificate checks not found")
	}
	for _, tc := range []struct {
		name, subject, thumb, before, after string
		wantOK                              bool
	}{
		{"packaging-signer", "CN=AppMana WinFsp MSI LAB ONLY", manifest.CertificateThumbprint, "-1", "1", true},
		{"manual-driver-signer", "CN=AppMana WinFsp LAB ONLY", manifest.CertificateThumbprint, "-1", "1", false},
		{"non-lab", "CN=Production", manifest.CertificateThumbprint, "-1", "1", false},
		{"wrong-thumbprint", "CN=AppMana WinFsp MSI LAB ONLY", strings.Repeat("b", 40), "-1", "1", false},
		{"expired", "CN=AppMana WinFsp MSI LAB ONLY", manifest.CertificateThumbprint, "-2", "-1", false},
		{"future", "CN=AppMana WinFsp MSI LAB ONLY", manifest.CertificateThumbprint, "1", "2", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `$ErrorActionPreference='Stop'; $now=[DateTime]::UtcNow; $cert=[pscustomobject]@{Subject=` + psLiteral(tc.subject) + `; Thumbprint=` + psLiteral(tc.thumb) + `; NotBefore=$now.AddHours(` + tc.before + `); NotAfter=$now.AddHours(` + tc.after + `)}; ` + script[start:end] + `; Write-Output 'CERTIFICATE_ACCEPTED'`
			out, err := exec.Command(pwsh, "-NoProfile", "-NonInteractive", "-Command", body).CombinedOutput()
			if (err == nil) != tc.wantOK || (tc.wantOK && !strings.Contains(string(out), "CERTIFICATE_ACCEPTED")) {
				t.Fatalf("accept=%v want=%v: %v %s", err == nil, tc.wantOK, err, out)
			}
		})
	}
}

func TestMSIBootstrapPinsAndPowerShellSyntax(t *testing.T) {
	manifest := candidateManifest{LabOnly: true, CertificateThumbprint: strings.Repeat("a", 40), DriverSHA256: strings.Repeat("a", 64), DLLSHA256: strings.Repeat("b", 64)}
	script, err := msiBootstrapScript(manifest, strings.Repeat("c", 64), `C:\LabInputs\candidate.test.exe`, strings.Repeat("d", 64))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"refuses an existing installation", "Get-FileHash", "APPMANA_LAB_ONLY=1", "/norestart", "CSI_MSI_INSTALLED_REBOOT_REQUIRED", "testsigning on"} {
		if !strings.Contains(script, required) {
			t.Fatal("missing safety contract", required)
		}
	}
	for _, forbidden := range []string{"nointegritychecks", "Set-ItemProperty", "sc.exe", "regsvr32", "Invoke-WebRequest"} {
		if strings.Contains(script, forbidden) {
			t.Fatal("bootstrap bypasses MSI or offline input", forbidden)
		}
	}
	for _, pin := range []string{"", strings.Repeat("z", 64), strings.Repeat("a", 63)} {
		if _, err := msiBootstrapScript(manifest, pin, `C:\LabInputs\candidate.test.exe`, strings.Repeat("d", 64)); err == nil {
			t.Fatal("invalid MSI digest accepted")
		}
	}
	manifest.LabOnly = false
	if _, err := msiBootstrapScript(manifest, strings.Repeat("c", 64), `C:\LabInputs\candidate.test.exe`, strings.Repeat("d", 64)); err == nil {
		t.Fatal("non-lab package accepted")
	}
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("PowerShell parser required")
	}
	cmd := exec.Command(pwsh, "-NoProfile", "-Command", `$tokens=$null; $errors=$null; $null=[Management.Automation.Language.Parser]::ParseInput([Console]::In.ReadToEnd(),[ref]$tokens,[ref]$errors); if($errors.Count){$errors | Format-List; exit 1}`)
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("PowerShell syntax: %v %s", err, out)
	}
}
