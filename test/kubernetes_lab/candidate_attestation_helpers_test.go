package kubernetes_lab

// These helpers belong to the test harness, not a separately buildable package.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

func candidateNativeInput(executable, expectedSHA256 string) (string, error) {
	const prefix = `C:\tools\`
	if !strings.HasPrefix(strings.ToLower(executable), strings.ToLower(prefix)) {
		return "", fmt.Errorf("candidate native executable must be in the read-only C:\\tools mount")
	}
	name := executable[len(prefix):]
	if name == "" || strings.ContainsAny(name, `\/:`) {
		return "", fmt.Errorf("candidate native executable must be a direct C:\\tools child")
	}
	if len(expectedSHA256) != 64 {
		return "", fmt.Errorf("candidate native executable SHA-256 is required")
	}
	if _, err := hex.DecodeString(expectedSHA256); err != nil {
		return "", fmt.Errorf("invalid candidate native executable SHA-256")
	}
	return `C:\LabInputs\` + name, nil
}

type candidateManifest struct {
	CertificateThumbprint     string `json:"certificate_thumbprint"`
	DriverSourceRevision      string `json:"driver_source_revision"`
	DriverSourceArchiveSHA256 string `json:"driver_source_archive_sha256"`
	DLLSHA256                 string `json:"dll_sha256"`
	DriverSHA256              string `json:"driver_sha256"`
	LabOnly                   bool   `json:"lab_only"`
}

func containsExactLine(output, marker string) bool {
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) == marker {
			return true
		}
	}
	return false
}

func loadCandidateManifest(path, expectedSHA256 string) (candidateManifest, error) {
	var manifest candidateManifest
	if path == "" || expectedSHA256 == "" {
		return manifest, fmt.Errorf("candidate manifest path and SHA-256 are both required")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return manifest, err
	}
	actual := sha256.Sum256(b)
	if !strings.EqualFold(hex.EncodeToString(actual[:]), expectedSHA256) {
		return manifest, fmt.Errorf("candidate manifest SHA-256 mismatch")
	}
	decoder := json.NewDecoder(strings.NewReader(string(b)))
	if err := decoder.Decode(&manifest); err != nil {
		return manifest, fmt.Errorf("decode candidate manifest: %w", err)
	}
	if !manifest.LabOnly {
		return manifest, fmt.Errorf("candidate manifest must identify a lab-only build")
	}
	for name, value := range map[string]string{
		"certificate_thumbprint":       manifest.CertificateThumbprint,
		"driver_source_revision":       manifest.DriverSourceRevision,
		"driver_source_archive_sha256": manifest.DriverSourceArchiveSHA256,
		"dll_sha256":                   manifest.DLLSHA256,
		"driver_sha256":                manifest.DriverSHA256,
	} {
		want := 64
		if name == "certificate_thumbprint" || name == "driver_source_revision" {
			want = 40
		}
		if len(value) != want {
			return manifest, fmt.Errorf("invalid %s", name)
		}
		if _, err := hex.DecodeString(value); err != nil {
			return manifest, fmt.Errorf("invalid %s", name)
		}
	}
	return manifest, nil
}

const candidateAttestationFunctions = `function Assert-CandidateBootPolicy($boot) {
 $testSigning=$false
 foreach($line in $boot) {
  if($line -match '^\s*testsigning\s+Yes\s*$'){$testSigning=$true}
  if($line -match '^\s*nointegritychecks\s+Yes\s*$'){throw 'nointegritychecks is enabled'}
 }
 if(!$testSigning){throw 'candidate lab driver requires testsigning'}
}
function Assert-CandidateEvidence($boot,$dlls,$drivers,$expectedDllHash,$expectedDriverHash,$expectedThumbprint) {
 Assert-CandidateBootPolicy $boot
 if(!$dlls -or @($dlls).Count -lt 1){throw 'no mounted WinFsp DLL loaded'}
 foreach($dll in @($dlls)) {
  if($dll.Hash -ine $expectedDllHash){throw "candidate DLL hash mismatch: $($dll.Path)"}
 }
 if(!$drivers -or @($drivers).Count -ne 1){throw 'expected one loaded WinFsp driver'}
 $driver=@($drivers)[0]
 if($driver.Hash -ine $expectedDriverHash){throw "candidate driver hash mismatch: $($driver.Path)"}
 if($driver.Thumbprint -ine $expectedThumbprint){throw "candidate driver signer mismatch: $($driver.Path)"}
}`

func candidateAttestation(manifest candidateManifest, nativePath, nativeSHA256 string) string {
	return candidateAttestationFunctions + `
$boot=& bcdedit /enum '{current}'
if($LASTEXITCODE -ne 0){throw 'cannot attest boot policy'}
$dlls=@(Get-Process weed -ErrorAction Stop | ForEach-Object { $_.Modules } | Where-Object {$_.ModuleName -ieq 'winfsp-x64.dll'} | ForEach-Object {[pscustomobject]@{Path=$_.FileName;Hash=(Get-FileHash -Algorithm SHA256 $_.FileName).Hash}})
$drivers=@(Get-CimInstance Win32_SystemDriver | Where-Object {$_.Name -like 'WinFsp*' -and $_.State -eq 'Running'} | ForEach-Object {
 $path=$_.PathName.Trim('"')
 if($path.StartsWith('\??\') -or $path.StartsWith('\\?\')){$path=$path.Substring(4)}
 $sig=Get-AuthenticodeSignature $path
 if($sig.Status -ne 'Valid'){throw "candidate driver signature $($sig.Status): $path"}
 [pscustomobject]@{Path=$path;Hash=(Get-FileHash -Algorithm SHA256 $path).Hash;Thumbprint=$sig.SignerCertificate.Thumbprint}
})
Assert-CandidateEvidence $boot $dlls $drivers ` + psLiteral(manifest.DLLSHA256) + ` ` + psLiteral(manifest.DriverSHA256) + ` ` + psLiteral(manifest.CertificateThumbprint) + `
$nativeHash=(Get-FileHash -Algorithm SHA256 ` + psLiteral(nativePath) + `).Hash
if($nativeHash -ine ` + psLiteral(nativeSHA256) + `){throw 'candidate native test executable hash mismatch'}
$dlls | Format-List
$drivers | Format-List
Write-Output "CANDIDATE_NATIVE_TEST_SHA256:$nativeHash"
Write-Output ` + psLiteral("CANDIDATE_WINFSP_ATTESTED:"+manifest.DriverSourceRevision+":"+manifest.DriverSourceArchiveSHA256) + `
`
}
