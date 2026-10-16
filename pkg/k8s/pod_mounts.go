package k8s

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// VolumeMount is where a container sees a volume and which subdirectory of
// the volume it shows there.
type VolumeMount struct {
	Path    string
	SubPath string
}

// GetPodVolumeMounts returns, for each container of the pod that mounts the
// claim bound to volumeId, its mounts of that claim.
func GetPodVolumeMounts(ctx context.Context, driverName, volumeId, namespace, podName string) ([][]VolumeMount, error) {
	client, err := newInCluster()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return podVolumeMounts(ctx, client, driverName, volumeId, namespace, podName)
}

func podVolumeMounts(ctx context.Context, client kubernetes.Interface, driverName, volumeId, namespace, podName string) ([][]VolumeMount, error) {
	volume, err := resolvePersistentVolume(ctx, client, driverName, volumeId)
	if err != nil {
		return nil, err
	}
	claim := volume.Spec.ClaimRef
	if claim == nil || claim.Namespace != namespace {
		return nil, fmt.Errorf("persistent volume %q is not bound to a claim in namespace %q", volume.Name, namespace)
	}
	pod, err := client.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get pod %s/%s: %w", namespace, podName, err)
	}
	names := map[string]bool{}
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == claim.Name {
			names[v.Name] = true
		}
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("pod %s/%s does not use claim %q", namespace, podName, claim.Name)
	}
	var views [][]VolumeMount
	for _, containers := range [][]corev1.Container{pod.Spec.InitContainers, pod.Spec.Containers} {
		for _, c := range containers {
			var mounts []VolumeMount
			for _, m := range c.VolumeMounts {
				if !names[m.Name] {
					continue
				}
				subPath := m.SubPath
				if m.SubPathExpr != "" {
					// expanded by kubelet, unknown here; an absolute subPath
					// is never mapped by the mount
					subPath = "/" + m.SubPathExpr
				}
				mounts = append(mounts, VolumeMount{Path: m.MountPath, SubPath: subPath})
			}
			if len(mounts) > 0 {
				views = append(views, mounts)
			}
		}
	}
	return views, nil
}
