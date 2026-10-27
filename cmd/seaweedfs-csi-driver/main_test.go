package main

import (
	"testing"

	"github.com/seaweedfs/seaweedfs-csi-driver/pkg/driver"
	flag "github.com/seaweedfs/seaweedfs/weed/util/fla9"
)

// Node plugins need no platform path flags: the defaults are the platform's
// cache base and kubelet's CSI plugins directory, so staging rediscovery is
// on unless --stagingScanDir= turns it off.
func TestPathFlagDefaultsArePlatformDefaults(t *testing.T) {
	for name, want := range map[string]string{
		"cacheDir":       driver.DefaultCacheDir,
		"stagingScanDir": driver.DefaultStagingScanDir,
	} {
		f := flag.Lookup(name)
		if f == nil {
			t.Fatalf("flag %s is not defined", name)
		}
		if f.DefValue != want {
			t.Errorf("--%s default = %q, want %q", name, f.DefValue, want)
		}
	}
}
