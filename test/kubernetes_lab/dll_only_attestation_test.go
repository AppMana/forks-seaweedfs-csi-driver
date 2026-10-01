package kubernetes_lab

import (
	"flag"
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

var dllOnlySHA256 = flag.String("csi-winfsp-dll-sha256", "", "explicit app-local DLL SHA256; requires official signed stock driver and normal boot policy")

const stockDriverSHA256 = "03553fffacd362f4a9a08c00b4f236a82354a183bc6028494fb32055386e13c9"

const dllOnlyEvidence = `
function Assert-DllOnlyEvidence($modules,$drivers,$expectedPath,$expectedHash) {
 if(@($modules).Count -lt 1){throw 'no mounted DLL'}
 foreach($m in $modules){
  if($m.Path -ine $expectedPath -or $m.Hash -ine $expectedHash){throw 'wrong app-local DLL'}
 }
 if(@($drivers).Count -ne 1){throw 'expected exactly one stock driver'}
 $d=@($drivers)[0]
 if($d.Hash -ine '` + stockDriverSHA256 + `' -or $d.Status -ne 'Valid' -or $d.Subject -notmatch 'CN=Microsoft Windows Hardware Compatibility Publisher(,|$)'){throw 'wrong or untrusted stock driver'}
}
`

func nonCandidateAttestation() (string, string, error) {
	if *dllOnlySHA256 == "" {
		return stockAttestation, "STOCK_WINFSP_ATTESTED", nil
	}
	if len(*dllOnlySHA256) != 64 || strings.Trim(*dllOnlySHA256, "0123456789abcdef") != "" {
		return "", "", fmt.Errorf("DLL-only qualification requires a lowercase SHA256")
	}
	if *candidateManifestPath != "" || *candidateManifestSHA256 != "" || *candidateMSISHA256 != "" {
		return "", "", fmt.Errorf("DLL-only qualification cannot select a lab driver candidate")
	}
	native, err := candidateNativeInput(*nativeTestExecutable, *candidateNativeTestSHA256)
	if err != nil {
		return "", "", err
	}
	script := bootPolicy + dllOnlyEvidence + `
$boot=& bcdedit /enum '{current}'
if($LASTEXITCODE -ne 0){throw 'cannot attest boot policy'}
Assert-DriverTrustPolicy $boot
$expectedPath=Join-Path $env:CONTAINER_SANDBOX_MOUNT_POINT 'winfsp-x64.dll'
$modules=@(Get-Process weed -ErrorAction Stop | ForEach-Object { $_.Modules } | Where-Object ModuleName -ieq 'winfsp-x64.dll' | ForEach-Object { [pscustomobject]@{Path=$_.FileName;Hash=(Get-FileHash $_.FileName -Algorithm SHA256).Hash} })
$drivers=@(Get-CimInstance Win32_SystemDriver | Where-Object {$_.Name -like 'WinFsp*' -and $_.State -eq 'Running'} | ForEach-Object {
 $path=$_.PathName.Trim('"')
 if($path.StartsWith('\??\') -or $path.StartsWith('\\?\')){$path=$path.Substring(4)}
 $sig=Get-AuthenticodeSignature $path
 [pscustomobject]@{Path=$path;Hash=(Get-FileHash $path -Algorithm SHA256).Hash;Status=[string]$sig.Status;Subject=$sig.SignerCertificate.Subject}
})
Assert-DllOnlyEvidence $modules $drivers $expectedPath ` + psLiteral(*dllOnlySHA256) + `
if((Get-FileHash -Algorithm SHA256 ` + psLiteral(native) + `).Hash -ine ` + psLiteral(*candidateNativeTestSHA256) + `){throw 'native test executable hash mismatch'}
if((Get-FileHash -Algorithm SHA256 'C:\LabInputs\winfsp-x64.dll').Hash -ine ` + psLiteral(*dllOnlySHA256) + `){throw 'native test DLL hash mismatch'}
$modules | Format-List
$drivers | Format-List
Write-Output 'DLL_ONLY_STOCK_DRIVER_ATTESTED'
`
	return script, "DLL_ONLY_STOCK_DRIVER_ATTESTED", nil
}

// Keep the existing lab-driver lane strict while also permitting the explicitly
// pinned DLL-only lane. An omitted candidate is not permission to skip evidence.
func persistenceAttestation() (string, string, error) {
	if *dllOnlySHA256 != "" {
		return nonCandidateAttestation()
	}
	manifest, err := loadCandidateManifest(*candidateManifestPath, *candidateManifestSHA256)
	if err != nil {
		return "", "", err
	}
	native, err := candidateNativeInput(*nativeTestExecutable, *candidateNativeTestSHA256)
	if err != nil {
		return "", "", err
	}
	return candidateAttestation(manifest, native, *candidateNativeTestSHA256), "CANDIDATE_WINFSP_ATTESTED:" + manifest.DriverSourceRevision + ":" + manifest.DriverSourceArchiveSHA256, nil
}

func TestDllOnlyEvidenceRejectsSubstitutions(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("requires PowerShell")
	}
	for _, tc := range []struct {
		name, path, dll, driver, signature string
		pass                               bool
	}{
		{"matching", "C:\\image\\winfsp-x64.dll", "DLL", stockDriverSHA256, "Valid", true},
		{"host fallback", "C:\\stock\\winfsp-x64.dll", "DLL", stockDriverSHA256, "Valid", false},
		{"wrong dll", "C:\\image\\winfsp-x64.dll", "OTHER", stockDriverSHA256, "Valid", false},
		{"lab driver", "C:\\image\\winfsp-x64.dll", "DLL", "LAB", "Valid", false},
		{"unsigned", "C:\\image\\winfsp-x64.dll", "DLL", stockDriverSHA256, "NotSigned", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := "$ErrorActionPreference='Stop';" + dllOnlyEvidence + ";Assert-DllOnlyEvidence @([pscustomobject]@{Path=" + psLiteral(tc.path) + ";Hash=" + psLiteral(tc.dll) + "}) @([pscustomobject]@{Hash=" + psLiteral(tc.driver) + ";Status=" + psLiteral(tc.signature) + ";Subject='CN=Microsoft Windows Hardware Compatibility Publisher, O=Microsoft Corporation'}) 'C:\\image\\winfsp-x64.dll' 'DLL'"
			out, err := exec.Command(pwsh, "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
			if (err == nil) != tc.pass {
				t.Fatalf("pass=%v err=%v output=%s", tc.pass, err, out)
			}
		})
	}
}

func TestDllOnlySelectionFailsClosed(t *testing.T) {
	flags := []*string{dllOnlySHA256, candidateManifestPath, candidateManifestSHA256, candidateMSISHA256, nativeTestExecutable, candidateNativeTestSHA256}
	for _, p := range flags {
		p, previous := p, *p
		t.Cleanup(func() { *p = previous })
		*p = ""
	}
	if _, _, err := persistenceAttestation(); err == nil {
		t.Fatal("missing candidate evidence accepted")
	}
	*dllOnlySHA256 = strings.Repeat("a", 64)
	if _, _, err := persistenceAttestation(); err == nil {
		t.Fatal("missing native test pin accepted")
	}
	*nativeTestExecutable = `C:\tools\native.test.exe`
	*candidateNativeTestSHA256 = strings.Repeat("b", 64)
	script, marker, err := persistenceAttestation()
	if err != nil || marker != "DLL_ONLY_STOCK_DRIVER_ATTESTED" {
		t.Fatalf("explicit DLL lane rejected: %s %v", marker, err)
	}
	for _, required := range []string{"Assert-DriverTrustPolicy $boot", "native test executable hash mismatch", "native test DLL hash mismatch", stockDriverSHA256} {
		if !strings.Contains(script, required) {
			t.Fatal("missing attestation", required)
		}
	}
	for _, p := range []*string{candidateManifestPath, candidateManifestSHA256, candidateMSISHA256} {
		*p = "candidate"
		if _, _, err := persistenceAttestation(); err == nil {
			t.Fatal("mixed DLL-only and lab-driver options accepted")
		}
		*p = ""
	}
	for _, bad := range []string{"abc", strings.Repeat("A", 64), strings.Repeat("g", 64)} {
		*dllOnlySHA256 = bad
		if _, _, err := persistenceAttestation(); err == nil {
			t.Fatal("invalid DLL pin accepted", bad)
		}
	}
}

func TestDllOnlyEvidenceRequiresUniqueSignedDriver(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("requires PowerShell")
	}
	for _, tc := range []struct{ name, mutation string }{
		{"no DLL", "$modules=@()"},
		{"no driver", "$drivers=@()"},
		{"two drivers", "$drivers=@($driver,$driver)"},
		{"wrong signer", "$driver.Subject='CN=AppMana LAB ONLY'"},
		{"extra wrong DLL", "$modules+= [pscustomobject]@{Path='C:\\other\\winfsp-x64.dll';Hash='DLL'}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := "$ErrorActionPreference='Stop';" + dllOnlyEvidence + `
$modules=@([pscustomobject]@{Path='C:\image\winfsp-x64.dll';Hash='DLL'})
$driver=[pscustomobject]@{Hash='` + stockDriverSHA256 + `';Status='Valid';Subject='CN=Microsoft Windows Hardware Compatibility Publisher, O=Microsoft Corporation'}
$drivers=@($driver)
` + tc.mutation + `
Assert-DllOnlyEvidence $modules $drivers 'C:\image\winfsp-x64.dll' 'DLL'
`
			out, err := exec.Command(pwsh, "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
			if err == nil {
				t.Fatalf("invalid evidence accepted: %s", out)
			}
		})
	}
}
