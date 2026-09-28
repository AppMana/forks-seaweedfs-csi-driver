package kubernetes_lab

import (
	"os/exec"
	"strings"
	"testing"
)

func TestPowerShellContainerTransport(t *testing.T) {
	path, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("PowerShell required")
	}
	args := ps("function Example {\n Write-Output 'quotes \" braces {} λ'\n}; Example")
	if args[3] != "-EncodedCommand" {
		t.Fatal("container command must not contain raw multiline script")
	}
	out, err := exec.Command(path, args[1:]...).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "quotes \" braces {} λ") {
		t.Fatalf("transport: %v %s", err, out)
	}
}

const bootPolicy = `function Assert-DriverTrustPolicy($boot) {
 foreach($line in $boot) {
  if($line -match '^\s*(testsigning|nointegritychecks)\s+Yes\s*$') {
   throw 'test driver trust policy is enabled'
  }
 }
}`

const stockAttestation = bootPolicy + `
$boot=& bcdedit /enum '{current}'
if($LASTEXITCODE -ne 0){throw 'cannot attest boot policy'}
$boot
Assert-DriverTrustPolicy $boot
$root=(Get-ItemProperty 'HKLM:\SOFTWARE\WOW6432Node\WinFsp').InstallDir
if(!$root){throw 'missing stock installation'}
$dlls=@(Get-Process weed -ErrorAction Stop | ForEach-Object { $_.Modules } | Where-Object {$_.ModuleName -ieq 'winfsp-x64.dll'})
if(!$dlls.Count){throw 'no mounted WinFsp DLL loaded'}
foreach($dll in $dlls){
 if(!$dll.FileName.StartsWith($root,[StringComparison]::OrdinalIgnoreCase)){throw "non-stock DLL path $($dll.FileName)"}
 $sig=Get-AuthenticodeSignature $dll.FileName
 if($sig.Status -ne 'Valid'){throw "DLL signature $($sig.Status)"}
 Get-FileHash $dll.FileName
 $sig.SignerCertificate.Subject
}
$drivers=@(Get-CimInstance Win32_SystemDriver | Where-Object {$_.Name -like 'WinFsp*' -and $_.State -eq 'Running'})
if($drivers.Count -ne 1){throw 'expected one loaded WinFsp driver'}
foreach($d in $drivers){
 $path=$d.PathName.Trim('"')
 if($path.StartsWith('\??\') -or $path.StartsWith('\\?\')){$path=$path.Substring(4)}
 $sig=Get-AuthenticodeSignature $path
 if($sig.Status -ne 'Valid'){throw "driver signature $($sig.Status): $path"}
 Get-FileHash $path
 $sig.SignerCertificate.Subject
}
Write-Output 'STOCK_WINFSP_ATTESTED'
`

func TestBootPolicyRejectsTestSigning(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("PowerShell required for boot-policy parser contract")
	}
	for _, tc := range []struct {
		line    string
		allowed bool
	}{{"testsigning             Yes", false}, {"nointegritychecks       Yes", false}, {"testsigning             No", true}, {"identifier              {current}", true}} {
		t.Run(tc.line, func(t *testing.T) {
			out, err := exec.Command(pwsh, "-NoProfile", "-NonInteractive", "-Command", "$ErrorActionPreference='Stop'; "+bootPolicy+"; Assert-DriverTrustPolicy @('Windows Boot Loader','"+tc.line+"')").CombinedOutput()
			if (err == nil) != tc.allowed {
				t.Fatalf("allowed=%v error=%v output=%s", tc.allowed, err, out)
			}
		})
	}
}
