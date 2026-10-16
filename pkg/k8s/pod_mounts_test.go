package k8s

import (
	"context"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func claimedVolume(name, handle, namespace, claim string) *corev1.PersistentVolume {
	pv := newPersistentVolume(name, handle, "1Gi")
	pv.Spec.ClaimRef = &corev1.ObjectReference{Namespace: namespace, Name: claim}
	return pv
}

func TestPodVolumeMountsListsEachContainersMountsOfTheClaim(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ci", Name: "runner"},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{
				{Name: "cache", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "build-cache"}}},
				{Name: "other", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "unrelated"}}},
			},
			InitContainers: []corev1.Container{{Name: "init", VolumeMounts: []corev1.VolumeMount{
				{Name: "cache", MountPath: `C:\seed`, SubPathExpr: "$(POD_NAME)"},
			}}},
			Containers: []corev1.Container{
				{Name: "build", VolumeMounts: []corev1.VolumeMount{
					{Name: "cache", MountPath: `C:\cache`},
					{Name: "cache", MountPath: `C:\deps`, SubPath: "deps"},
					{Name: "other", MountPath: `C:\other`},
				}},
				{Name: "sidecar", VolumeMounts: []corev1.VolumeMount{{Name: "other", MountPath: `C:\other`}}},
			},
		},
	}
	client := fake.NewSimpleClientset(claimedVolume("pvc-1", "/buckets/pvc-1", "ci", "build-cache"), pod)

	got, err := podVolumeMounts(context.Background(), client, "seaweedfs-csi-driver", "/buckets/pvc-1", "ci", "runner")
	if err != nil {
		t.Fatal(err)
	}
	want := [][]VolumeMount{
		// a subPathExpr is expanded by kubelet and unknown here; an absolute
		// subPath is never mapped by the mount
		{{Path: `C:\seed`, SubPath: "/$(POD_NAME)"}},
		{{Path: `C:\cache`}, {Path: `C:\deps`, SubPath: "deps"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("podVolumeMounts = %#v, want %#v", got, want)
	}
}

func TestPodVolumeMountsRejectsClaimFromAnotherNamespace(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ci", Name: "runner"}}
	client := fake.NewSimpleClientset(claimedVolume("pvc-1", "/buckets/pvc-1", "elsewhere", "build-cache"), pod)
	if _, err := podVolumeMounts(context.Background(), client, "seaweedfs-csi-driver", "/buckets/pvc-1", "ci", "runner"); err == nil {
		t.Fatal("a claim outside the pod's namespace must not resolve")
	}
}

func TestPodVolumeMountsRequiresThePodToUseTheClaim(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ci", Name: "runner"}}
	client := fake.NewSimpleClientset(claimedVolume("pvc-1", "/buckets/pvc-1", "ci", "build-cache"), pod)
	if _, err := podVolumeMounts(context.Background(), client, "seaweedfs-csi-driver", "/buckets/pvc-1", "ci", "runner"); err == nil {
		t.Fatal("a pod without the claim must not resolve")
	}
}
