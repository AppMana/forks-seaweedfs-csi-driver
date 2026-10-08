package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

const runMainEnv = "SEAWEEDFS_MOUNT_TEST_RUN_MAIN"

func TestMain(m *testing.M) {
	if os.Getenv(runMainEnv) == "1" {
		os.Args = []string{os.Args[0], os.Getenv(runMainEnv + "_ARG")}
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// startSupervisor runs main() in a child process with TMPDIR set, and waits
// until it serves /healthz.
func startSupervisor(t *testing.T, tmpdir string) (*lockedBuffer, <-chan error) {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "mount.sock")
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(),
		runMainEnv+"=1",
		runMainEnv+"_ARG=-endpoint=unix://"+socket,
		"TMPDIR="+tmpdir,
	)
	stderr := &lockedBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-exited
	})
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	}}, Timeout: time.Second}
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case err := <-exited:
			t.Fatalf("supervisor exited (%v):\n%s", err, stderr.String())
		default:
		}
		resp, err := client.Get("http://unix/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return stderr, exited
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("supervisor never served /healthz: %v\n%s", err, stderr.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// The supervisor's logs reach the container log through stderr. glog's
// default also writes INFO/WARNING/ERROR files into TMPDIR, which on a node is
// the container layer on the root disk: on appmana-031 (2026-10-08) the full
// root disk made it print "glog: exiting because of error: write
// /tmp/seaweedfs-mount...log.ERROR: no space left on device" and stop logging
// to files. It must never write log files at all.
func TestSupervisorLogsOnlyToStderr(t *testing.T) {
	tmpdir := t.TempDir()
	stderr, _ := startSupervisor(t, tmpdir)
	entries, err := os.ReadDir(tmpdir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("supervisor wrote %s into TMPDIR", e.Name())
	}
	if stderr.Len() == 0 {
		t.Fatal("supervisor logged nothing to stderr")
	}
}

// A TMPDIR that cannot hold files at all (below a regular file, as a full
// disk would) must not stop the supervisor from serving.
func TestSupervisorServesWhenNoFileCanBeCreated(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	stderr, exited := startSupervisor(t, filepath.Join(blocker, "tmp"))
	select {
	case err := <-exited:
		t.Fatalf("supervisor exited (%v):\n%s", err, stderr.String())
	case <-time.After(300 * time.Millisecond):
	}
	if got := stderr.String(); bytes.Contains([]byte(got), []byte("glog: exiting")) {
		t.Fatalf("supervisor gave up on logging:\n%s", got)
	}
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *lockedBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}
