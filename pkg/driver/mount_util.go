package driver

import (
	"fmt"
	"sync"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/glog"
)

// statfsProbeTimeout bounds every statfs probe. statfs on a hung FUSE
// daemon can block in the kernel indefinitely, so no caller — a CSI
// request holding the per-volume mutex or a health-monitor sweep — may
// wait on it unbounded. A var so tests can shorten it.
var statfsProbeTimeout = defaultHealthCheckTimeout

// statfsProbe is one in-flight statfs syscall for a path. Callers that
// arrive while it is still blocked attach to it instead of spawning
// another syscall goroutine, so a permanently hung daemon cannot
// accumulate one blocked goroutine per sweep or CSI retry.
type statfsProbe struct {
	done chan struct{}
	err  error
}

var statfsProbes sync.Map // map[string]*statfsProbe

// probeStatfs returns the result of a statfs probe for path,
// deduplicated and bounded. (err, true) means the syscall produced a
// result; (nil, false) means the wait timed out — an inconclusive result
// that must never be treated as proof of a dead mount.
//
// A timed-out probe stays registered until its syscall returns, so later
// callers reuse it rather than pile up blocked goroutines. Once the
// mount at path is detached or replaced, callers must resetStatfsProbe:
// the in-flight result describes the old mount and must not be reused
// for the new one.
func probeStatfs(path string) (error, bool) {
	p, loaded := statfsProbes.LoadOrStore(path, &statfsProbe{done: make(chan struct{})})
	probe := p.(*statfsProbe)
	if !loaded {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					glog.Errorf("statfs probe for %s panicked: %v", path, r)
					probe.err = fmt.Errorf("statfs probe for %s panicked: %v", path, r)
				}
				close(probe.done)
				// CompareAndDelete so a probe orphaned by
				// resetStatfsProbe cannot remove a newer probe's entry.
				statfsProbes.CompareAndDelete(path, probe)
			}()
			probe.err = statfsFn(path)
		}()
	}
	select {
	case <-probe.done:
		return probe.err, true
	case <-time.After(statfsProbeTimeout):
		return nil, false
	}
}

// resetStatfsProbe forgets an in-flight probe for path after the mount
// there was detached or replaced. The orphaned goroutine still exits on
// its own when the syscall returns; it just stops absorbing new callers.
func resetStatfsProbe(path string) {
	statfsProbes.Delete(path)
}
