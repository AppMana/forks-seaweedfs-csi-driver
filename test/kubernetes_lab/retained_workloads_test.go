package kubernetes_lab

import (
	"encoding/json"
	"fmt"
	"path"
	"testing"
	"time"

	core "k8s.io/api/core/v1"
)

// Reuse the full qualification's unchanged workload functions without rebuilding
// a working cluster or replacing an EmptyDir backend. No recovery or seeding of
// an earlier failed dataset: each invocation creates only a new test directory.
func TestCSIRetainedWindowsWorkloads(t *testing.T) {
	if !*live {
		t.Skip("requires isolated retained CSI fixture")
	}
	assertFixture(t)
	if !existingVolumePattern.MatchString(*existingFilerRoot) || csiFilerRoot(t) != *existingFilerRoot {
		t.Fatal("original PVC handle required")
	}
	if err := validateSplitImages(); err != nil || *splitImages["mount-windows"] == "" {
		t.Fatalf("exact image pins required: %v", err)
	}
	script, marker, err := persistenceAttestation()
	if err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) []byte {
		t.Helper()
		out, err := kubectl(nil, args...)
		if err != nil {
			t.Fatalf("%v: %v %s", args, err, out)
		}
		return out
	}
	var pods core.PodList
	if err := json.Unmarshal(run("get", "pods", "-n", ns, "-o", "json"), &pods); err != nil {
		t.Fatal(err)
	}
	if err := verifySplitImageRuntime(pods); err != nil {
		t.Fatal(err)
	}
	attest := func() {
		out := run(append([]string{"exec", "-n", ns, "daemonset/mount-windows", "-c", "plugin", "--"}, ps(script)...)...)
		t.Logf("driver evidence:\n%s", out)
		if !containsExactLine(string(out), marker) {
			t.Fatal("driver attestation missing")
		}
	}
	attest()
	checkController := watchCSIController(t)
	token := fmt.Sprint(time.Now().UnixNano())
	dir := "qualification-" + token
	run("exec", "-n", ns, clientName("linux"), "--", "mkdir", "-p", "/data/"+dir+"/native")
	root := `C:\data\` + dir
	t.Run("GitLFS", func(t *testing.T) { runCSIGitLFS(t, root) })
	t.Run("Native", func(t *testing.T) { runCSINative(t, root+`\native`, path.Join(*existingFilerRoot, dir, "native"), "") })
	attest()
	checkController()
}
