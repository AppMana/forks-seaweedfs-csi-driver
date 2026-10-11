//go:build linux

package mountmanager

import (
	"bufio"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/seaweedfs/seaweedfs/weed/glog"
)

// removeUnheldInstanceCaches removes the instance cache directories in a
// volume's cacheDir that no process holds a file open or mapped in. weed
// mount names its instance directory after its version, so each weed
// upgrade leaves the previous one behind. Called once the new weed mount
// is up, when it holds its own instance directory open. If the holders
// cannot all be read, nothing is removed.
func removeUnheldInstanceCaches(cacheDir, procRoot string) {
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return
	}
	held, err := heldInstances(filepath.Clean(cacheDir), procRoot)
	if err != nil {
		glog.Warningf("keeping instance caches in %s: cannot read their holders: %v", cacheDir, err)
		return
	}
	for _, e := range entries {
		if !e.IsDir() || held[e.Name()] {
			continue
		}
		dir := filepath.Join(cacheDir, e.Name())
		if err := os.RemoveAll(dir); err != nil {
			glog.Warningf("removing unheld instance cache %s: %v", dir, err)
			continue
		}
		glog.Infof("removed instance cache %s that no running mount holds", dir)
	}
}

// heldInstances returns the names of the entries of cacheDir that hold a
// file some process has open or mapped.
func heldInstances(cacheDir, procRoot string) (map[string]bool, error) {
	procs, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, err
	}
	prefix := cacheDir + string(filepath.Separator)
	held := map[string]bool{}
	note := func(path string) {
		if rel, ok := strings.CutPrefix(path, prefix); ok {
			held[strings.SplitN(rel, string(filepath.Separator), 2)[0]] = true
		}
	}
	for _, p := range procs {
		if _, err := strconv.Atoi(p.Name()); err != nil {
			continue
		}
		dir := filepath.Join(procRoot, p.Name())
		fds, err := os.ReadDir(filepath.Join(dir, "fd"))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue // the process exited
			}
			return nil, err
		}
		for _, fd := range fds {
			if target, err := os.Readlink(filepath.Join(dir, "fd", fd.Name())); err == nil {
				note(target)
			}
		}
		maps, err := os.Open(filepath.Join(dir, "maps"))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, err
		}
		scanner := bufio.NewScanner(maps)
		for scanner.Scan() {
			// address perms offset dev inode pathname
			if fields := strings.Fields(scanner.Text()); len(fields) >= 6 {
				note(strings.Join(fields[5:], " "))
			}
		}
		maps.Close()
	}
	return held, nil
}
