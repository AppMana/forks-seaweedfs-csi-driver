package kubernetes_lab

import (
	"flag"
	"fmt"
	"path"
	"regexp"
	"strings"
	"testing"
)

var existingToken = flag.String("csi-existing-token", "", "exact completed qualification dataset token; never seed or repair it")
var existingFilerRoot = flag.String("csi-existing-filer-root", "", "exact existing CSI volume handle, required to reject a replacement PVC")

var existingVolumePattern = regexp.MustCompile(`^/buckets/pvc-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func existingDataset(token, volume string) (string, string, error) {
	if len(token) != 19 || strings.Trim(token, "0123456789") != "" || token[0] == '0' {
		return "", "", fmt.Errorf("require exact 19-digit qualification token")
	}
	if !existingVolumePattern.MatchString(volume) {
		return "", "", fmt.Errorf("require exact CSI PVC volume handle")
	}
	return "/data/qualification-" + token, `C:\data\qualification-` + token, nil
}

func TestExistingDatasetScope(t *testing.T) {
	const token = "1790647831195823253"
	const volume = "/buckets/pvc-e19d01ca-e92c-460a-998d-a68c07a2b671"
	l, w, err := existingDataset(token, volume)
	if err != nil || l != "/data/qualification-"+token || w != `C:\data\qualification-`+token {
		t.Fatalf("invalid valid scope: %q %q %v", l, w, err)
	}
	for _, bad := range []string{"", "../" + token, "0" + token[1:], token + "/", "179064783119582325x"} {
		if _, _, err := existingDataset(bad, volume); err == nil {
			t.Fatalf("accepted token %q", bad)
		}
	}
	for _, bad := range []string{"", "/", "/buckets/other", "/buckets/pvc-", volume + "/../other", volume + `\child`} {
		if _, _, err := existingDataset(token, bad); err == nil {
			t.Fatalf("accepted volume %q", bad)
		}
	}
}

// This verifier never applies Kubernetes objects or seeds payloads. The mixed
// executable updates only its existing .sync rendezvous markers; all payload,
// rename, deletion and inventory checks reuse the original strict oracle.
// Run the identical token and volume before and after a host-driven VM crash.
func TestCSIExistingPersistence(t *testing.T) {
	if !*live {
		t.Skip("requires disposable mixed-platform Labcontainers Kubernetes fixture")
	}
	l, w, err := existingDataset(*existingToken, *existingFilerRoot)
	if err != nil {
		t.Fatal(err)
	}
	attestation, marker, err := persistenceAttestation()
	if err != nil {
		t.Fatal(err)
	}
	assertFixture(t)
	if actual := csiFilerRoot(t); actual != *existingFilerRoot {
		t.Fatalf("CSI volume replaced: got %q want %q", actual, *existingFilerRoot)
	}
	run := func(args ...string) []byte {
		t.Helper()
		out, err := kubectl(nil, args...)
		t.Logf("kubectl %v\n%s", args, out)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return out
	}
	run("wait", "-n", ns, "pod/"+clientName("linux"), "pod/"+clientName("windows"), "--for=condition=Ready", "--timeout=5m")
	captureWindowsNetwork(t)
	checkController := watchCSIController(t)
	out := run(append([]string{"exec", "-n", ns, "daemonset/mount-windows", "-c", "plugin", "--"}, ps(attestation)...)...)
	if !containsExactLine(string(out), marker) {
		t.Fatal("persistence driver attestation missing")
	}
	for _, platform := range []string{"linux", "windows"} {
		binary, root := "/usr/local/bin/mixed-linux", l+"/mixed"
		if platform == "windows" {
			binary, root = `C:\tools\mixed-windows.exe`, w+`\mixed`
		}
		out := run("exec", "-n", ns, clientName(platform), "--", binary, root, platform, "verify-remount", *existingToken)
		if !containsExactLine(string(out), "MIXED_COMPLETE:"+*existingToken+":"+platform+":verify-remount") {
			t.Fatal("mixed persistence completion missing")
		}
	}
	runCSINative(t, w+`\native`, path.Join(*existingFilerRoot, "qualification-"+*existingToken, "native"), "verify")
	checkController()
	fmt.Println("CSI_EXISTING_PERSISTENCE_COMPLETE:" + *existingToken)
}
