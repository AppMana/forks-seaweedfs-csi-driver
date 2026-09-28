package kubernetes_lab

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"
)

func candidateManifestFixture() string {
	return `{"certificate_thumbprint":"581769D4AB466DF1B61FA2F7B4FD9B04C98D66E2","driver_source_revision":"a2e86d9aa1f48c3059ce45ff58fffbc511d82ecd","driver_source_archive_sha256":"860f35dc36d9a14f7b858bdcd3cd2896ace9917a705343e4d49069ba2c61a4ac","dll_sha256":"c5e54b08dc9227e613ef450d8169a1791b83b78fc0bdfbd0e0b1eec73dd40599","driver_sha256":"51eba9ef01bb12071a561c8b3cc28af38be48f3ea09584a96037c5c1ea59ab3d","lab_only":true}`
}

func TestCandidateManifestRequiresImmutableLabProvenance(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")
	body := candidateManifestFixture()
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(body))
	if _, err := loadCandidateManifest(path, hex.EncodeToString(sum[:])); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func() (string, string){
		"wrong digest": func() (string, string) { return body, strings.Repeat("0", 64) },
		"not lab only": func() (string, string) {
			changed := strings.Replace(body, `"lab_only":true`, `"lab_only":false`, 1)
			h := sha256.Sum256([]byte(changed))
			return changed, hex.EncodeToString(h[:])
		},
		"missing provenance": func() (string, string) {
			changed := strings.Replace(body, `"driver_source_revision":"a2e86d9aa1f48c3059ce45ff58fffbc511d82ecd"`, `"driver_source_revision":""`, 1)
			h := sha256.Sum256([]byte(changed))
			return changed, hex.EncodeToString(h[:])
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed, digest := mutate()
			if err := os.WriteFile(path, []byte(changed), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadCandidateManifest(path, digest); err == nil {
				t.Fatal("invalid candidate manifest accepted")
			}
		})
	}
}

func TestCandidateEvidenceRefusesMismatch(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("PowerShell required")
	}
	goodBoot := "@('testsigning Yes','nointegritychecks No')"
	goodDLL := "@([pscustomobject]@{Path='candidate.dll';Hash='DLL'})"
	goodDriver := "@([pscustomobject]@{Path='candidate.sys';Hash='SYS';Thumbprint='CERT'})"
	for _, tc := range []struct {
		name, boot, dll, driver string
		want                    bool
	}{
		{"matched", goodBoot, goodDLL, goodDriver, true},
		{"testsigning absent", "@('testsigning No')", goodDLL, goodDriver, false},
		{"integrity disabled", "@('testsigning Yes','nointegritychecks Yes')", goodDLL, goodDriver, false},
		{"DLL mismatch", goodBoot, "@([pscustomobject]@{Path='candidate.dll';Hash='OTHER'})", goodDriver, false},
		{"driver mismatch", goodBoot, goodDLL, "@([pscustomobject]@{Path='candidate.sys';Hash='OTHER';Thumbprint='CERT'})", false},
		{"signer mismatch", goodBoot, goodDLL, "@([pscustomobject]@{Path='candidate.sys';Hash='SYS';Thumbprint='OTHER'})", false},
		{"extra driver", goodBoot, goodDLL, goodDriver + "+" + goodDriver, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := "$ErrorActionPreference='Stop';" + candidateAttestationFunctions + ";Assert-CandidateEvidence " + tc.boot + " " + tc.dll + " " + tc.driver + " 'DLL' 'SYS' 'CERT'"
			out, err := exec.Command(pwsh, "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
			if (err == nil) != tc.want {
				t.Fatalf("accepted=%v error=%v output=%s", err == nil, err, out)
			}
		})
	}
}

func decodedPowerShell(t *testing.T, command []string) string {
	t.Helper()
	if len(command) != 5 || command[3] != "-EncodedCommand" {
		t.Fatalf("unexpected PowerShell transport: %q", command)
	}
	b, err := base64.StdEncoding.DecodeString(command[4])
	if err != nil || len(b)%2 != 0 {
		t.Fatalf("invalid encoded command: %v", err)
	}
	units := make([]uint16, len(b)/2)
	for i := range units {
		units[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
	}
	return string(utf16.Decode(units))
}

func TestCandidateInitializationCannotInstallStockWinFsp(t *testing.T) {
	candidate := nodePodForQualification("windows", true, true)
	stock := nodePodForQualification("windows", true, false)
	if len(candidate.Spec.InitContainers) != 2 || candidate.Spec.InitContainers[0].Name != "require-candidate" {
		t.Fatalf("candidate init is not fail-closed: %+v", candidate.Spec.InitContainers)
	}
	candidateScript := decodedPowerShell(t, candidate.Spec.InitContainers[0].Command)
	if strings.Contains(strings.ToLower(candidateScript), "winfsp.msi") || strings.Contains(candidateScript, "Start-Process") {
		t.Fatalf("candidate init can replace the prepared WinFsp install: %s", candidateScript)
	}
	if len(stock.Spec.InitContainers) != 2 || stock.Spec.InitContainers[0].Name != "install-stock" {
		t.Fatalf("stock init changed unexpectedly: %+v", stock.Spec.InitContainers)
	}
	stockScript := decodedPowerShell(t, stock.Spec.InitContainers[0].Command)
	if !strings.Contains(strings.ToLower(stockScript), "winfsp.msi") || !strings.Contains(stockScript, "Start-Process") {
		t.Fatalf("stock install contract changed unexpectedly: %s", stockScript)
	}
	// Both lanes still stage the native executable and the DLL from the active
	// registered installation for the canonical mounted-suite hash check.
	for i, name := range []string{"candidate", "stock"} {
		pod := candidate
		if i == 1 {
			pod = stock
		}
		inputs := decodedPowerShell(t, pod.Spec.InitContainers[1].Command)
		if !strings.Contains(inputs, "winfsp-csi.test.exe") || !strings.Contains(inputs, "winfsp-x64.dll") {
			t.Fatalf("%s native inputs missing: %s", name, inputs)
		}
	}
}
