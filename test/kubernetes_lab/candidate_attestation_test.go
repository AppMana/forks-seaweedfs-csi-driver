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

func TestAttestationEvidenceRequiresExactLine(t *testing.T) {
	marker := "CANDIDATE_WINFSP_ATTESTED:revision:source"
	for _, tc := range []struct {
		output string
		want   bool
	}{
		{"prefix " + marker, false},
		{"Write-Output '" + marker + "'", false},
		{marker + "-suffix", false},
		{"evidence\r\n" + marker + "\r\n", true},
	} {
		if got := containsExactLine(tc.output, marker); got != tc.want {
			t.Fatalf("output %q: got %v want %v", tc.output, got, tc.want)
		}
	}
}

func TestQualificationAttestsBeforeAndAfterRecovery(t *testing.T) {
	b, err := os.ReadFile("csi_test.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	pre := strings.Index(text, `attest("pre-workload")`)
	recovery := strings.Index(text, "runCSIMixedRecovery(t,")
	post := strings.Index(text, `attest("post-recovery")`)
	complete := strings.Index(text, `fmt.Println("CSI_CANDIDATE_DRIVER_QUALIFICATION_COMPLETE")`)
	if pre < 0 || recovery <= pre || post <= recovery || complete <= post {
		t.Fatalf("attestation/recovery/completion order is not fail-closed: pre=%d recovery=%d post=%d complete=%d", pre, recovery, post, complete)
	}
}

func TestRetainedRecoveryAttestsBeforeAndAfterRecovery(t *testing.T) {
	b, err := os.ReadFile("retained_recovery_test.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	pre := strings.Index(text, `attest("pre-recovery")`)
	recovery := strings.Index(text, "runCSIMixedRecovery(t,")
	post := strings.Index(text, `attest("post-recovery")`)
	complete := strings.Index(text, `fmt.Println("CSI_RETAINED_MIXED_RECOVERY_COMPLETE")`)
	if pre < 0 || recovery <= pre || post <= recovery || complete <= post || strings.Contains(text, `strings.Contains(string(stock)`) {
		t.Fatalf("retained attestation/recovery/completion order is not fail-closed: pre=%d recovery=%d post=%d complete=%d", pre, recovery, post, complete)
	}
}

func TestCandidateNativeInputIsExplicitAndReadOnly(t *testing.T) {
	hash := strings.Repeat("a", 64)
	if got, err := candidateNativeInput(`C:\tools\winfsp-csi-candidate.test.exe`, hash); err != nil || got != `C:\LabInputs\winfsp-csi-candidate.test.exe` {
		t.Fatalf("valid input: %q %v", got, err)
	}
	for _, tc := range []struct{ path, hash string }{
		{`C:\tools\winfsp-csi-candidate.test.exe`, ""},
		{`C:\tools\subdir\test.exe`, hash},
		{`C:\LabInputs\test.exe`, hash},
		{`C:\tools\..\test.exe`, hash},
	} {
		if _, err := candidateNativeInput(tc.path, tc.hash); err == nil {
			t.Fatalf("accepted unsafe native input: %+v", tc)
		}
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
	oldExe, oldSHA := *nativeTestExecutable, *candidateNativeTestSHA256
	t.Cleanup(func() { *nativeTestExecutable, *candidateNativeTestSHA256 = oldExe, oldSHA })
	*nativeTestExecutable = `C:\tools\winfsp-csi-candidate.test.exe`
	*candidateNativeTestSHA256 = strings.Repeat("a", 64)
	candidate := nodePodForQualification("windows", true, true)
	stock := nodePodForQualification("windows", true, false)
	if len(candidate.Spec.InitContainers) != 3 || candidate.Spec.InitContainers[0].Name != "require-candidate" {
		t.Fatalf("candidate init is not fail-closed: %+v", candidate.Spec.InitContainers)
	}
	candidateScript := decodedPowerShell(t, candidate.Spec.InitContainers[0].Command)
	if strings.Contains(strings.ToLower(candidateScript), "winfsp.msi") || strings.Contains(candidateScript, "Start-Process") {
		t.Fatalf("candidate init can replace the prepared WinFsp install: %s", candidateScript)
	}
	if len(stock.Spec.InitContainers) != 3 || stock.Spec.InitContainers[0].Name != "install-stock" {
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
		if pod.Spec.InitContainers[1].Name != "test-inputs" || pod.Spec.InitContainers[2].Name != "native-test-inputs" {
			t.Fatal("test inputs must have explicit ordered initialization")
		}
		inputs := decodedPowerShell(t, pod.Spec.InitContainers[2].Command)
		if !strings.Contains(inputs, "winfsp-x64.dll") {
			t.Fatalf("%s native inputs missing: %s", name, inputs)
		}
		if name == "candidate" && (strings.Contains(inputs, "CONTAINER_SANDBOX_MOUNT_POINT\\winfsp-csi.test.exe") || !strings.Contains(inputs, "winfsp-csi-candidate.test.exe")) {
			t.Fatalf("candidate native input can be overwritten or is not explicit: %s", inputs)
		}
		if name == "stock" && !strings.Contains(inputs, "CONTAINER_SANDBOX_MOUNT_POINT\\winfsp-csi.test.exe") {
			t.Fatalf("stock native input changed unexpectedly: %s", inputs)
		}
	}
}
