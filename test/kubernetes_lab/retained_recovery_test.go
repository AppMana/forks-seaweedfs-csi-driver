package kubernetes_lab

import (
	"encoding/json"
	"fmt"
	"path"
	"testing"
	"time"

	core "k8s.io/api/core/v1"
)

// Diagnose recovery independently of native conformance failures. Never apply
// backend/driver objects here: retained EmptyDir data must not be recreated to
// satisfy a newer fixture manifest. This entry point still remounts workload
// pods and reboots the disposable Windows node, using the full gate's oracles.
func TestCSIRetainedMixedRecovery(t *testing.T) {
	if !*live {
		t.Skip("requires disposable mixed-platform Labcontainers Kubernetes fixture")
	}
	assertFixture(t)
	run := func(args ...string) []byte {
		t.Helper()
		out, err := kubectl(nil, args...)
		t.Logf("kubectl %v\n%s", args, out)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return out
	}
	applyClient := func(object any) {
		t.Helper()
		data, err := json.Marshal(object)
		if err != nil {
			t.Fatal(err)
		}
		out, err := kubectl(data, "apply", "-f", "-")
		t.Logf("apply workload: %s", out)
		if err != nil {
			t.Fatal(err)
		}
	}
	run("wait", "-n", ns, "pod/"+clientName("linux"), "pod/"+clientName("windows"), "--for=condition=Ready", "--timeout=45s")
	if err := validateSplitImages(); err != nil {
		t.Fatal(err)
	}
	if *splitImages["driver-linux"] != "" {
		var pods core.PodList
		if err := json.Unmarshal(run("get", "pods", "-n", ns, "-o", "json"), &pods); err != nil {
			t.Fatal(err)
		}
		if err := verifySplitImageRuntime(pods); err != nil {
			t.Fatal(err)
		}
	}
	checkController := watchCSIController(t)
	script, marker := stockAttestation, "STOCK_WINFSP_ATTESTED"
	if *candidateManifestPath != "" || *candidateManifestSHA256 != "" {
		manifest, err := loadCandidateManifest(*candidateManifestPath, *candidateManifestSHA256)
		if err != nil {
			t.Fatal(err)
		}
		native, err := candidateNativeInput(*nativeTestExecutable, *candidateNativeTestSHA256)
		if err != nil {
			t.Fatal(err)
		}
		script = candidateAttestation(manifest, native, *candidateNativeTestSHA256)
		marker = "CANDIDATE_WINFSP_ATTESTED:" + manifest.DriverSourceRevision + ":" + manifest.DriverSourceArchiveSHA256
	}
	attest := func(stage string) {
		t.Helper()
		evidence := run(append([]string{"exec", "-n", ns, "daemonset/mount-windows", "-c", "plugin", "--"}, ps(script)...)...)
		if !containsExactLine(string(evidence), marker) {
			t.Fatalf("missing exact driver evidence at %s", stage)
		}
	}
	attest("pre-recovery")
	token := fmt.Sprint(time.Now().UnixNano())
	linuxPath, windowsPath := "/data/qualification-"+token, `C:\data\qualification-`+token
	phase := "write"
	if *existingToken != "" || *existingFilerRoot != "" {
		var err error
		linuxPath, windowsPath, err = existingDataset(*existingToken, *existingFilerRoot)
		if err != nil {
			t.Fatal(err)
		}
		if actual := csiFilerRoot(t); actual != *existingFilerRoot {
			t.Fatalf("CSI volume replaced: got %q want %q", actual, *existingFilerRoot)
		}
		token, phase = *existingToken, "verify"
	}
	nativePath := windowsPath + `\native`
	filerPath := path.Join(csiFilerRoot(t), "qualification-"+token, "native")
	// The full native suite normally creates this test subdirectory. A retained
	// persistence-only run must supply it explicitly before the attributes test,
	// which intentionally uses Mkdir rather than repairing missing parents.
	if phase == "write" {
		run("exec", "-n", ns, clientName("linux"), "--", "mkdir", "-p", linuxPath+"/mixed/.sync", linuxPath+"/native")
	}
	// With an explicit existing token, verify the completed write phase; never
	// rewrite native fixtures to turn a failed persistence check into a pass.
	runCSINative(t, nativePath, filerPath, phase)
	runCSIMixedRecovery(t, linuxPath, windowsPath, nativePath, filerPath, token, run, applyClient)
	attest("post-recovery")
	checkController()
	fmt.Println("CSI_RETAINED_MIXED_RECOVERY_COMPLETE")
}
