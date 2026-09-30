package kubernetes_lab

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func offlineImageRefs(image string) (source, canonical, digest string, err error) {
	source, digest, ok := strings.Cut(image, "@")
	if !ok || !strings.HasPrefix(digest, "sha256:") || len(digest) != 71 || strings.Trim(digest[7:], "0123456789abcdef") != "" || source == "" {
		return "", "", "", fmt.Errorf("invalid pinned image %q", image)
	}
	repository := source
	if colon := strings.LastIndex(repository, ":"); colon > strings.LastIndex(repository, "/") {
		repository = repository[:colon]
	}
	if repository == "" {
		return "", "", "", fmt.Errorf("missing repository")
	}
	return source, repository + "@" + digest, digest, nil
}

func listedImageDigest(data []byte, reference string) (string, error) {
	var digest string
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[0] != reference {
			continue
		}
		if digest != "" {
			return "", fmt.Errorf("ambiguous image reference %s", reference)
		}
		digest = fields[2]
	}
	return digest, nil
}

// k0s imports an OCI archive's tag, but CRI normalizes tag@digest to
// repository@digest. Register that alias only after verifying the existing
// descriptor. Never pull, retag different bytes, or force-replace an alias.
func prepareSplitOfflineImages(t *testing.T) {
	t.Helper()
	if err := validateSplitImages(); err != nil {
		t.Fatal(err)
	}
	for _, platform := range []string{"linux", "windows"} {
		call := func(args ...string) []byte {
			t.Helper()
			var out []byte
			var err error
			if platform == "linux" {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				out, err = exec.CommandContext(ctx, "k0s", args...).CombinedOutput()
			} else {
				script := "& 'C:\\LabQualification\\k0s.exe'"
				for _, arg := range args {
					script += " " + psLiteral(arg)
				}
				script += "; if($LASTEXITCODE -ne 0){exit $LASTEXITCODE}"
				argv := append([]string{"exec", "-n", "kube-system", "daemonset/calico-node-windows", "-c", "node", "--"}, ps(script)...)
				out, err = kubectlWithTimeout(45*time.Second, nil, argv...)
			}
			if err != nil {
				t.Fatalf("offline image metadata %s %v: %v %s", platform, args, err, out)
			}
			return out
		}
		for _, role := range []string{"driver", "mount"} {
			image := *splitImages[role+"-"+platform]
			if image == "" {
				continue
			}
			source, canonical, digest, err := offlineImageRefs(image)
			if err != nil {
				t.Fatal(err)
			}
			lookup := func(ref string) string {
				t.Helper()
				actual, err := listedImageDigest(call("ctr", "images", "ls", "name=="+ref), ref)
				if err != nil {
					t.Fatal(err)
				}
				return actual
			}
			if actual := lookup(canonical); actual != "" {
				if actual != digest {
					t.Fatalf("refusing conflicting offline alias %s: %s", canonical, actual)
				}
			} else {
				if actual := lookup(source); actual != digest {
					t.Fatalf("offline source %s: expected %s, found %s", source, digest, actual)
				}
				call("ctr", "images", "tag", source, canonical)
			}
			if actual := lookup(canonical); actual != digest {
				t.Fatalf("offline alias %s: expected %s, found %s", canonical, digest, actual)
			}
			t.Logf("OFFLINE_IMAGE_DIGEST_READY platform=%s image=%s", platform, canonical)
		}
	}
}

func TestOfflineImageReferences(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	for _, source := range []string{"registry.test:5000/org/image:tag", "registry.test:5000/org/image", "org/image:tag"} {
		_, canonical, actual, err := offlineImageRefs(source + "@" + digest)
		want := strings.TrimSuffix(source, ":tag") + "@" + digest
		if err != nil || canonical != want || actual != digest {
			t.Fatalf("%s: %s %s %v", source, canonical, actual, err)
		}
	}
	for _, bad := range []string{"image:latest", "@" + digest, "image@sha256:bad", "image@sha256:" + strings.Repeat("z", 64)} {
		if _, _, _, err := offlineImageRefs(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	data := []byte("REF TYPE DIGEST SIZE\nexample.test/image:tag application/vnd.oci.image.manifest.v1+json " + digest + " 20 MiB\n")
	if got, err := listedImageDigest(data, "example.test/image:tag"); err != nil || got != digest {
		t.Fatalf("digest: %q %v", got, err)
	}
	if got, err := listedImageDigest(data, "missing"); err != nil || got != "" {
		t.Fatalf("invented missing image: %q %v", got, err)
	}
	if _, err := listedImageDigest(append(data, data...), "example.test/image:tag"); err == nil {
		t.Fatal("duplicate reference accepted")
	}
}

func TestPrepareSplitOfflineImages(t *testing.T) {
	if !*live {
		t.Skip("requires disposable mixed-platform Labcontainers Kubernetes fixture")
	}
	assertFixture(t)
	prepareSplitOfflineImages(t)
}
