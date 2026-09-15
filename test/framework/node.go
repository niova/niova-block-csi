package framework

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

// CheckByUUIDSymlink verifies /dev/disk/by-uuid/<volumeID> exists on the node
// by running a privileged pod. Returns the symlink target (e.g. ../../ublkb0).
func (f *Framework) CheckByUUIDSymlink(volumeID string) (string, error) {
	podName := "check-uuid-" + volumeID[:8]
	privileged := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: f.Namespace},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			NodeName:      f.NodeName,
			Containers: []corev1.Container{
				{
					Name:            "checker",
					Image:           "busybox:latest",
					Command:         []string{"sleep", "60"},
					SecurityContext: &corev1.SecurityContext{Privileged: &privileged},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "dev", MountPath: "/dev"},
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "dev",
					VolumeSource: corev1.VolumeSource{
						HostPath: &corev1.HostPathVolumeSource{Path: "/dev"},
					},
				},
			},
		},
	}

	_, err := f.KubeClient.CoreV1().Pods(f.Namespace).Create(
		context.Background(), pod, metav1.CreateOptions{})
	if err != nil {
		return "", err
	}
	defer f.DeletePod(podName)

	if err := f.WaitForPodRunning(podName, 60*time.Second); err != nil {
		return "", fmt.Errorf("checker pod not running: %v", err)
	}

	out, err := f.ExecInPod(podName, "checker",
		[]string{"readlink", "-f", "/dev/disk/by-uuid/" + volumeID})
	if err != nil {
		return "", fmt.Errorf("by-uuid symlink not found for %s: %v", volumeID, err)
	}
	return strings.TrimSpace(out), nil
}

// RestartCSIDaemonSetPod deletes the CSI node pod on f.NodeName, triggering a
// restart. It waits until the replacement pod is Running before returning.
func (f *Framework) RestartCSIDaemonSetPod() error {
	ctx := context.Background()
	pods, err := f.KubeClient.CoreV1().Pods(DefaultCSINamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app=" + CSIDaemonSetName,
	})
	if err != nil || len(pods.Items) == 0 {
		return fmt.Errorf("no CSI pods found: %v", err)
	}

	var target *corev1.Pod
	for i := range pods.Items {
		if f.NodeName == "" || pods.Items[i].Spec.NodeName == f.NodeName {
			target = &pods.Items[i]
			break
		}
	}
	if target == nil {
		return fmt.Errorf("no CSI pod found on node %s", f.NodeName)
	}

	grace := int64(0)
	Logf("deleting CSI pod %s to trigger restart", target.Name)
	if err := f.KubeClient.CoreV1().Pods(DefaultCSINamespace).Delete(
		ctx, target.Name, metav1.DeleteOptions{GracePeriodSeconds: &grace}); err != nil {
		return err
	}

	// Wait for the replacement pod to become Running.
	return wait.PollImmediateWithContext(ctx, PollInterval, 2*time.Minute,
		func(ctx context.Context) (bool, error) {
			pods, err := f.KubeClient.CoreV1().Pods(DefaultCSINamespace).List(ctx, metav1.ListOptions{
				LabelSelector: "app=" + CSIDaemonSetName,
			})
			if err != nil {
				return false, nil
			}
			for _, p := range pods.Items {
				if (f.NodeName == "" || p.Spec.NodeName == f.NodeName) &&
					p.Name != target.Name &&
					p.Status.Phase == corev1.PodRunning {
					Logf("replacement CSI pod %s is Running", p.Name)
					return true, nil
				}
			}
			return false, nil
		},
	)
}

// KillUblkProcess sends SIGKILL to the niova-ublk process for a given volumeID
// on the node, simulating a daemon crash. It does this via a privileged pod
// that can see the host PID namespace.
func (f *Framework) KillUblkProcess(volumeID string) error {
	podName := "kill-ublk-" + volumeID[:8]
	privileged := true
	hostPID := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: f.Namespace,
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			NodeName:      f.NodeName,
			HostPID:       hostPID,

			Volumes: []corev1.Volume{
				{
					Name: "host-root",
					VolumeSource: corev1.VolumeSource{
						HostPath: &corev1.HostPathVolumeSource{
							Path: "/",
						},
					},
				},
			},

			Containers: []corev1.Container{
				{
					Name:    "killer",
					Image:   "busybox:latest",
					Command: []string{"sleep", "60"},

					SecurityContext: &corev1.SecurityContext{
						Privileged: &privileged,
					},

					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "host-root",
							MountPath: "/host",
						},
					},
				},
			},
		},
	}
	_, err := f.KubeClient.CoreV1().Pods(f.Namespace).Create(
		context.Background(), pod, metav1.CreateOptions{})
	if err != nil {
		return err
	}
	defer f.DeletePod(podName)

	if err := f.WaitForPodRunning(podName, 60*time.Second); err != nil {
		return err
	}

	// First show all niova-ublk processes visible from the host PID namespace.
	out, err := f.ExecInPod(
		podName,
		"killer",
		[]string{
			"sh",
			"-c",
			"echo '=== niova-ublk processes ==='; ps -eo pid,args | grep '[n]iova-ublk' || true",
		},
	)

	Logf("niova-ublk processes visible from killer pod:\n%s", out)

	if err != nil {
		return fmt.Errorf("failed to inspect niova-ublk processes: %w", err)
	}
	findCmd := fmt.Sprintf(`
	for proc in /host/proc/[0-9]*; do
	    if [ -f "$proc/cmdline" ]; then
	        cmd=$(tr '\000' ' ' < "$proc/cmdline")
	        case "$cmd" in
	            *niova-ublk*%s*)
	                echo "${proc#/host/proc/}"
	                exit 0
	                ;;
	        esac
	    fi
	done

	echo "No matching niova-ublk process found"
	echo "Host processes:"
	for proc in /host/proc/[0-9]*; do
	    if [ -f "$proc/cmdline" ]; then
	        cmd=$(tr '\000' ' ' < "$proc/cmdline")
	        case "$cmd" in
	            *niova-ublk*)
	                echo "${proc#/host/proc/}: $cmd"
	                ;;
	        esac
	    fi
	done

	exit 1
	`, volumeID)

	out, err = f.ExecInPod(
		podName,
		"killer",
		[]string{"sh", "-c", findCmd},
	)

	pid := strings.TrimSpace(out)
	if err != nil || pid == "" {
		return fmt.Errorf(
			"niova-ublk process for volume %s not found: %v; pgrep output: %q",
			volumeID,
			err,
			out,
		)
	}

	Logf("killing niova-ublk pid %s for volume %s on node %s", pid, volumeID, f.NodeName)
	// SIGKILL = 9.
	_, err = f.ExecInPod(
		podName,
		"killer",
		[]string{"kill", "-9", pid},
	)
	if err != nil {
		return fmt.Errorf(
			"failed to SIGKILL niova-ublk PID %s for volume %s: %w",
			pid,
			volumeID,
			err,
		)
	}
	time.Sleep(30 * time.Second)
	verifyCmd := fmt.Sprintf(`
	echo "=== checking original PID %s ==="

	if [ -d /host/proc/%s ]; then
	    echo "PID %s still exists"
	    if [ -f /host/proc/%s/cmdline ]; then
	        tr '\000' ' ' < /host/proc/%s/cmdline
	        echo
	    fi
	else
	    echo "PID %s is gone"
	fi

	echo "=== current niova-ublk processes ==="
	for proc in /host/proc/[0-9]*; do
	    if [ -f "$proc/cmdline" ]; then
	        cmd=$(tr '\000' ' ' < "$proc/cmdline")
	        case "$cmd" in
	            *niova-ublk*)
	                echo "${proc#/host/proc/}: $cmd"
	                ;;
	        esac
	    fi
	done
	`, pid, pid, pid, pid, pid, pid)

	verifyOut, _ := f.ExecInPod(
		podName,
		"killer",
		[]string{"sh", "-c", verifyCmd},
	)

	Logf("after SIGKILL:\n%s", verifyOut)
	Logf(
		"successfully killed niova-ublk PID %s for volume %s",
		pid,
		volumeID,
	)

	return nil
}

func (f *Framework) StartUblkProcessOnPodNode(podName, volumeID string) error {
	ctx := context.Background()

	// Find the node where the workload pod is running.
	pod, err := f.KubeClient.CoreV1().Pods(f.Namespace).
		Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	nodeName := pod.Spec.NodeName

	helperName := "ublk-helper-" + volumeID[:8]
	privileged := true
	Logf("NIOVA_BLOCK_UBLK_UNIFIED=%q", os.Getenv("NIOVA_BLOCK_UBLK_UNIFIED"))
	Logf("NIOVA_LOG_LEVEL=%q", os.Getenv("NIOVA_LOG_LEVEL"))
	Logf("NIOVA_BLOCK_MDSVC_GET_CHUNKS_LIMIT=%q", os.Getenv("NIOVA_BLOCK_MDSVC_GET_CHUNKS_LIMIT"))
	Logf("NIOVA_GOSSIP_PATH=%q", os.Getenv("NIOVA_GOSSIP_PATH"))
	Logf("NIOVA_GOSSIP_KEY=%q", os.Getenv("NIOVA_GOSSIP_KEY"))
	Logf("NIOVA_BLOCK_CP_AUTH_USERNAME=%q", os.Getenv("NIOVA_BLOCK_CP_AUTH_USERNAME"))
	Logf("NIOVA_BLOCK_CP_AUTH_SECRET=%q", os.Getenv("NIOVA_BLOCK_CP_AUTH_SECRET"))
	Logf("LD_LIBRARY_PATH=%q", os.Getenv("LD_LIBRARY_PATH"))

	helper := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      helperName,
			Namespace: f.Namespace,
		},
		Spec: corev1.PodSpec{
			NodeName:      nodeName,
			HostPID:       true,
			RestartPolicy: corev1.RestartPolicyNever,
			Volumes: []corev1.Volume{
				{
					Name: "host",
					VolumeSource: corev1.VolumeSource{
						HostPath: &corev1.HostPathVolumeSource{Path: "/"},
					},
				},
			},
			Containers: []corev1.Container{
				{
					Name:  "helper",
					Image: "ubuntu:22.04",
					Command: []string{
						"sleep", "3600",
					},
					SecurityContext: &corev1.SecurityContext{
						Privileged: &privileged,
					},
					Env: []corev1.EnvVar{
						{Name: "NIOVA_BLOCK_UBLK_UNIFIED", Value: os.Getenv("NIOVA_BLOCK_UBLK_UNIFIED")},
						{Name: "NIOVA_LOG_LEVEL", Value: os.Getenv("NIOVA_LOG_LEVEL")},
						{Name: "NIOVA_BLOCK_MDSVC_GET_CHUNKS_LIMIT", Value: os.Getenv("NIOVA_BLOCK_MDSVC_GET_CHUNKS_LIMIT")},
						{Name: "NIOVA_GOSSIP_PATH", Value: os.Getenv("NIOVA_GOSSIP_PATH")},
						{Name: "NIOVA_GOSSIP_KEY", Value: os.Getenv("NIOVA_GOSSIP_KEY")},
						{Name: "NIOVA_BLOCK_CP_AUTH_USERNAME", Value: os.Getenv("NIOVA_BLOCK_CP_AUTH_USERNAME")},
						{Name: "NIOVA_BLOCK_CP_AUTH_SECRET", Value: os.Getenv("NIOVA_BLOCK_CP_AUTH_SECRET")},
						{Name: "LD_LIBRARY_PATH", Value: os.Getenv("LD_LIBRARY_PATH")},
					},
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "host",
							MountPath: "/host",
						},
					},
				},
			},
		},
	}

	_, err = f.KubeClient.CoreV1().Pods(f.Namespace).
		Create(ctx, helper, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	defer f.DeletePod(helperName)

	if err := f.WaitForPodRunning(helperName, PodRunningTimeout); err != nil {
		return err
	}
	cmd := []string{
		"sh",
		"-c",
		fmt.Sprintf(`
	set -x

	echo "=== Helper pod environment ==="
	echo "NIOVA_BLOCK_UBLK_UNIFIED=$NIOVA_BLOCK_UBLK_UNIFIED"
	echo "NIOVA_LOG_LEVEL=$NIOVA_LOG_LEVEL"
	echo "NIOVA_BLOCK_MDSVC_GET_CHUNKS_LIMIT=$NIOVA_BLOCK_MDSVC_GET_CHUNKS_LIMIT"
	echo "NIOVA_GOSSIP_PATH=$NIOVA_GOSSIP_PATH"
	echo "NIOVA_GOSSIP_KEY=$NIOVA_GOSSIP_KEY"
	echo "NIOVA_BLOCK_CP_AUTH_USERNAME=$NIOVA_BLOCK_CP_AUTH_USERNAME"
	echo "NIOVA_BLOCK_CP_AUTH_SECRET=$NIOVA_BLOCK_CP_AUTH_SECRET"
	echo "LD_LIBRARY_PATH=$LD_LIBRARY_PATH"

	chroot /host /usr/bin/env \
	  NIOVA_BLOCK_UBLK_UNIFIED="$NIOVA_BLOCK_UBLK_UNIFIED" \
	  NIOVA_LOG_LEVEL="$NIOVA_LOG_LEVEL" \
	  NIOVA_BLOCK_MDSVC_GET_CHUNKS_LIMIT="$NIOVA_BLOCK_MDSVC_GET_CHUNKS_LIMIT" \
	  NIOVA_GOSSIP_PATH="$NIOVA_GOSSIP_PATH" \
	  NIOVA_GOSSIP_KEY="$NIOVA_GOSSIP_KEY" \
	  NIOVA_BLOCK_CP_AUTH_USERNAME="$NIOVA_BLOCK_CP_AUTH_USERNAME" \
	  NIOVA_BLOCK_CP_AUTH_SECRET="$NIOVA_BLOCK_CP_AUTH_SECRET" \
	  LD_LIBRARY_PATH="$LD_LIBRARY_PATH" \
	  systemd-run \
	    --unit=niova-ublk-%s \
	    --setenv=NIOVA_BLOCK_UBLK_UNIFIED="$NIOVA_BLOCK_UBLK_UNIFIED" \
	    --setenv=NIOVA_LOG_LEVEL="$NIOVA_LOG_LEVEL" \
	    --setenv=NIOVA_BLOCK_MDSVC_GET_CHUNKS_LIMIT="$NIOVA_BLOCK_MDSVC_GET_CHUNKS_LIMIT" \
	    --setenv=NIOVA_GOSSIP_PATH="$NIOVA_GOSSIP_PATH" \
	    --setenv=NIOVA_GOSSIP_KEY="$NIOVA_GOSSIP_KEY" \
	    --setenv=NIOVA_BLOCK_CP_AUTH_USERNAME="$NIOVA_BLOCK_CP_AUTH_USERNAME" \
	    --setenv=NIOVA_BLOCK_CP_AUTH_SECRET="$NIOVA_BLOCK_CP_AUTH_SECRET" \
	    --setenv=LD_LIBRARY_PATH="$LD_LIBRARY_PATH" \
	    /usr/local/bin/niova-ublk \
	    -t cp \
	    -v %s \
	    -q 128 \
	    -b 1048576 \
	    -T \
	    -r

	echo "=== systemctl status ==="
	chroot /host systemctl status niova-ublk-%s --no-pager || true

	echo "=== journal ==="
	chroot /host journalctl -u niova-ublk-%s --no-pager -n 50 || true
	`,
			volumeID,
			volumeID,
			volumeID,
			volumeID,
		),
	}
	out, err := f.ExecInPod(helperName, "helper", cmd)
	Logf("systemd-run output:\n%s", out)
	if err != nil {
		return fmt.Errorf("failed to start niova-ublk on node %s: %w", nodeName, err)
	}

	return nil
}
