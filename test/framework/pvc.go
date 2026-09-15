package framework

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

// CreatePVC creates a PVC in the test namespace.
func (f *Framework) CreatePVC(name, size string, mode corev1.PersistentVolumeMode, accessMode corev1.PersistentVolumeAccessMode) (*corev1.PersistentVolumeClaim, error) {
	sc := f.StorageClass
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: f.Namespace,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: &sc,
			AccessModes:      []corev1.PersistentVolumeAccessMode{accessMode},
			VolumeMode:       &mode,
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse(size),
				},
			},
		},
	}
	return f.KubeClient.CoreV1().PersistentVolumeClaims(f.Namespace).Create(
		context.Background(), pvc, metav1.CreateOptions{})
}

// WaitForPVCBound polls until the PVC reaches Bound phase or timeout.
func (f *Framework) WaitForPVCBound(name string, timeout time.Duration) error {
	return wait.PollImmediateWithContext(
		context.Background(), PollInterval, timeout,
		func(ctx context.Context) (bool, error) {
			pvc, err := f.KubeClient.CoreV1().PersistentVolumeClaims(f.Namespace).
				Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			Logf("PVC %s phase: %s", name, pvc.Status.Phase)
			return pvc.Status.Phase == corev1.ClaimBound, nil
		},
	)
}

// DeletePVC deletes the named PVC from the test namespace.
func (f *Framework) DeletePVC(name string) error {
	return f.KubeClient.CoreV1().PersistentVolumeClaims(f.Namespace).Delete(
		context.Background(), name, metav1.DeleteOptions{})
}

// WaitForPVCDeleted polls until the PVC is gone or timeout.
func (f *Framework) WaitForPVCDeleted(name string, timeout time.Duration) error {
	return wait.PollImmediateWithContext(
		context.Background(), PollInterval, timeout,
		func(ctx context.Context) (bool, error) {
			_, err := f.KubeClient.CoreV1().PersistentVolumeClaims(f.Namespace).
				Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return true, nil // gone
			}
			Logf("waiting for PVC %s to be deleted", name)
			return false, nil
		},
	)
}

// PVCVolumeID returns the volume ID stored in the PVC's spec.volumeName (PV name)
// and the PV's CSI volume handle, which is the niova volume UUID.
func (f *Framework) PVCVolumeID(pvcName string) (string, error) {
	ctx := context.Background()
	pvc, err := f.KubeClient.CoreV1().PersistentVolumeClaims(f.Namespace).
		Get(ctx, pvcName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	pvName := pvc.Spec.VolumeName
	if pvName == "" {
		return "", fmt.Errorf("PVC %s has no bound PV", pvcName)
	}
	pv, err := f.KubeClient.CoreV1().PersistentVolumes().Get(ctx, pvName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	if pv.Spec.CSI == nil {
		return "", fmt.Errorf("PV %s has no CSI spec", pvName)
	}
	Logf("PVC %s -> PV %s -> CSI volume_id %s",
		pvcName, pvName, pv.Spec.CSI.VolumeHandle)
	return pv.Spec.CSI.VolumeHandle, nil
}
