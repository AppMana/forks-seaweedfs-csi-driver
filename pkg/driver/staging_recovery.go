package driver

import (
	"encoding/json"
	"os"
	"path"
	"path/filepath"

	"github.com/seaweedfs/seaweedfs/weed/glog"
)

// kubeletVolData is the subset of kubelet's vol_data.json written next
// to each CSI staging (globalmount) directory.
type kubeletVolData struct {
	DriverName   string `json:"driverName"`
	VolumeHandle string `json:"volumeHandle"`
}

// recoverStagedVolumesFromDisk rebuilds the in-memory volume map from
// kubelet's staging directories after a plugin restart. Without this, a
// staged volume whose weed mount later dies is invisible to the health
// monitor and is never re-staged: kubelet does not call NodeStageVolume
// again while it believes the volume is staged, so the volume stays
// broken until its pods are deleted.
//
// scanDir is the kubelet CSI plugins directory, e.g.
// /var/lib/kubelet/plugins/kubernetes.io/csi; the per-volume layout
// underneath is <scanDir>/<driverName>/<hash>/{globalmount,vol_data.json}.
//
// Volumes recovered this way get an empty (non-nil) volume context: a
// dynamically provisioned volume re-mounts identically from defaults
// derived from its volume handle. Statically provisioned volumes with a
// custom "path"/"collection" context cannot be fully reconstructed from
// vol_data.json; for those the health monitor logs the re-stage and the
// mount falls back to handle-derived defaults.
func (ns *NodeServer) recoverStagedVolumesFromDisk(scanDir string) {
	if scanDir == "" {
		return
	}
	base := filepath.Join(scanDir, ns.Driver.name)
	entries, err := os.ReadDir(base)
	if err != nil {
		if !os.IsNotExist(err) {
			glog.Warningf("staging recovery: cannot read %s: %v", base, err)
		}
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		volDir := filepath.Join(base, e.Name())
		data, err := os.ReadFile(filepath.Join(volDir, "vol_data.json"))
		if err != nil {
			glog.Warningf("staging recovery: skipping %s: %v", volDir, err)
			continue
		}
		var vd kubeletVolData
		if err := json.Unmarshal(data, &vd); err != nil || vd.VolumeHandle == "" {
			glog.Warningf("staging recovery: skipping %s: unusable vol_data.json (%v)", volDir, err)
			continue
		}
		if vd.DriverName != "" && vd.DriverName != ns.Driver.name {
			continue
		}
		if _, loaded := ns.volumes.Load(vd.VolumeHandle); loaded {
			continue
		}
		stagingPath := filepath.Join(volDir, "globalmount")
		vol := ns.rebuildVolumeFromStaging(vd.VolumeHandle, stagingPath)
		// recoverVolume refuses nil contexts; an empty context lets the
		// health monitor re-stage with handle-derived defaults.
		vol.volContext = map[string]string{}
		ns.recoverPublishPathsFromDisk(scanDir, vd.VolumeHandle, vol)
		ns.volumes.Store(vd.VolumeHandle, vol)
		if ns.checkHealth(stagingPath) == healthOK {
			glog.Infof("staging recovery: volume %s healthy at %s, tracking", vd.VolumeHandle, stagingPath)
		} else {
			glog.Warningf("staging recovery: volume %s unhealthy at %s, health monitor will re-stage", vd.VolumeHandle, stagingPath)
		}
	}
}

// recoverPublishPathsFromDisk restores the pod bind mounts associated with a
// staged volume. Kubelet does not replay NodePublishVolume after a node-plugin
// restart when the target path still exists, even when its FUSE transport is
// disconnected. Tracking these paths lets the health monitor re-bind them
// after it recreates the staging mount.
func (ns *NodeServer) recoverPublishPathsFromDisk(scanDir, volumeID string, vol *Volume) {
	kubeletRoot := filepath.Clean(filepath.Join(scanDir, "..", "..", ".."))
	podsDir := filepath.Join(kubeletRoot, "pods")
	pods, err := os.ReadDir(podsDir)
	if err != nil {
		if !os.IsNotExist(err) {
			glog.Warningf("staging recovery: cannot read pod mounts under %s: %v", podsDir, err)
		}
		return
	}

	volumeName := path.Base(filepath.ToSlash(volumeID))
	for _, pod := range pods {
		if !pod.IsDir() {
			continue
		}
		volumeDir := filepath.Join(podsDir, pod.Name(), "volumes", "kubernetes.io~csi", volumeName)
		publishPath := filepath.Join(volumeDir, "mount")
		if _, err := os.Lstat(publishPath); err != nil && !isCorruptedMount(err) {
			// A partial recovery may already have removed the dead mount
			// child. Kubelet still owns the publish and records its identity
			// beside that child, so recover it from vol_data.json instead of
			// waiting for a NodePublish call that kubelet will not replay.
			data, readErr := os.ReadFile(filepath.Join(volumeDir, "vol_data.json"))
			if readErr != nil {
				continue
			}
			var vd kubeletVolData
			if json.Unmarshal(data, &vd) != nil || vd.VolumeHandle != volumeID {
				continue
			}
			if vd.DriverName != "" && vd.DriverName != ns.Driver.name {
				continue
			}
		}
		vol.AddPublishPath(publishPath, false)
		glog.Infof("staging recovery: volume %s tracking publish path %s", volumeID, publishPath)
	}
}
