package kubernetes_lab

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"testing"
	"time"
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
	checkController := watchCSIController(t)
	stock := run(append([]string{"exec", "-n", ns, "daemonset/mount-windows", "-c", "plugin", "--"}, ps(stockAttestation)...)...)
	if !strings.Contains(string(stock), "STOCK_WINFSP_ATTESTED") {
		t.Fatal("missing stock-driver evidence")
	}
	token := fmt.Sprint(time.Now().UnixNano())
	linuxPath, windowsPath := "/data/qualification-"+token, `C:\data\qualification-`+token
	nativePath := windowsPath + `\native`
	filerPath := path.Join(csiFilerRoot(t), "qualification-"+token, "native")
	// The full native suite normally creates this test subdirectory. A retained
	// persistence-only run must supply it explicitly before the attributes test,
	// which intentionally uses Mkdir rather than repairing missing parents.
	run("exec", "-n", ns, clientName("linux"), "--", "mkdir", "-p", linuxPath+"/mixed/.sync", linuxPath+"/native")
	runCSINative(t, nativePath, filerPath, "write")
	runCSIMixedRecovery(t, linuxPath, windowsPath, nativePath, filerPath, token, run, applyClient)
	checkController()
	fmt.Println("CSI_RETAINED_MIXED_RECOVERY_COMPLETE")
}
