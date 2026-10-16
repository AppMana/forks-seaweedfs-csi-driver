package driver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/seaweedfs/seaweedfs-csi-driver/pkg/k8s"
	"github.com/seaweedfs/seaweedfs-csi-driver/pkg/mountmanager"
	"github.com/seaweedfs/seaweedfs/weed/glog"
)

// A Windows pod sees a volume at its container mount path, which the weed
// mount cannot see, so an absolute symlink the pod creates names a path the
// mount cannot place. Each publish records where every container of the pod
// mounts the volume, in the file the mount reads (-winfspMountRoots); the
// mount stores a target under a mount path shared by all of them
// volume-relative, so the link can be followed from the pod.

const (
	podNameKey      = "csi.storage.k8s.io/pod.name"
	podNamespaceKey = "csi.storage.k8s.io/pod.namespace"
)

// containerMount and mountRootsFile are the weed mount's -winfspMountRoots format.
type containerMount struct {
	Path    string `json:"path"`
	SubPath string `json:"subPath,omitempty"`
}

type mountRootsFile struct {
	Publishes map[string][][]containerMount `json:"publishes"`
}

func (ns *NodeServer) recordMountRoots(ctx context.Context, volumeID, targetPath string, volContext map[string]string) {
	if ns.podVolumeMountsFn == nil {
		return
	}
	// A pod whose mounts are unknown could be the creator of any link, so it
	// records one container without mounts, which the mount never maps for.
	views := [][]containerMount{{}}
	podName, podNamespace := volContext[podNameKey], volContext[podNamespaceKey]
	if podName == "" || podNamespace == "" {
		glog.Warningf("volume %s published to %s without pod info; absolute symlinks stay unmapped", volumeID, targetPath)
	} else if mounts, err := ns.podVolumeMountsFn(ctx, volumeID, podNamespace, podName); err != nil {
		glog.Warningf("volume %s: mount paths of pod %s/%s: %v; absolute symlinks stay unmapped", volumeID, podNamespace, podName, err)
	} else {
		views = make([][]containerMount, 0, len(mounts))
		for _, container := range mounts {
			view := make([]containerMount, 0, len(container))
			for _, m := range container {
				view = append(view, containerMount{Path: m.Path, SubPath: m.SubPath})
			}
			views = append(views, view)
		}
	}
	if err := updateMountRoots(mountmanager.MountRootsPath(ns.Driver.volumeSocketDir, volumeID), targetPath, views); err != nil {
		glog.Warningf("volume %s: record mount paths for %s: %v", volumeID, targetPath, err)
	}
}

func (ns *NodeServer) forgetMountRoots(volumeID, targetPath string) {
	if ns.podVolumeMountsFn == nil {
		return
	}
	if err := updateMountRoots(mountmanager.MountRootsPath(ns.Driver.volumeSocketDir, volumeID), targetPath, nil); err != nil {
		glog.Warningf("volume %s: forget mount paths for %s: %v", volumeID, targetPath, err)
	}
}

// updateMountRoots sets (views != nil) or removes the publish entry for
// targetPath. Callers hold the volume mutex.
func updateMountRoots(file, targetPath string, views [][]containerMount) error {
	roots := mountRootsFile{}
	if data, err := os.ReadFile(file); err == nil {
		if err := json.Unmarshal(data, &roots); err != nil {
			glog.Warningf("replacing unreadable mount roots %s: %v", file, err)
			roots = mountRootsFile{}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if roots.Publishes == nil {
		roots.Publishes = map[string][][]containerMount{}
	}
	if views == nil {
		delete(roots.Publishes, targetPath)
	} else {
		roots.Publishes[targetPath] = views
	}
	if len(roots.Publishes) == 0 {
		if err := os.Remove(file); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	data, err := json.Marshal(roots)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(file), filepath.Base(file)+".*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), file); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// PodVolumeMountsFn resolves a pod's container mounts of a volume.
type PodVolumeMountsFn func(ctx context.Context, volumeID, namespace, name string) ([][]k8s.VolumeMount, error)
