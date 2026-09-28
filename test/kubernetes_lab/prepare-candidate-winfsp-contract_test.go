package kubernetes_lab

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestCandidatePreparationScriptIsFailClosedAndNonDestructive(t *testing.T) {
	b, err := os.ReadFile("prepare-candidate-winfsp.ps1")
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	for _, required := range []string{
		"ManifestSHA256", "InstallerSHA256", "StockMSISHA256", "NativeTestSHA256",
		"driver_source_revision", "driver_source_archive_sha256", "certificate_thumbprint",
		"source.zip", "driver-source.zip",
		"Get-AuthenticodeSignature", "AppMana WinFsp LAB ONLY", "Assert-CandidateManifest",
		"clean stock InstallDir", "existing stock WinFsp DLL is not validly signed",
		"WinFsp service exists without its stock installation registry state", "stock WinFsp bootstrap did not complete",
		"C:\\WinFspCandidates", "winfsp-csi-candidate.test.exe",
		"CSI_CANDIDATE_PREPARED_REBOOT_REQUIRED",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("candidate preparation contract missing %q", required)
		}
	}
	for _, forbidden := range []string{
		`Copy-Item $candidateDll (Join-Path $stockRoot 'bin\\winfsp-x64.dll')`,
		`Remove-Item`, `shutdown.exe`,
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("candidate preparation contains unsafe operation %q", forbidden)
		}
	}
}

func TestCandidatePreparationStockBootstrapBehavior(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("PowerShell required")
	}
	for _, tc := range []struct {
		name, args, expected string
		want                 bool
	}{
		{"fresh absent", "$false '' ''", "absent", true},
		{"clean signed stock", "$true 'C:\\Program Files (x86)\\WinFsp\\' 'Valid'", "present", true},
		{"partial missing path", "$true '' ''", "", false},
		{"candidate already selected", "$true 'C:\\WinFspCandidates\\abc\\' 'Valid'", "", false},
		{"unsigned stock", "$true 'C:\\Program Files (x86)\\WinFsp\\' 'NotSigned'", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := `$text=Get-Content -LiteralPath 'prepare-candidate-winfsp.ps1' -Raw;$tokens=$null;$errors=$null;$ast=[Management.Automation.Language.Parser]::ParseInput($text,[ref]$tokens,[ref]$errors);if($errors.Count){throw 'parse failed'};$f=@($ast.FindAll({param($n)$n -is [Management.Automation.Language.FunctionDefinitionAst] -and $n.Name -eq 'Assert-CleanStockInstall'},$true));if($f.Count -ne 1){throw 'function missing'};Invoke-Expression $f[0].Extent.Text;$state=Assert-CleanStockInstall ` + tc.args + `;if($state -cne '` + tc.expected + `'){throw "unexpected state: $state"}`
			out, err := exec.Command(pwsh, "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
			if (err == nil) != tc.want {
				t.Fatalf("accepted=%v error=%v output=%s", err == nil, err, out)
			}
		})
	}
}

func TestCandidatePreparationManifestBehavior(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("PowerShell required")
	}
	path := "prepare-candidate-winfsp.ps1"
	base := `@{lab_only=$true;source_revision='aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa';driver_source_revision='aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa';source_archive_sha256='bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb';driver_source_archive_sha256='bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb';driver_sha256='cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc';dll_sha256='dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd';certificate_thumbprint='581769D4AB466DF1B61FA2F7B4FD9B04C98D66E2'}`
	for _, tc := range []struct {
		name, mutation string
		want           bool
	}{
		{"matched", "", true},
		{"not lab only", `$m.lab_only=$false`, false},
		{"moving revision", `$m.source_revision='HEAD'`, false},
		{"missing archive", `$m.driver_source_archive_sha256=''`, false},
		{"bad signer", `$m.certificate_thumbprint='not-a-thumbprint'`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := `$text=Get-Content -LiteralPath '` + path + `' -Raw;$tokens=$null;$errors=$null;$ast=[Management.Automation.Language.Parser]::ParseInput($text,[ref]$tokens,[ref]$errors);if($errors.Count){throw 'parse failed'};$f=@($ast.FindAll({param($n)$n -is [Management.Automation.Language.FunctionDefinitionAst] -and $n.Name -eq 'Assert-CandidateManifest'},$true));if($f.Count -ne 1){throw 'function missing'};Invoke-Expression $f[0].Extent.Text;$m=[pscustomobject]` + base + `;` + tc.mutation + `;Assert-CandidateManifest $m`
			out, err := exec.Command(pwsh, "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
			if (err == nil) != tc.want {
				t.Fatalf("accepted=%v error=%v output=%s", err == nil, err, out)
			}
		})
	}
}

func TestCandidatePreparationBootPolicyBehavior(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("PowerShell required")
	}
	for _, tc := range []struct {
		name, lines string
		want        bool
	}{
		{"realistic enabled array", `@('Windows Boot Loader','-------------------','identifier {current}','path \\Windows\\system32\\winload.exe','testsigning Yes','nx OptIn')`, true},
		{"disabled", `@('identifier {current}','testsigning No','nx OptIn')`, false},
		{"missing", `@('identifier {current}','nx OptIn')`, false},
		{"integrity bypass", `@('identifier {current}','testsigning Yes','nointegritychecks Yes')`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := `$text=Get-Content -LiteralPath 'prepare-candidate-winfsp.ps1' -Raw;$tokens=$null;$errors=$null;$ast=[Management.Automation.Language.Parser]::ParseInput($text,[ref]$tokens,[ref]$errors);if($errors.Count){throw 'parse failed'};$f=@($ast.FindAll({param($n)$n -is [Management.Automation.Language.FunctionDefinitionAst] -and $n.Name -eq 'Assert-CandidateBootPolicy'},$true));if($f.Count -ne 1){throw 'function missing'};Invoke-Expression $f[0].Extent.Text;Assert-CandidateBootPolicy ` + tc.lines
			out, err := exec.Command(pwsh, "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
			if (err == nil) != tc.want {
				t.Fatalf("accepted=%v error=%v output=%s", err == nil, err, out)
			}
		})
	}
}
