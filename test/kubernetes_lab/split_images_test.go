package kubernetes_lab

import (
	"flag"
	"fmt"
	"strings"
	"testing"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func verifySplitImageRuntime(pods core.PodList) error {
	expected := map[string]string{
		"mount-linux": *splitImages["mount-linux"], "node-linux": *splitImages["driver-linux"],
		"mount-windows": *splitImages["mount-windows"], "node-windows": *splitImages["driver-windows"],
		"controller": *splitImages["driver-linux"],
		"backend":    *splitImages["mount-linux"], "capacity": *splitImages["mount-linux"],
	}
	seen := map[string]bool{}
	for _, pod := range pods.Items {
		role := pod.Labels["app"]
		image, ok := expected[role]
		if !ok {
			continue
		}
		name := "plugin"
		if role == "controller" {
			name = "driver"
		}
		if role == "backend" {
			name = "weed"
		}
		if role == "capacity" {
			name = "volume"
		}
		for _, status := range pod.Status.ContainerStatuses {
			if status.Name != name {
				continue
			}
			parts := strings.Split(image, "@")
			if len(parts) != 2 || !status.Ready || status.State.Running == nil || !strings.HasSuffix(status.ImageID, parts[1]) {
				return fmt.Errorf("%s runtime image not verified: expected %s, observed %+v", role, image, status)
			}
			seen[role] = true
		}
	}
	if len(seen) != len(expected) {
		return fmt.Errorf("only %d of %d split runtime roles attested", len(seen), len(expected))
	}
	return nil
}

func TestSplitImagesKeepToolsOutOfRuntimeImages(t *testing.T) {
	for _, platform := range []string{"linux", "windows"} {
		for _, role := range []string{"driver", "mount"} {
			name := "csi-" + role + "-" + platform + "-image"
			f := flag.Lookup(name)
			if f == nil {
				t.Fatalf("missing final image selector %s", name)
			}
			old := f.Value.String()
			t.Cleanup(func() { _ = f.Value.Set(old) })
			if err := f.Value.Set("example.test/" + role + ":" + platform + "@sha256:" + strings.Repeat("a", 64)); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, platform := range []string{"linux", "windows"} {
		for _, mount := range []bool{false, true} {
			p := nodePod(platform, mount)
			role := "driver"
			if mount {
				role = "mount"
			}
			c := p.Spec.Containers[0]
			if c.Image != flag.Lookup("csi-"+role+"-"+platform+"-image").Value.String() || c.ImagePullPolicy != core.PullNever {
				t.Fatalf("wrong runtime image: %+v", c)
			}
			if platform == "linux" && c.Command[0] != "/seaweedfs-"+map[string]string{"driver": "csi-driver", "mount": "mount"}[role] {
				t.Fatalf("wrong final image command: %v", c.Command)
			}
			for _, init := range p.Spec.InitContainers {
				if (init.Name == "test-inputs" || init.Name == "native-test-inputs") && init.Image != windowsImage {
					t.Fatalf("test inputs must use tool image: %+v", init)
				}
			}
			if platform == "windows" && mount {
				found := false
				for _, init := range p.Spec.InitContainers {
					if init.Name == "test-inputs" {
						found = true
					}
				}
				if !found {
					t.Fatal("test tools still coupled to runtime image")
				}
			}
		}
	}
	for _, object := range objects() {
		if p, ok := object.(core.Pod); ok && p.Name == "controller" {
			if p.Spec.Containers[0].Image != flag.Lookup("csi-driver-linux-image").Value.String() || p.Spec.Containers[0].Command[0] != "/seaweedfs-csi-driver" {
				t.Fatal("controller bypasses final image")
			}
		}
	}
}

func TestSplitImagesRejectPartialOrMutableSelection(t *testing.T) {
	for _, value := range splitImages {
		old := *value
		t.Cleanup(func() { *value = old })
		*value = ""
	}
	if err := validateSplitImages(); err != nil {
		t.Fatal(err)
	}
	*splitImages["driver-linux"] = "example.test/driver@sha256:" + strings.Repeat("a", 64)
	if validateSplitImages() == nil {
		t.Fatal("partial selection accepted")
	}
	for _, value := range splitImages {
		*value = "example.test/image@sha256:" + strings.Repeat("a", 64)
	}
	if err := validateSplitImages(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"image:latest", "image@sha256:abc", "image@sha256:" + strings.Repeat("z", 64), "@sha256:" + strings.Repeat("a", 64)} {
		*splitImages["mount-windows"] = bad
		if validateSplitImages() == nil {
			t.Fatalf("mutable or malformed image accepted: %s", bad)
		}
	}
}

func TestSplitRuntimeRequiresEveryRunningDigest(t *testing.T) {
	const digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, value := range splitImages {
		old := *value
		t.Cleanup(func() { *value = old })
		*value = "example.test/image@" + digest
	}
	var pods core.PodList
	for _, role := range []string{"controller", "mount-linux", "node-linux", "mount-windows", "node-windows", "backend", "capacity"} {
		name := "plugin"
		if role == "controller" {
			name = "driver"
		}
		if role == "backend" {
			name = "weed"
		}
		if role == "capacity" {
			name = "volume"
		}
		pods.Items = append(pods.Items, core.Pod{ObjectMeta: meta.ObjectMeta{Labels: map[string]string{"app": role}}, Status: core.PodStatus{ContainerStatuses: []core.ContainerStatus{{Name: name, Ready: true, ImageID: "example.test/image@" + digest, State: core.ContainerState{Running: &core.ContainerStateRunning{}}}}}})
	}
	if err := verifySplitImageRuntime(pods); err != nil {
		t.Fatal(err)
	}
	for _, corrupt := range []func(*core.PodList){
		func(p *core.PodList) { p.Items = p.Items[:5] },
		func(p *core.PodList) { p.Items = p.Items[:4] },
		func(p *core.PodList) {
			p.Items[0].Status.ContainerStatuses[0].ImageID = "example.test/image@sha256:" + strings.Repeat("b", 64)
		},
		func(p *core.PodList) { p.Items[0].Status.ContainerStatuses[0].Ready = false },
		func(p *core.PodList) { p.Items[0].Status.ContainerStatuses[0].State.Running = nil },
		func(p *core.PodList) { p.Items[0].Status.ContainerStatuses = nil },
	} {
		bad := pods.DeepCopy()
		corrupt(bad)
		if verifySplitImageRuntime(*bad) == nil {
			t.Fatal("incomplete runtime image evidence accepted")
		}
	}
}
