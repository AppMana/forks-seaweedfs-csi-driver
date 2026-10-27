package mountmanager

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/glog"
)

// RequiredMountFlags are the weed mount flags without which a mount is
// unsafe or wrong: its identity (what is mounted where, from which filer),
// its access mode, and the settings that decide how data is written, owned
// and bounded. The CSI node plugin and the mount service can run different
// builds on a node (both DaemonSets roll OnDelete), so when the mount
// service's weed does not define one of these the mount fails with an
// error naming the flag. Every other flag weed does not define is cache or
// tuning, and is dropped with a warning instead.
//
// The node plugin sends its own list with each request (MountRequest.
// RequiredFlags), so a flag a newer node plugin requires is honored by an
// older mount service as well.
var RequiredMountFlags = []string{
	"dir",
	"dirAutoCreate",
	"filer",
	"filer.path",
	"collection",
	"collectionQuotaMB",
	"readOnly",
	"localSocket",
	"cacheDir",
	"umask",
	"winfspMountRoots",
	"map.uid",
	"map.gid",
	"replication",
	"ttl",
	"disk",
	"dlm",
}

// weedProbeTimeout bounds `weed mount -h` and `weed version`.
const weedProbeTimeout = 30 * time.Second

// weedMountFlags is what the mount service's weed binary accepts.
type weedMountFlags struct {
	defined map[string]struct{}
	version string
}

// helpFlagLine matches a flag line of Go's flag.PrintDefaults output, which
// is what `weed mount -h` prints: two spaces, the dash, the name, then an
// optional type name. Usage text lines are indented with four spaces and a
// tab.
var helpFlagLine = regexp.MustCompile(`^  -([^\s=]+)`)

// parseWeedMountHelp returns the flag names listed in `weed mount -h`
// output.
func parseWeedMountHelp(help []byte) map[string]struct{} {
	defined := make(map[string]struct{})
	scanner := bufio.NewScanner(bytes.NewReader(help))
	for scanner.Scan() {
		if m := helpFlagLine.FindStringSubmatch(scanner.Text()); m != nil {
			defined[m[1]] = struct{}{}
		}
	}
	return defined
}

// probeWeedMountFlags asks the weed binary which flags its mount command
// defines and which version it is.
func probeWeedMountFlags(binary string) (*weedMountFlags, error) {
	ctx, cancel := context.WithTimeout(context.Background(), weedProbeTimeout)
	defer cancel()

	// weed prints the usage and exits 2 for -h; the output is what matters.
	help, err := exec.CommandContext(ctx, binary, "mount", "-h").CombinedOutput()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return nil, fmt.Errorf("running %s mount -h: %w", binary, err)
	}
	defined := parseWeedMountHelp(help)
	if len(defined) == 0 {
		first, _, _ := strings.Cut(strings.TrimSpace(string(help)), "\n")
		return nil, fmt.Errorf("%s mount -h listed no flags (%v): %q", binary, err, first)
	}

	version := "unknown version"
	if out, err := exec.CommandContext(ctx, binary, "version").Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				version = line
				break
			}
		}
	}
	return &weedMountFlags{defined: defined, version: version}, nil
}

// mountFlagName returns the flag name of a -name or -name=value argument
// (one or two leading dashes, as Go's flag package accepts).
func mountFlagName(arg string) string {
	name := strings.TrimPrefix(strings.TrimPrefix(arg, "-"), "-")
	name, _, _ = strings.Cut(name, "=")
	return name
}

// filterMountArgs drops the arguments of the mount subcommand that weed does
// not define and that are not required, logging each one. If a required
// flag is not defined it returns an error naming every such flag. Global
// weed flags before the mount subcommand are kept as they are.
func filterMountArgs(volumeID string, args []string, flags *weedMountFlags, required []string) ([]string, error) {
	mountIndex := -1
	for i, arg := range args {
		if arg == "mount" {
			mountIndex = i
			break
		}
	}
	if mountIndex < 0 {
		return args, nil
	}

	requiredSet := make(map[string]struct{}, len(RequiredMountFlags)+len(required))
	for _, name := range RequiredMountFlags {
		requiredSet[name] = struct{}{}
	}
	for _, name := range required {
		requiredSet[name] = struct{}{}
	}

	filtered := append([]string(nil), args[:mountIndex+1]...)
	var missing, dropped []string
	for _, arg := range args[mountIndex+1:] {
		name := mountFlagName(arg)
		if _, ok := flags.defined[name]; ok || !strings.HasPrefix(arg, "-") {
			filtered = append(filtered, arg)
			continue
		}
		if _, ok := requiredSet[name]; ok {
			missing = append(missing, "-"+name)
			continue
		}
		dropped = append(dropped, arg)
	}

	if len(missing) > 0 {
		return nil, fmt.Errorf("weed mount (%s) in the mount service does not define required flag(s) %s; the seaweedfs-mount image is older than the CSI node plugin needs", flags.version, strings.Join(missing, ", "))
	}
	for _, arg := range dropped {
		glog.Warningf("[%s] dropping mount flag %s: weed mount (%s) in the mount service does not define -%s", volumeID, arg, flags.version, mountFlagName(arg))
	}
	return filtered, nil
}
