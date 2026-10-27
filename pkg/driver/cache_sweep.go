package driver

import (
	"time"

	"github.com/seaweedfs/seaweedfs/weed/glog"
)

// cacheSweepInterval is how often a running node plugin removes cache
// directories no mount can still use. Startup sweeps once as well.
var cacheSweepInterval = time.Hour

// holdCaches keeps the cache sweep from removing any cache directory until
// the returned release is called. Staging holds it until the volume is
// recorded as staged.
func (ns *NodeServer) holdCaches() (release func()) {
	ns.cacheMu.RLock()
	return ns.cacheMu.RUnlock
}

func (ns *NodeServer) startCacheSweeper(interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				ns.runCacheSweep()
			case <-ns.stopCh:
				return
			}
		}
	}()
}

func (ns *NodeServer) runCacheSweep() {
	defer func() {
		if r := recover(); r != nil {
			glog.Errorf("cache sweep: recovered from panic: %v", r)
		}
	}()
	ns.removeOrphanCacheDirs()
}
