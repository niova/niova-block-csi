package framework

import (
	"bytes"
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
	"os"
)

// CreatePodWithBlockPVC creates a privileged pod that mounts a raw block PVC
// at /dev/test-block. Used for direct device I/O tests.
func (f *Framework) CreatePodWithBlockPVC(name, pvcName string) (*corev1.Pod, error) {
	privileged := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.Namespace},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{
				{
					Name:    "test",
					Image:   f.FIOImage,
					Command: []string{"sleep", "3600"},
					SecurityContext: &corev1.SecurityContext{
						Privileged: &privileged,
					},
					VolumeDevices: []corev1.VolumeDevice{
						{Name: "vol", DevicePath: "/dev/test-block"},
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "vol",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: pvcName,
						},
					},
				},
			},
		},
	}
	if f.NodeName != "" {
		pod.Spec.NodeName = f.NodeName
	}
	return f.KubeClient.CoreV1().Pods(f.Namespace).Create(
		context.Background(), pod, metav1.CreateOptions{})
}

// CreatePodWithFSPVC creates a pod that mounts a filesystem PVC at /data.
func (f *Framework) CreatePodWithFSPVC(name, pvcName string) (*corev1.Pod, error) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.Namespace},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{
				{
					Name:    "test",
					Image:   f.FIOImage,
					Command: []string{"sleep", "3600"},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "vol", MountPath: "/data"},
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "vol",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: pvcName,
						},
					},
				},
			},
		},
	}
	if f.NodeName != "" {
		pod.Spec.NodeName = f.NodeName
	}
	return f.KubeClient.CoreV1().Pods(f.Namespace).Create(
		context.Background(), pod, metav1.CreateOptions{})
}

// WaitForPodRunning polls until the pod is Running or timeout.
func (f *Framework) WaitForPodRunning(name string, timeout time.Duration) error {
	return wait.PollImmediateWithContext(
		context.Background(), PollInterval, timeout,
		func(ctx context.Context) (bool, error) {
			pod, err := f.KubeClient.CoreV1().Pods(f.Namespace).
				Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			Logf("pod %s phase: %s", name, pod.Status.Phase)
			if pod.Status.Phase == corev1.PodFailed {
				return false, fmt.Errorf("pod %s entered Failed phase", name)
			}
			return pod.Status.Phase == corev1.PodRunning, nil
		},
	)
}

// DeletePod deletes the named pod from the test namespace.
func (f *Framework) DeletePod(name string) error {
	grace := int64(0)
	return f.KubeClient.CoreV1().Pods(f.Namespace).Delete(
		context.Background(), name,
		metav1.DeleteOptions{GracePeriodSeconds: &grace})
}

// WaitForPodDeleted polls until the pod is gone or timeout.
func (f *Framework) WaitForPodDeleted(name string, timeout time.Duration) error {
	return wait.PollImmediateWithContext(
		context.Background(), PollInterval, timeout,
		func(ctx context.Context) (bool, error) {
			_, err := f.KubeClient.CoreV1().Pods(f.Namespace).
				Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return true, nil
			}
			Logf("waiting for pod %s to be deleted", name)
			return false, nil
		},
	)
}

// ExecInPod runs a command inside a running pod and returns combined output.
func (f *Framework) ExecInPod(podName, containerName string, cmd []string) (string, error) {
	kubeconfig := envOr("E2E_KUBECONFIG", os.Getenv("KUBECONFIG"))
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return "", err
	}

	req := f.KubeClient.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(f.Namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: containerName,
			Command:   cmd,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(cfg, "POST", req.URL())
	if err != nil {
		return "", err
	}

	var out bytes.Buffer
	err = exec.StreamWithContext(context.Background(), remotecommand.StreamOptions{
		Stdout: &out,
		Stderr: &out,
	})
	return out.String(), err
}
