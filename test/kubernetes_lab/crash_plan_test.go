package kubernetes_lab

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// Reuse all candidate/image/CNI pins, but select only the non-seeding oracle.
// The host fixture executes this same binary before and after its VM Crash.
func existingPersistencePlan(args []string, token, volume string) (string, error) {
	if _, _, err := existingDataset(token, volume); err != nil {
		return "", err
	}
	result := make([]string, 0, len(args)+2)
	runs := 0
	for _, arg := range args {
		if strings.HasPrefix(arg, "-csi-existing-") {
			return "", fmt.Errorf("existing scope must not be overridden")
		}
		if strings.HasPrefix(arg, "-test.run=") {
			switch arg {
			case "-test.run=^TestCSICandidateWinFsp$", "-test.run=^TestCSIStockWinFsp$", "-test.run=^TestCSIRetainedMixedRecovery$":
			default:
				return "", fmt.Errorf("unexpected initial consumer")
			}
			arg = "-test.run=^TestCSIExistingPersistence$"
			runs++
		}
		result = append(result, arg)
	}
	if runs != 1 {
		return "", fmt.Errorf("one exact initial consumer required")
	}
	result = append(result, "-csi-existing-token="+token, "-csi-existing-filer-root="+volume)
	body, err := json.Marshal(struct {
		Args    []string `json:"args"`
		Success string   `json:"success"`
	}{result, "CSI_EXISTING_PERSISTENCE_COMPLETE:" + token})
	return "KUBERNETES_CRASH_VERIFY=" + string(body), err
}

func TestExistingPersistencePlan(t *testing.T) {
	const token = "1790647831195823253"
	const volume = "/buckets/pvc-e19d01ca-e92c-460a-998d-a68c07a2b671"
	args := []string{"-test.v", "-test.run=^TestCSICandidateWinFsp$", "-csi-cni=calico-bgp", "-csi-candidate-manifest-sha256=exact-pin"}
	line, err := existingPersistencePlan(args, token, volume)
	if err != nil {
		t.Fatal(err)
	}
	var plan struct {
		Args    []string
		Success string
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "KUBERNETES_CRASH_VERIFY=")), &plan); err != nil {
		t.Fatal(err)
	}
	if len(plan.Args) != 6 || plan.Args[1] != "-test.run=^TestCSIExistingPersistence$" || plan.Args[2] != args[2] || plan.Args[3] != args[3] || plan.Success != "CSI_EXISTING_PERSISTENCE_COMPLETE:"+token {
		t.Fatalf("changed pins or oracle: %+v", plan)
	}
	if args[1] != "-test.run=^TestCSICandidateWinFsp$" {
		t.Fatal("mutated original arguments")
	}
	for _, bad := range [][]string{nil, {"-test.run=.*"}, {args[1], args[1]}, {args[1], "-csi-existing-token=other"}} {
		if _, err := existingPersistencePlan(bad, token, volume); err == nil {
			t.Fatalf("accepted %v", bad)
		}
	}
	if _, err := existingPersistencePlan(args, "../other", volume); err == nil {
		t.Fatal("accepted invalid dataset")
	}
}

func TestDllOnlyPersistencePlan(t *testing.T) {
	for _, entry := range []string{"TestCSIStockWinFsp", "TestCSIRetainedMixedRecovery"} {
		t.Run(entry, func(t *testing.T) {
			args := []string{"-test.run=^" + entry + "$", "-csi-winfsp-dll-sha256=" + strings.Repeat("a", 64)}
			line, err := existingPersistencePlan(args, "1790647831195823253", "/buckets/pvc-e19d01ca-e92c-460a-998d-a68c07a2b671")
			if err != nil {
				t.Fatal(err)
			}
			var plan struct{ Args []string }
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "KUBERNETES_CRASH_VERIFY=")), &plan); err != nil {
				t.Fatal(err)
			}
			if len(plan.Args) != 4 || plan.Args[0] != "-test.run=^TestCSIExistingPersistence$" || plan.Args[1] != args[1] {
				t.Fatalf("lost pinned DLL or read-only oracle: %v", plan.Args)
			}
		})
	}
}
