package kubernetes_lab

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	rbac "k8s.io/api/rbac/v1"
	storage "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var live = flag.Bool("csi-live", false, "run only inside the disposable Labcontainers k0s controller")
var smokeSource = flag.String("mount-smoke-source", "/mnt/qualification/mount-smoke.ps1", "existing SeaweedFS Git LFS regression source on hash-pinned offline media")
var clientRun = flag.String("csi-client-run", "", "explicit client name suffix for independent A/B runs; never repairs or replaces a failed run")
var nativeTestExecutable = flag.String("csi-native-test-executable", `C:\tools\winfsp-csi.test.exe`, "explicit native test input in the CSI client's read-only tools share")
var candidateManifestPath = flag.String("csi-candidate-manifest", "", "explicit lab-only WinFsp candidate build manifest")
var candidateManifestSHA256 = flag.String("csi-candidate-manifest-sha256", "", "immutable SHA-256 of the candidate build manifest")
var candidateNativeTestSHA256 = flag.String("csi-candidate-native-test-sha256", "", "immutable SHA-256 of the separately staged candidate native test executable")
var emitCrashPlan = flag.Bool("csi-emit-crash-plan", false, "publish original-dataset readback continuation for the host fixture's opt-in crash gate")
var splitImages = map[string]*string{
	"driver-linux":   flag.String("csi-driver-linux-image", "", "digest-pinned final Linux driver image; provide all four split images"),
	"mount-linux":    flag.String("csi-mount-linux-image", "", "digest-pinned final Linux mount image"),
	"driver-windows": flag.String("csi-driver-windows-image", "", "digest-pinned final Windows driver image"),
	"mount-windows":  flag.String("csi-mount-windows-image", "", "digest-pinned final Windows mount image"),
}

func runtimeImage(platform string, mount bool) string {
	role := "driver"
	if mount {
		role = "mount"
	}
	if image := *splitImages[role+"-"+platform]; image != "" {
		return image
	}
	if platform == "windows" {
		return windowsImage
	}
	return linuxImage
}

func validateSplitImages() error {
	count := 0
	for role, value := range splitImages {
		if *value == "" {
			continue
		}
		count++
		parts := strings.Split(*value, "@sha256:")
		if len(parts) != 2 || parts[0] == "" || len(parts[1]) != 64 || strings.Trim(parts[1], "0123456789abcdef") != "" {
			return fmt.Errorf("%s must be an immutable sha256 image reference", role)
		}
	}
	if count != 0 && count != 4 {
		return fmt.Errorf("provide all four split runtime images, got %d", count)
	}
	return nil
}

const ns = "seaweedfs-csi-qualification"
const driver = "seaweedfs-csi-driver"
const csiLegacyPermissionUID uint32 = 0
const csiLegacyPermissionGID uint32 = 0
const csiLegacyPermissionMode uint32 = 0770
const linuxImage = "docker.io/appmana/seaweedfs-csi-lab:linux-8ecd3e03f-b6bb02a"
const windowsImage = "docker.io/appmana/seaweedfs-csi-lab:windows-8ecd3e03f-b6bb02a"

// The multi-platform tag also contains Server 2019. Offline media must select
// the Server 2022 manifest explicitly, not the first windows/amd64 descriptor.
const windowsRegistrar = "registry.k8s.io/sig-storage/csi-node-driver-registrar:v2.16.0@sha256:22a61d2c07212b93abad5600c6deaaa1b6c4264920ee14a4a451630979137fbb"
const servercore = "mcr.microsoft.com/windows/servercore@sha256:e10503b9a4f7faafa30aa0f5d0e8e7f7ca30a4496b3b87d61178b4d7c6815fb5"
const linuxRoot = "/var/lib/k0s/kubelet"
const windowsRoot = `C:\var\lib\k0s\kubelet`

func ptr[T any](v T) *T { return &v }
func metadata(name string) meta.ObjectMeta {
	return meta.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{"app": name}}
}
func container(name, image string, command []string, args ...string) core.Container {
	return core.Container{Name: name, Image: image, ImagePullPolicy: core.PullNever, Command: command, Args: args}
}
func ps(script string) []string {
	units := utf16.Encode([]rune("$ErrorActionPreference='Stop'; " + script))
	encoded := make([]byte, 2*len(units))
	for i, u := range units {
		encoded[2*i] = byte(u)
		encoded[2*i+1] = byte(u >> 8)
	}
	return []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-EncodedCommand", base64.StdEncoding.EncodeToString(encoded)}
}
func basePod(name, platform string) core.Pod {
	return core.Pod{TypeMeta: meta.TypeMeta{APIVersion: "v1", Kind: "Pod"}, ObjectMeta: metadata(name), Spec: core.PodSpec{NodeSelector: map[string]string{"kubernetes.io/os": platform}, Tolerations: []core.Toleration{{Operator: core.TolerationOpExists}}}}
}
func hostPath(p *core.Pod, name, path, target string, propagation bool) {
	p.Spec.Volumes = append(p.Spec.Volumes, core.Volume{Name: name, VolumeSource: core.VolumeSource{HostPath: &core.HostPathVolumeSource{Path: path, Type: ptr(core.HostPathDirectoryOrCreate)}}})
	m := core.VolumeMount{Name: name, MountPath: target}
	if propagation {
		m.MountPropagation = ptr(core.MountPropagationBidirectional)
	}
	p.Spec.Containers[0].VolumeMounts = append(p.Spec.Containers[0].VolumeMounts, m)
}
func daemon(p core.Pod) apps.DaemonSet {
	return apps.DaemonSet{TypeMeta: meta.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSet"}, ObjectMeta: p.ObjectMeta, Spec: apps.DaemonSetSpec{Selector: &meta.LabelSelector{MatchLabels: p.Labels}, Template: core.PodTemplateSpec{ObjectMeta: meta.ObjectMeta{Labels: p.Labels}, Spec: p.Spec}}}
}
func nodePod(platform string, mount bool) core.Pod {
	return nodePodForQualification(platform, mount, false)
}
func nodePodForQualification(platform string, mount, candidate bool) core.Pod {
	name := "node-" + platform
	if mount {
		name = "mount-" + platform
	}
	p := basePod(name, platform)
	p.Spec.ServiceAccountName = "csi"
	p.Spec.HostNetwork = true
	p.Spec.DNSPolicy = core.DNSClusterFirstWithHostNet
	root, image, endpoint, mountEndpoint := linuxRoot, linuxImage, "unix:///csi/csi.sock", "unix:///var/lib/seaweedfs-mount/seaweedfs-mount.sock"
	command := []string{"/usr/local/bin/seaweedfs-csi-driver"}
	cache := "/var/cache/seaweedfs"
	if platform == "windows" {
		root, image, cache = windowsRoot, windowsImage, `C:\var\cache\seaweedfs`
		endpoint = `unix://` + root + `\plugins\` + driver + `\csi.sock`
		mountEndpoint = `unix://C:\var\lib\seaweedfs-mount\seaweedfs-mount.sock`
		command = []string{"$env:CONTAINER_SANDBOX_MOUNT_POINT/seaweedfs-csi-driver.exe"}
		p.Spec.SecurityContext = &core.PodSecurityContext{WindowsOptions: &core.WindowsSecurityContextOptions{HostProcess: ptr(true), RunAsUserName: ptr(`NT AUTHORITY\SYSTEM`)}}
	}
	args := []string{"--endpoint=" + endpoint, "--filer=192.0.2.10:8888", "--driverName=" + driver, "--components=node", "--nodeid=$(NODE_ID)", "--mountEndpoint=" + mountEndpoint, "--cacheDir=" + cache, "--cacheCapacityMB=1024", "--stagingScanDir=" + root + "/plugins/kubernetes.io/csi"}
	if mount {
		args = []string{"--endpoint=" + mountEndpoint}
		command = []string{"/usr/local/bin/seaweedfs-mount"}
		if platform == "windows" {
			command = []string{"$env:CONTAINER_SANDBOX_MOUNT_POINT/seaweedfs-mount.exe"}
		}
	}
	image = runtimeImage(platform, mount)
	if platform == "linux" && image != linuxImage {
		command[0] = strings.Replace(command[0], "/usr/local/bin/", "/", 1)
	}
	c := container("plugin", image, command, args...)
	c.Env = []core.EnvVar{{Name: "NODE_ID", ValueFrom: &core.EnvVarSource{FieldRef: &core.ObjectFieldSelector{FieldPath: "spec.nodeName"}}}}
	p.Spec.Containers = []core.Container{c}
	if platform == "linux" {
		p.Spec.HostPID = !mount
		p.Spec.Containers[0].SecurityContext = &core.SecurityContext{Privileged: ptr(true)}
		hostPath(&p, "plugins", root+"/plugins", root+"/plugins", true)
		hostPath(&p, "pods", root+"/pods", root+"/pods", true)
		hostPath(&p, "dev", "/dev", "/dev", false)
		hostPath(&p, "cache", cache, cache, false)
		hostPath(&p, "supervisor", "/var/lib/seaweedfs-mount", "/var/lib/seaweedfs-mount", false)
		if !mount {
			hostPath(&p, "socket", root+"/plugins/"+driver, "/csi", false)
			hostPath(&p, "registration", root+"/plugins_registry", "/registration", false)
			r := container("registrar", "registry.k8s.io/sig-storage/csi-node-driver-registrar:v2.8.0", nil, "--csi-address=/csi/csi.sock", "--kubelet-registration-path="+root+"/plugins/"+driver+"/csi.sock")
			r.VolumeMounts = []core.VolumeMount{{Name: "socket", MountPath: "/csi"}, {Name: "registration", MountPath: "/registration"}}
			p.Spec.Containers = append(p.Spec.Containers, r)
		}
	} else {
		if mount {
			p.Spec.Containers[0].Env = append(p.Spec.Containers[0].Env, core.EnvVar{Name: "WEED_WINFSP_VOLUME_PREFIX", Value: `\seaweedfs`})
			prepare := `if (!(Test-Path 'HKLM:\SOFTWARE\WOW6432Node\WinFsp')) { Copy-Item "$env:CONTAINER_SANDBOX_MOUNT_POINT\winfsp.msi" C:\Windows\Temp\winfsp.msi; $p=Start-Process msiexec -Wait -PassThru -ArgumentList '/i','C:\Windows\Temp\winfsp.msi','/qn','INSTALLLEVEL=1000'; if($p.ExitCode -ne 0){throw "MSI exit $($p.ExitCode)"} };`
			name := "install-stock"
			if candidate {
				// Candidate installation and reboot are an explicit lab prerequisite.
				// Never let image initialization replace it with the stock MSI.
				prepare = `if (!(Test-Path 'HKLM:\SOFTWARE\WOW6432Node\WinFsp')) { throw 'prepared candidate WinFsp installation missing' };`
				name = "require-candidate"
			}
			prepare += ` New-Item -ItemType Directory -Force C:\LabInputs,C:\var\lib\seaweedfs-mount,C:\var\cache\seaweedfs | Out-Null;`
			p.Spec.InitContainers = []core.Container{container(name, image, ps(prepare))}
			p.Spec.InitContainers = append(p.Spec.InitContainers, container("test-inputs", windowsImage, ps(`Copy-Item "$env:CONTAINER_SANDBOX_MOUNT_POINT\mixed-windows.exe" C:\LabInputs; Copy-Item "$env:CONTAINER_SANDBOX_MOUNT_POINT\Git-2.51.0-64-bit.exe" C:\LabInputs`)))
			nativeInputs := `Copy-Item "$env:CONTAINER_SANDBOX_MOUNT_POINT\winfsp-csi.test.exe" C:\LabInputs;`
			if candidate || *dllOnlySHA256 != "" {
				nativePath, err := candidateNativeInput(*nativeTestExecutable, *candidateNativeTestSHA256)
				if err != nil {
					nativeInputs = `throw ` + psLiteral(err.Error()) + `;`
				} else {
					nativeInputs = `$native=` + psLiteral(nativePath) + `; if(!(Test-Path -LiteralPath $native)){throw 'prepared candidate native test executable missing'}; if((Get-FileHash -Algorithm SHA256 -LiteralPath $native).Hash -ine ` + psLiteral(*candidateNativeTestSHA256) + `){throw 'prepared candidate native test executable hash mismatch'};`
					if *dllOnlySHA256 != "" {
						// Fresh DLL-only fixtures do not run the kernel-candidate MSI
						// bootstrap. Stage their separately pinned native oracle from
						// the read-only ISO; never overwrite a retained executable.
						nativeInputs = `$native=` + psLiteral(nativePath) + `; if(!(Test-Path -LiteralPath $native)){ $v=@(Get-Volume -FileSystemLabel LCQUAL); if($v.Count -ne 1){throw 'expected one qualification ISO'}; $src=Join-Path ($v[0].DriveLetter+':\') (Split-Path -Leaf $native); if((Get-FileHash -Algorithm SHA256 -LiteralPath $src).Hash -ine ` + psLiteral(*candidateNativeTestSHA256) + `){throw 'offline native test executable hash mismatch'}; Copy-Item -LiteralPath $src -Destination $native }; ` + nativeInputs
					}
				}
			}
			nativeImage := windowsImage
			if *dllOnlySHA256 != "" {
				// Test executable is separately hash-attested; load the same
				// app-local DLL as the mount, not the host installation's DLL.
				nativeImage = image
				nativeInputs += `; $dll=Join-Path $env:CONTAINER_SANDBOX_MOUNT_POINT 'winfsp-x64.dll'; if((Get-FileHash $dll -Algorithm SHA256).Hash -ine ` + psLiteral(*dllOnlySHA256) + `){throw 'app-local DLL hash mismatch'}; Copy-Item $dll C:\LabInputs\winfsp-x64.dll`
			} else {
				nativeInputs += ` $root=(Get-ItemProperty 'HKLM:\SOFTWARE\WOW6432Node\WinFsp').InstallDir; if(!$root){throw 'missing WinFsp installation'}; Copy-Item (Join-Path $root 'bin\winfsp-x64.dll') C:\LabInputs`
			}
			p.Spec.InitContainers = append(p.Spec.InitContainers, container("native-test-inputs", nativeImage, ps(nativeInputs)))
		} else {
			p.Spec.InitContainers = []core.Container{container("directories", image, ps(`New-Item -ItemType Directory -Force '`+root+`\plugins\`+driver+`' | Out-Null`))}
			p.Spec.Containers = append(p.Spec.Containers, container("registrar", windowsRegistrar, []string{"csi-node-driver-registrar.exe"}, "--csi-address="+endpoint, "--kubelet-registration-path="+strings.TrimPrefix(endpoint, "unix://"), "--plugin-registration-path="+root+`\plugins_registry\`, "--v=2"))
		}
	}
	return p
}
func clientName(platform string) string {
	name := "client-" + platform
	if *clientRun != "" {
		name += "-" + *clientRun
	}
	return name
}
func clientPod(platform string) core.Pod {
	p := basePod(clientName(platform), platform)
	image, path, cmd := linuxImage, "/data", []string{"sh", "-c", "sleep 86400"}
	if platform == "windows" {
		image, path, cmd = servercore, `C:\data`, ps("Start-Sleep -Seconds 86400")
	}
	c := container("workload", image, cmd)
	c.VolumeMounts = []core.VolumeMount{{Name: "data", MountPath: path}}
	p.Spec.Containers = []core.Container{c}
	p.Spec.Volumes = []core.Volume{{Name: "data", VolumeSource: core.VolumeSource{PersistentVolumeClaim: &core.PersistentVolumeClaimVolumeSource{ClaimName: "shared"}}}}
	if platform == "windows" {
		hostPath(&p, "tools", `C:\LabInputs`, `C:\tools`, false)
		p.Spec.Containers[0].VolumeMounts[1].ReadOnly = true
	}
	return p
}
func objects() []any { return objectsForQualification(false) }
func objectsForQualification(candidate bool) []any {
	o := []any{core.Namespace{TypeMeta: meta.TypeMeta{APIVersion: "v1", Kind: "Namespace"}, ObjectMeta: meta.ObjectMeta{Name: ns}}, core.ServiceAccount{TypeMeta: meta.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"}, ObjectMeta: metadata("csi")}}
	// These privileges match the provisioner/attacher/resizer and node responsibilities;
	// no cluster-admin binding or credentials leave the disposable controller VM.
	rules := []rbac.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"persistentvolumes", "persistentvolumeclaims", "persistentvolumeclaims/status", "events"}, Verbs: []string{"get", "list", "watch", "create", "delete", "update", "patch"}},
		{APIGroups: []string{""}, Resources: []string{"nodes", "pods", "secrets"}, Verbs: []string{"get", "list", "watch"}},
		{APIGroups: []string{"storage.k8s.io"}, Resources: []string{"storageclasses", "csinodes", "volumeattachments", "volumeattachments/status"}, Verbs: []string{"get", "list", "watch", "update", "patch"}},
		{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, Verbs: []string{"get", "list", "watch", "create", "delete", "update"}},
	}
	o = append(o, rbac.ClusterRole{TypeMeta: meta.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole"}, ObjectMeta: meta.ObjectMeta{Name: ns}, Rules: rules}, rbac.ClusterRoleBinding{TypeMeta: meta.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"}, ObjectMeta: meta.ObjectMeta{Name: ns}, RoleRef: rbac.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: ns}, Subjects: []rbac.Subject{{Kind: "ServiceAccount", Name: "csi", Namespace: ns}}})
	o = append(o, storage.CSIDriver{TypeMeta: meta.TypeMeta{APIVersion: "storage.k8s.io/v1", Kind: "CSIDriver"}, ObjectMeta: meta.ObjectMeta{Name: driver}, Spec: storage.CSIDriverSpec{AttachRequired: ptr(true), PodInfoOnMount: ptr(true), VolumeLifecycleModes: []storage.VolumeLifecycleMode{storage.VolumeLifecyclePersistent}}}, storage.StorageClass{TypeMeta: meta.TypeMeta{APIVersion: "storage.k8s.io/v1", Kind: "StorageClass"}, ObjectMeta: meta.ObjectMeta{Name: ns}, Provisioner: driver, AllowVolumeExpansion: ptr(true)})
	backend := basePod("backend", "linux")
	backend.Spec.HostNetwork = true
	backend.Spec.Containers = []core.Container{container("weed", runtimeImage("linux", true), []string{"/usr/bin/weed"}, "server", "-ip=192.0.2.10", "-dir=/data", "-filer", "-master.volumeSizeLimitMB=64", "-volume.max=5")}
	// Match the standalone native Windows suite's UTF-8 byte budget. Windows
	// still enforces 255 UTF-16 units; Linux retains its 255-byte mount limit.
	// Set this globally on the isolated filer: rename also uses that limit.
	backend.Spec.Containers[0].Env = []core.EnvVar{{Name: "WEED_FILER_OPTIONS_MAX_FILE_NAME_LENGTH", Value: "1020"}}
	backend.Spec.Volumes = []core.Volume{{Name: "data", VolumeSource: core.VolumeSource{EmptyDir: &core.EmptyDirVolumeSource{}}}}
	backend.Spec.Containers[0].VolumeMounts = []core.VolumeMount{{Name: "data", MountPath: "/data"}}
	o = append(o, backend)
	// The default collection consumes the small embedded server's slots for
	// filer logs. CSI allocates a separate collection per PVC: provide explicit
	// additional slots instead of confusing slot exhaustion with disk capacity.
	capacity := basePod("capacity", "linux")
	capacity.Spec.HostNetwork = true
	capacity.Spec.Containers = []core.Container{container("volume", runtimeImage("linux", true), []string{"/usr/bin/weed"}, "volume", "-ip=192.0.2.10", "-port=8081", "-master=192.0.2.10:9333", "-dir=/data", "-max=32")}
	capacity.Spec.Volumes = []core.Volume{{Name: "data", VolumeSource: core.VolumeSource{EmptyDir: &core.EmptyDirVolumeSource{}}}}
	capacity.Spec.Containers[0].VolumeMounts = []core.VolumeMount{{Name: "data", MountPath: "/data"}}
	o = append(o, capacity)
	controller := basePod("controller", "linux")
	controller.Spec.ServiceAccountName = "csi"
	controller.Spec.Containers = []core.Container{container("driver", linuxImage, []string{"/usr/local/bin/seaweedfs-csi-driver"}, "--endpoint=unix:///csi/csi.sock", "--filer=192.0.2.10:8888", "--driverName="+driver, "--components=controller", "--attacher=true")}
	controller.Spec.Containers[0].Image = runtimeImage("linux", false)
	if controller.Spec.Containers[0].Image != linuxImage {
		controller.Spec.Containers[0].Command = []string{"/seaweedfs-csi-driver"}
	}
	for _, s := range []struct{ name, version string }{{"provisioner", "v3.5.0"}, {"attacher", "v4.3.0"}, {"resizer", "v1.8.0"}} {
		controller.Spec.Containers = append(controller.Spec.Containers, container(s.name, "registry.k8s.io/sig-storage/csi-"+s.name+":"+s.version, nil, "--csi-address=/csi/csi.sock", "--leader-election", "--leader-election-namespace="+ns))
	}
	controller.Spec.Volumes = []core.Volume{{Name: "socket", VolumeSource: core.VolumeSource{EmptyDir: &core.EmptyDirVolumeSource{}}}}
	for i := range controller.Spec.Containers {
		controller.Spec.Containers[i].VolumeMounts = []core.VolumeMount{{Name: "socket", MountPath: "/csi"}}
	}
	o = append(o, controller)
	for _, platform := range []string{"linux", "windows"} {
		o = append(o, daemon(nodePodForQualification(platform, true, candidate)), daemon(nodePodForQualification(platform, false, candidate)))
	}
	o = append(o, core.PersistentVolumeClaim{TypeMeta: meta.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"}, ObjectMeta: metadata("shared"), Spec: core.PersistentVolumeClaimSpec{StorageClassName: ptr(ns), AccessModes: []core.PersistentVolumeAccessMode{core.ReadWriteMany}, Resources: core.VolumeResourceRequirements{Requests: core.ResourceList{core.ResourceStorage: resource.MustParse("1Gi")}}}})
	return o
}
func kubectl(input []byte, args ...string) ([]byte, error) {
	return kubectlWithTimeout(12*time.Minute, input, args...)
}
func kubectlWithTimeout(timeout time.Duration, input []byte, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/local/bin/k0s", append([]string{"kubectl"}, args...)...)
	cmd.Stdin = bytes.NewReader(input)
	return kubectlOutput(cmd)
}

func kubectlOutput(cmd *exec.Cmd) ([]byte, error) {
	// PowerShell writes CLIXML diagnostics to stderr without respecting stdout
	// line boundaries. Never merge that stream into JSON or completion evidence.
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err != nil {
		return stdout.Bytes(), fmt.Errorf("%w: %s", err, stderr.String())
	}
	return stdout.Bytes(), nil
}
func TestManifestContracts(t *testing.T) {
	for _, platform := range []string{"linux", "windows"} {
		p := clientPod(platform)
		if p.Spec.HostNetwork || p.Spec.SecurityContext != nil {
			t.Fatal("workload must be an ordinary container")
		}
		if p.Spec.Volumes[0].PersistentVolumeClaim == nil || p.Spec.Volumes[0].HostPath != nil {
			t.Fatal("test data bypasses CSI")
		}
		for _, mount := range []bool{false, true} {
			d := daemon(nodePod(platform, mount))
			if _, err := json.Marshal(d); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, o := range objects() {
		if _, err := json.Marshal(o); err != nil {
			t.Fatal(err)
		}
	}
}
func TestClientRunNamesRemainScoped(t *testing.T) {
	old := *clientRun
	t.Cleanup(func() { *clientRun = old })
	*clientRun = "preserve-b"
	for _, platform := range []string{"linux", "windows"} {
		p := clientPod(platform)
		if p.Name != "client-"+platform+"-preserve-b" || p.Namespace != ns || p.Spec.Volumes[0].PersistentVolumeClaim.ClaimName != "shared" {
			t.Fatalf("A/B client escaped expected namespace or PVC: %+v", p)
		}
	}
}
func TestCSIStockWinFsp(t *testing.T) {
	runCSIQualification(t, nil, false)
}

// Targeted recovery qualification retains Git LFS, native persistence, mixed
// reads/writes, remount and reboot assertions. Its distinct marker must never
// be interpreted as full WinFsp native conformance qualification.
func TestCSIRecoveryQualification(t *testing.T) {
	runCSIQualification(t, nil, true)
}

func TestCSICandidateWinFsp(t *testing.T) {
	if !*live {
		t.Skip("requires disposable mixed-platform Labcontainers Kubernetes fixture")
	}
	manifest, err := loadCandidateManifest(*candidateManifestPath, *candidateManifestSHA256)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := candidateNativeInput(*nativeTestExecutable, *candidateNativeTestSHA256); err != nil {
		t.Fatal(err)
	}
	runCSIQualification(t, &manifest, false)
}

func runCSIQualification(t *testing.T, candidate *candidateManifest, recoveryOnly bool) {
	t.Helper()
	if !*live {
		t.Skip("requires disposable mixed-platform Labcontainers Kubernetes fixture")
	}
	if _, err := os.Stat("/mnt/qualification"); err != nil {
		t.Fatal("offline fixture media missing", err)
	}
	assertFixture(t)
	if _, _, err := nonCandidateAttestation(); err != nil {
		t.Fatal(err)
	}
	if err := validateSplitImages(); err != nil {
		t.Fatal(err)
	}
	prepareSplitOfflineImages(t)
	if *candidateMSISHA256 != "" {
		if candidate == nil {
			t.Fatal("MSI bootstrap requires the candidate lane")
		}
		bootstrapCandidateMSI(t, *candidate)
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
	defer func() {
		for _, args := range [][]string{{"get", "pods,pvc", "-n", ns, "-o", "wide"}, {"get", "events", "-n", ns, "--sort-by=.metadata.creationTimestamp"}, {"describe", "pods", "-n", ns}} {
			out, _ := kubectl(nil, args...)
			t.Logf("diagnostic %v\n%s", args, out)
		}
		out, _ := kubectl(nil, "get", "pods", "-n", ns, "-o", "json")
		var pods core.PodList
		_ = json.Unmarshal(out, &pods)
		for _, p := range pods.Items {
			for _, c := range append(p.Spec.InitContainers, p.Spec.Containers...) {
				out, _ := kubectl(nil, "logs", "-n", ns, p.Name, "-c", c.Name, "--tail=200")
				t.Logf("logs %s/%s\n%s", p.Name, c.Name, out)
			}
		}
	}()
	apply := func(o any) {
		t.Helper()
		b, err := json.Marshal(o)
		if err != nil {
			t.Fatal(err)
		}
		out, err := kubectl(b, "apply", "-f", "-")
		t.Logf("%s", out)
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, o := range objectsForQualification(candidate != nil) {
		apply(o)
	}
	for _, platform := range []string{"linux", "windows"} {
		for _, role := range []string{"mount", "node"} {
			run("rollout", "status", "-n", ns, "daemonset/"+role+"-"+platform, "--timeout=8m")
		}
	}
	for _, platform := range []string{"linux", "windows"} {
		// A retained-cluster rerun may change an immutable container command.
		// Recreate only the lab's workload pods, preserving the PVC and data.
		run("delete", "pod", "-n", ns, clientName(platform), "--ignore-not-found=true", "--wait=true")
		apply(clientPod(platform))
	}
	run("wait", "-n", ns, "pod/"+clientName("linux"), "pod/"+clientName("windows"), "--for=condition=Ready", "--timeout=10m")
	if *splitImages["driver-linux"] != "" {
		var pods core.PodList
		if err := json.Unmarshal(run("get", "pods", "-n", ns, "-o", "json"), &pods); err != nil {
			t.Fatal(err)
		}
		if err := verifySplitImageRuntime(pods); err != nil {
			t.Fatal(err)
		}
		t.Log("SPLIT_CSI_RUNTIME_IMAGES_ATTESTED")
	}
	// Reject a mismatched matrix/network before the expensive filesystem suite.
	captureWindowsNetwork(t)
	checkController := watchCSIController(t)
	token := fmt.Sprint(time.Now().UnixNano())
	testLinuxRoot := "/data/qualification-" + token
	testWindowsRoot := `C:\data\qualification-` + token
	run("exec", "-n", ns, clientName("linux"), "--", "mkdir", "-p", testLinuxRoot+"/mixed/.sync", testLinuxRoot+"/native")
	attestation, marker, attestationErr := nonCandidateAttestation()
	if attestationErr != nil {
		t.Fatal(attestationErr)
	}
	if candidate != nil {
		nativePath, err := candidateNativeInput(*nativeTestExecutable, *candidateNativeTestSHA256)
		if err != nil {
			t.Fatal(err)
		}
		attestation = candidateAttestation(*candidate, nativePath, *candidateNativeTestSHA256)
		marker = "CANDIDATE_WINFSP_ATTESTED:" + candidate.DriverSourceRevision + ":" + candidate.DriverSourceArchiveSHA256
	}
	attest := func(stage string) {
		t.Helper()
		driverEvidence := run(append([]string{"exec", "-n", ns, "daemonset/mount-windows", "-c", "plugin", "--"}, ps(attestation)...)...)
		if !containsExactLine(string(driverEvidence), marker) {
			t.Fatalf("missing exact WinFsp driver evidence at %s", stage)
		}
	}
	attest("pre-workload")
	runCSIGitLFS(t, testWindowsRoot)
	nativeRoot := testWindowsRoot + `\native`
	nativeFilerRoot := path.Join(csiFilerRoot(t), "qualification-"+token, "native")
	if !recoveryOnly {
		runCSINative(t, nativeRoot, nativeFilerRoot, "")
	}
	runCSINative(t, nativeRoot, nativeFilerRoot, "write")
	runCSIMixedRecovery(t, testLinuxRoot, testWindowsRoot, nativeRoot, nativeFilerRoot, token, run, apply)
	attest("post-recovery")
	checkController()
	if *emitCrashPlan {
		plan, err := existingPersistencePlan(os.Args[1:], token, csiFilerRoot(t))
		if err != nil {
			t.Fatal(err)
		}
		fmt.Println(plan)
	}
	if recoveryOnly {
		fmt.Println("CSI_RECOVERY_QUALIFICATION_COMPLETE")
	} else if candidate != nil {
		fmt.Println("CSI_CANDIDATE_DRIVER_QUALIFICATION_COMPLETE")
	} else {
		fmt.Println("CSI_QUALIFICATION_COMPLETE")
	}
}

func runCSIGitLFS(t *testing.T, testWindowsRoot string) {
	t.Helper()
	// Execute the existing regression function, not its standalone mount bootstrap.
	// Parsing the AST selects exactly the two named functions without dot-sourcing
	// the script (which would create a second mount and bypass Kubernetes CSI).
	source, err := os.ReadFile(*smokeSource)
	if err != nil {
		t.Fatal(err)
	}
	gitScript := `$p=Start-Process C:\tools\Git-2.51.0-64-bit.exe -Wait -PassThru -ArgumentList '/VERYSILENT','/NORESTART','/DIR=C:\Git'; if($p.ExitCode -ne 0){throw "Git installer exit $($p.ExitCode)"}; $env:PATH='C:\Git\cmd;'+$env:PATH; & git --version; if($LASTEXITCODE -ne 0){throw 'git unavailable'}; & git lfs version; if($LASTEXITCODE -ne 0){throw 'lfs unavailable'}; $text=[Text.Encoding]::UTF8.GetString([Convert]::FromBase64String([Console]::In.ReadToEnd())); $tokens=$null; $errors=$null; $ast=[Management.Automation.Language.Parser]::ParseInput($text,[ref]$tokens,[ref]$errors); if($errors.Count){throw 'regression source parse failed'}; foreach($name in @('Assert','Invoke-GitLfsTempMetadataTest')) { $f=@($ast.FindAll({param($n) $n -is [Management.Automation.Language.FunctionDefinitionAst] -and $n.Name -eq $name},$true)); if($f.Count -ne 1){throw "expected exactly one function $name"}; Invoke-Expression $f[0].Extent.Text }; $script:failures=0; $GitIterations=20; Invoke-GitLfsTempMetadataTest C:\data; if($script:failures){throw "LFS assertions failed: $script:failures"}; Write-Output 'CSI_LFS_COMPLETE'`
	// Retained reruns get a new directory; preserve every earlier failure artifact.
	gitScript = strings.Replace(gitScript, `Invoke-GitLfsTempMetadataTest C:\data`, `Invoke-GitLfsTempMetadataTest `+testWindowsRoot, 1)
	gitScript = gitTrustScript(testWindowsRoot) + "; " + gitScript
	lfsOutput, err := kubectl([]byte(base64.StdEncoding.EncodeToString(source)), append([]string{"exec", "-i", "-n", ns, clientName("windows"), "--"}, ps(gitScript)...)...)
	t.Logf("Git LFS regression: %s", lfsOutput)
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"PASS: Git LFS filter is active", "PASS: Git LFS seed commit succeeds", "PASS: Git LFS status iteration 20 reports all 32 modified assets", "CSI_LFS_COMPLETE"} {
		if !strings.Contains(string(lfsOutput), marker) {
			t.Fatalf("missing LFS evidence: %s", marker)
		}
	}
}

// Both entry points use the same byte, remount, reboot and sandbox-teardown
// oracles. A targeted recovery pass is not full native-suite qualification.
func runCSIMixedRecovery(t *testing.T, testLinuxRoot, testWindowsRoot, nativeRoot, nativeFilerRoot, token string, run func(...string) []byte, apply func(any)) {
	t.Helper()
	runPhase := func(phase string) {
		var wg sync.WaitGroup
		for _, platform := range []string{"linux", "windows"} {
			wg.Add(1)
			go func(platform string) {
				defer wg.Done()
				binary, root := "/usr/local/bin/mixed-linux", testLinuxRoot+"/mixed"
				if platform == "windows" {
					binary, root = `C:\tools\mixed-windows.exe`, testWindowsRoot+`\mixed`
				}
				out, err := kubectl(nil, "exec", "-n", ns, clientName(platform), "--", binary, root, platform, phase, token)
				t.Logf("%s %s: %s", platform, phase, out)
				if err != nil {
					t.Errorf("%s %s: %v", platform, phase, err)
				}
				if !strings.Contains(string(out), "MIXED_COMPLETE:"+token+":"+platform+":"+phase) {
					t.Errorf("%s %s: missing completion evidence", platform, phase)
				}
			}(platform)
		}
		wg.Wait()
		if t.Failed() {
			t.FailNow()
		}
	}
	for _, phase := range []string{"seed", "verify-seed", "cache-coherence-files", "rewrite", "verify-rewrite", "rename-delete", "verify-final"} {
		runPhase(phase)
	}
	runLinuxSupervisorRecovery(t, testLinuxRoot, token, run)
	for _, platform := range []string{"linux", "windows"} {
		run("delete", "pod", "-n", ns, clientName(platform), "--wait=true")
		apply(clientPod(platform))
	}
	run("wait", "-n", ns, "pod/"+clientName("linux"), "pod/"+clientName("windows"), "--for=condition=Ready", "--timeout=10m")
	runPhase("verify-remount")
	runCSINative(t, nativeRoot, nativeFilerRoot, "verify")
	// A clean guest reboot is distinct from power loss. Require a new kernel
	// boot ID, then ordinary-pod readiness and the full byte oracle again.
	before := strings.TrimSpace(string(run("get", "node", "windows", "-o", "jsonpath={.status.nodeInfo.bootID}")))
	beforeNetwork := captureWindowsNetwork(t)
	if before == "" {
		t.Fatal("missing Windows boot identity")
	}
	run(append([]string{"exec", "-n", ns, "daemonset/mount-windows", "-c", "plugin", "--"}, ps(`& shutdown.exe /r /t 10 /f; if($LASTEXITCODE -ne 0){throw 'reboot request failed'}`)...)...)
	deadline := time.Now().Add(10 * time.Minute)
	rebooted := false
	for time.Now().Before(deadline) {
		out, err := kubectl(nil, "get", "node", "windows", "-o", "json")
		var n core.Node
		if err == nil && json.Unmarshal(out, &n) == nil && n.Status.NodeInfo.BootID != "" && n.Status.NodeInfo.BootID != before {
			for _, condition := range n.Status.Conditions {
				if condition.Type == core.NodeReady && condition.Status == core.ConditionTrue {
					rebooted = true
				}
			}
		}
		if rebooted {
			break
		}
		time.Sleep(5 * time.Second)
	}
	if !rebooted {
		t.Fatal("Windows did not return Ready with a different boot ID")
	}
	run("wait", "-n", ns, "pod/"+clientName("linux"), "pod/"+clientName("windows"), "--for=condition=Ready", "--timeout=5m")
	afterNetwork := captureWindowsNetwork(t)
	assertNetworkPreserved(t, beforeNetwork, afterNetwork)
	runPhase("verify-remount")
	runCSINative(t, nativeRoot, nativeFilerRoot, "verify")
	// Readiness alone previously hid a sandbox that could never be deleted.
	// Require normal teardown, actual HNS resource removal and fresh-pod reads.
	run("delete", "pod", "-n", ns, clientName("windows"), "--wait=true", "--timeout=3m")
	removed := `$namespace=` + psLiteral(afterNetwork.NamespaceID) + `; $endpoint=` + psLiteral(afterNetwork.EndpointID) + `;
if(@(Get-HnsNamespace | Where-Object {$_.ID -eq $namespace}).Count){throw 'deleted pod left its HNS namespace'};
if(@(Get-HnsEndpoint | Where-Object {$_.ID -eq $endpoint}).Count){throw 'deleted pod left its HNS endpoint'};
Write-Output 'CSI_SANDBOX_TEARDOWN_COMPLETE'`
	out := run(append([]string{"exec", "-n", ns, "daemonset/mount-windows", "-c", "plugin", "--"}, ps(removed)...)...)
	if !strings.Contains(string(out), "CSI_SANDBOX_TEARDOWN_COMPLETE") {
		t.Fatal("missing sandbox teardown evidence")
	}
	apply(clientPod("windows"))
	run("wait", "-n", ns, "pod/"+clientName("windows"), "--for=condition=Ready", "--timeout=5m")
	captureWindowsNetwork(t)
	runPhase("verify-remount")
}
