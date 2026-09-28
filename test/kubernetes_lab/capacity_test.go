package kubernetes_lab

import (
	core "k8s.io/api/core/v1"
	"strconv"
	"strings"
	"testing"
)

// The default and per-PVC collections cannot share writable volume slots.
// With the tested master defaults, each requests a seven-volume growth batch.
// This guards the exact lab error where the default collection used all five
// slots even though the filesystem still had gigabytes of free space.
func TestCSICollectionSlotCapacity(t *testing.T) {
	slots := 0
	for _, o := range objects() {
		pod, ok := o.(core.Pod)
		if !ok {
			continue
		}
		for _, c := range pod.Spec.Containers {
			for _, arg := range c.Args {
				for _, prefix := range []string{"-volume.max=", "-max="} {
					if strings.HasPrefix(arg, prefix) {
						count, err := strconv.Atoi(strings.TrimPrefix(arg, prefix))
						if err != nil || count <= 0 {
							t.Fatalf("explicit positive lab capacity required: %s", arg)
						}
						slots += count
					}
				}
			}
		}
	}
	const collections = 2
	const defaultGrowthBatch = 7
	if slots < collections*defaultGrowthBatch {
		t.Fatalf("%d volume slots cannot cover default and PVC collection growth (%d required)", slots, collections*defaultGrowthBatch)
	}
}
