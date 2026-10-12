//go:build linux && integration

package driver

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/seaweedfs/go-fuse/v2/fs"
	"github.com/seaweedfs/go-fuse/v2/fuse"
	"golang.org/x/sys/unix"
)

// A real FUSE daemon stands in for weed mount: the test binary re-executes
// itself with these variables set and serves a loopback filesystem of the
// backing directory, which plays the filer. Killing it leaves exactly what a
// mount-service restart leaves: a kernel mount whose daemon is gone.
const (
	fuseServeEnv   = "CSI_TEST_FUSE_SERVE"
	fuseBackingEnv = "CSI_TEST_FUSE_BACKING"
	fuseTargetEnv  = "CSI_TEST_FUSE_TARGET"
	fuseSourceEnv  = "CSI_TEST_FUSE_SOURCE"
)

func TestMain(m *testing.M) {
	if os.Getenv(fuseServeEnv) == "1" {
		serveLoopbackFuse(os.Getenv(fuseBackingEnv), os.Getenv(fuseTargetEnv), os.Getenv(fuseSourceEnv))
		return
	}
	os.Exit(m.Run())
}

// serveLoopbackFuse mounts backing at target under the given source and
// fuse.seaweedfs type, as weed mount does, and unmounts on SIGTERM.
func serveLoopbackFuse(backing, target, source string) {
	root, err := fs.NewLoopbackRoot(backing)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loopback root %s: %v\n", backing, err)
		os.Exit(1)
	}
	_, err = fs.Mount(target, root, &fs.Options{MountOptions: fuse.MountOptions{
		FsName:            source,
		Name:              "seaweedfs",
		AllowOther:        true,
		DirectMountStrict: true,
	}})
	if err != nil {
		fmt.Fprintf(os.Stderr, "mount %s: %v\n", target, err)
		os.Exit(1)
	}
	// Like weed mount (grace.OnInterrupt): unmount the directory and exit,
	// even while another bind of the filesystem keeps it referenced.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM)
	<-signals
	if err := unix.Unmount(target, 0); err != nil {
		fmt.Fprintf(os.Stderr, "unmount %s: %v\n", target, err)
	}
	os.Exit(0)
}

// fuseMountService is the mount service of these tests: every Mount starts
// one FUSE daemon process, and restart kills them all the way a deleted
// mount pod's PID namespace does.
type fuseMountService struct {
	t       *testing.T
	backing string
	source  string

	mu      sync.Mutex
	daemons []*fuseDaemon
}

type fuseDaemon struct {
	cmd  *exec.Cmd
	done chan struct{}
}

func (s *fuseMountService) Mount(target string) (Unmounter, error) {
	if err := os.MkdirAll(target, 0o755); err != nil {
		return nil, err
	}
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(),
		fuseServeEnv+"=1",
		fuseBackingEnv+"="+s.backing,
		fuseTargetEnv+"="+target,
		fuseSourceEnv+"="+s.source,
	)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	d := &fuseDaemon{cmd: cmd, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(d.done)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for !isFuseMountedWithSource(target, s.source) {
		select {
		case <-d.done:
			return nil, fmt.Errorf("FUSE daemon for %s exited before mounting", target)
		default:
		}
		if time.Now().After(deadline) {
			d.kill()
			return nil, fmt.Errorf("FUSE daemon did not mount %s", target)
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.mu.Lock()
	s.daemons = append(s.daemons, d)
	s.mu.Unlock()
	return d, nil
}

// Unmount stops the daemon the way the mount service stops weed mount.
func (d *fuseDaemon) Unmount() error {
	_ = d.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-d.done:
		return nil
	case <-time.After(10 * time.Second):
		d.kill()
		return errors.New("FUSE daemon did not stop on SIGTERM")
	}
}

func (d *fuseDaemon) kill() {
	_ = d.cmd.Process.Kill()
	<-d.done
}

// restart kills every daemon without unmounting, like deleting the mount pod.
func (s *fuseMountService) restart() {
	s.mu.Lock()
	daemons := s.daemons
	s.daemons = nil
	s.mu.Unlock()
	for _, d := range daemons {
		d.kill()
	}
}

// isFuseMountedWithSource reports whether the topmost mount at path is a
// fuse.seaweedfs mount of source.
func isFuseMountedWithSource(path, source string) bool {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	defer f.Close()
	mounted := false
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 10 || unescapeMountField(fields[4]) != path {
			continue
		}
		mounted = false
		for i := 6; i+2 < len(fields); i++ {
			if fields[i] == "-" {
				mounted = fields[i+1] == "fuse.seaweedfs" && fields[i+2] == source
				break
			}
		}
	}
	return mounted
}

// consumer is a running container: its own mount namespace with a private
// bind of the publish path, as the container runtime makes for a
// volumeMount with mountPropagation None, inside a cgroup named after the
// pod UID as kubelet's are.
type consumer struct {
	pid       int
	mountPath string
	cmd       *exec.Cmd
}

func startConsumer(t *testing.T, root, podUID, publishPath string) *consumer {
	t.Helper()
	cgroup := filepath.Join("/sys/fs/cgroup", "csi-test-pod"+podUID)
	if err := os.Mkdir(cgroup, 0o755); err != nil {
		t.Fatalf("create consumer cgroup: %v", err)
	}
	mountPath := filepath.Join(root, "container", "data")
	if err := os.MkdirAll(mountPath, 0o755); err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(root, "consumer.ready")
	cmd := exec.Command("unshare", "--mount", "--propagation", "private", "sh", "-c", fmt.Sprintf(
		`echo $$ > %s/cgroup.procs && mount --bind %s %s && echo $$ > %s && exec sleep 600`,
		cgroup, publishPath, mountPath, ready))
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = os.Remove(cgroup)
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		if data, err := os.ReadFile(ready); err == nil && len(data) > 0 {
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil {
				t.Fatal(err)
			}
			return &consumer{pid: pid, mountPath: mountPath, cmd: cmd}
		}
		if time.Now().After(deadline) {
			t.Fatal("consumer did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// path resolves p inside the consumer's mount namespace.
func (c *consumer) path(p string) string {
	return fmt.Sprintf("/proc/%d/root%s", c.pid, filepath.Join(c.mountPath, p))
}

// works reports whether the consumer can statfs, write and read back through
// its mount, and that the write reached the backing store.
func (c *consumer) works(backing, name string) error {
	var sfs unix.Statfs_t
	if err := unix.Statfs(c.path(""), &sfs); err != nil {
		return fmt.Errorf("statfs: %w", err)
	}
	want := []byte("written by the consumer: " + name)
	if err := os.WriteFile(c.path(name), want, 0o644); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	got, err := os.ReadFile(c.path(name))
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if string(got) != string(want) {
		return fmt.Errorf("read %q, wrote %q", got, want)
	}
	stored, err := os.ReadFile(filepath.Join(backing, name))
	if err != nil || string(stored) != string(want) {
		return fmt.Errorf("write did not reach the backing store: %q %v", stored, err)
	}
	return nil
}

type consumerFixture struct {
	t          *testing.T
	ns         *NodeServer
	svc        *fuseMountService
	volumeID   string
	backing    string
	staging    string
	publish    string
	consumer   *consumer
	capability *csi.VolumeCapability
}

func newConsumerFixture(t *testing.T) *consumerFixture {
	t.Helper()
	skipIfNotRoot(t)
	root := t.TempDir()
	backing := filepath.Join(root, "filer")
	if err := os.MkdirAll(backing, 0o755); err != nil {
		t.Fatal(err)
	}
	const podUID = "6f1a2b3c-4d5e-4f60-8a7b-9c0d1e2f3a4b"
	volumeID := "/buckets/pvc-consumer"
	kubelet := filepath.Join(root, "kubelet")
	staging := filepath.Join(kubelet, "plugins", "kubernetes.io", "csi", "seaweedfs-csi-driver", "0123abcd", "globalmount")
	publish := filepath.Join(kubelet, "pods", podUID, "volumes", "kubernetes.io~csi", "pvc-consumer", "mount")
	svc := &fuseMountService{t: t, backing: backing, source: "seaweedfs-filer.test:8888:" + volumeID}
	t.Cleanup(svc.restart)
	t.Cleanup(func() {
		for _, p := range []string{publish, staging} {
			for unix.Unmount(p, unix.MNT_DETACH) == nil {
			}
		}
	})

	// The production NodeServer (NewNodeServer) with the mount service, the
	// orchestrator API and the VAC store replaced, and no background ticker:
	// the test drives health-monitor sweeps itself.
	ns := &NodeServer{
		Driver: &SeaweedFsDriver{
			name:            "seaweedfs-csi-driver",
			CacheDir:        filepath.Join(root, "cache"),
			volumeSocketDir: filepath.Join(root, "sockets"),
		},
		volumeMutexes: NewKeyMutex(),
		stopCh:        make(chan struct{}),
		mounterFactory: func(string, bool, *SeaweedFsDriver, map[string]string) (Mounter, error) {
			return svc, nil
		},
		capacityFn:        func(string) (int64, error) { return 0, errors.New("no orchestrator in this test") },
		vacLoader:         func(context.Context, string) (map[string]string, error) { return nil, nil },
		isHealthyFn:       isStagingPathHealthy,
		healthProbeFn:     stagingPathHealth,
		cleanupStagingFn:  cleanupStaleStagingPath,
		detachStagingFn:   detachDeadMountPoint,
		unmountFn:         unmountVolume,
		bindMountFn:       defaultBindMount,
		volumeUnmounterFn: func(string) (Unmounter, error) { return nil, errors.New("no mount service client") },
	}
	t.Cleanup(ns.NodeCleanup)

	f := &consumerFixture{
		t: t, ns: ns, svc: svc, volumeID: volumeID, backing: backing, staging: staging, publish: publish,
		capability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER},
		},
	}
	f.stage()
	f.publishVolume()
	f.consumer = startConsumer(t, root, podUID, publish)
	if err := f.consumer.works(backing, "before"); err != nil {
		t.Fatalf("consumer cannot use the freshly published volume: %v", err)
	}
	return f
}

func (f *consumerFixture) stage() {
	f.t.Helper()
	if _, err := f.ns.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId: f.volumeID, StagingTargetPath: f.staging, VolumeCapability: f.capability,
	}); err != nil {
		f.t.Fatalf("NodeStageVolume: %v", err)
	}
}

func (f *consumerFixture) publishVolume() {
	f.t.Helper()
	if _, err := f.ns.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId: f.volumeID, StagingTargetPath: f.staging, TargetPath: f.publish, VolumeCapability: f.capability,
	}); err != nil {
		f.t.Fatalf("NodePublishVolume: %v", err)
	}
}

// restageUnderRunningConsumer is what kubelet has been seen doing to
// running pods: unpublish, unstage, stage and publish the volume again
// without restarting the container. Unstage stops the FUSE daemon the
// container's bind belongs to.
func (f *consumerFixture) restageUnderRunningConsumer() {
	f.t.Helper()
	if _, err := f.ns.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId: f.volumeID, TargetPath: f.publish,
	}); err != nil {
		f.t.Fatalf("NodeUnpublishVolume: %v", err)
	}
	if _, err := f.ns.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
		VolumeId: f.volumeID, StagingTargetPath: f.staging,
	}); err != nil {
		f.t.Fatalf("NodeUnstageVolume: %v", err)
	}
	f.stage()
	f.publishVolume()
}

// requireConsumerRecovers runs health-monitor sweeps, as the 30 s ticker
// would, and requires the consumer to work again within maxSweeps.
func (f *consumerFixture) requireConsumerRecovers(name string, maxSweeps int) {
	f.t.Helper()
	var err error
	for sweep := 1; sweep <= maxSweeps; sweep++ {
		f.ns.runHealthCheckTick()
		f.ns.recoveryWg.Wait()
		if err = f.consumer.works(f.backing, fmt.Sprintf("%s-%d", name, sweep)); err == nil {
			f.t.Logf("consumer works again after %d health-monitor sweep(s)", sweep)
			return
		}
	}
	f.t.Fatalf("consumer still cannot use its volume after %d health-monitor sweeps: %v", maxSweeps, err)
}

// TestIntegrationConsumerSurvivesMountServiceRestart: the mount pod is
// replaced while a consumer runs. Its daemon dies, the node plugin
// re-stages, and the consumer must work again without being restarted.
func TestIntegrationConsumerSurvivesMountServiceRestart(t *testing.T) {
	f := newConsumerFixture(t)
	f.svc.restart()
	if err := f.consumer.works(f.backing, "dead"); err == nil {
		t.Fatal("consumer still works after its FUSE daemon was killed; the test exercises nothing")
	}
	f.requireConsumerRecovers("after-restart", defaultUnhealthyThreshold)
}

// TestIntegrationConsumerSurvivesRestageUnderIt: kubelet unstages and
// re-stages the volume of a running pod. The new mount is healthy on the
// host, but the container still holds the old one, whose daemon is gone.
func TestIntegrationConsumerSurvivesRestageUnderIt(t *testing.T) {
	f := newConsumerFixture(t)
	f.restageUnderRunningConsumer()
	if err := f.consumer.works(f.backing, "dead"); err == nil {
		t.Fatal("consumer still works after its volume was unstaged; the test exercises nothing")
	}
	f.requireConsumerRecovers("after-restage", 1)
}

// TestIntegrationConsumerSurvivesMountServiceRestartAfterRestage: a
// consumer whose volume kubelet had re-staged under it earlier, then a
// mount pod replacement. The node
// plugin re-staged after the restart, yet the consumer stayed on a dead
// mount (ENOTCONN) and Ready.
func TestIntegrationConsumerSurvivesMountServiceRestartAfterRestage(t *testing.T) {
	f := newConsumerFixture(t)
	f.restageUnderRunningConsumer()
	f.svc.restart()
	f.requireConsumerRecovers("after-restage-and-restart", defaultUnhealthyThreshold)
}
