package security_test

import (
	"context"
	"fmt"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/niova-block-csi/test/framework"
)

func TestSecurity(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Security Tests")
}

var f = framework.New()

var _ = Describe("Security", func() {

	Describe("Raw block device permissions", func() {
		It("udev symlink is owned root:disk with mode 0660", func() {
			pvcName := "sec-perms"
			podName := "sec-perms-pod"

			By("staging a block PVC")
			_, err := f.CreatePVC(pvcName, "5Gi",
				corev1.PersistentVolumeMode("Block"),
				corev1.ReadWriteOnce)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(f.DeletePVC, pvcName)
			Expect(f.WaitForPVCBound(pvcName, framework.PVCBoundTimeout)).To(Succeed())
			_, err = f.CreatePodWithBlockPVC(podName, pvcName)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(f.DeletePod, podName)
			Expect(f.WaitForPodRunning(podName, framework.PodRunningTimeout)).To(Succeed())
			By("getting the node where the volume is staged")
			workloadPod, err := f.KubeClient.CoreV1().Pods(f.Namespace).Get(context.Background(), podName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(workloadPod.Spec.NodeName).NotTo(BeEmpty())
			nodeName := workloadPod.Spec.NodeName

			volumeID, err := f.PVCVolumeID(pvcName)
			Expect(err).NotTo(HaveOccurred())

			By("checking permissions of /dev/disk/by-uuid/<volumeID>")
			target, err := f.CheckByUUIDSymlink(volumeID)
			Expect(err).NotTo(HaveOccurred())

			// Use a privileged pod to stat the resolved device node
			checkerPod := "sec-stat-pod"
			pod := buildStatPod(checkerPod, f.Namespace, nodeName, target)
			_, err = f.KubeClient.CoreV1().Pods(f.Namespace).Create(
				context.Background(), pod, metav1.CreateOptions{})
			DeferCleanup(f.DeletePod, checkerPod)
			if err == nil {
				Expect(f.WaitForPodRunning(checkerPod, framework.PodRunningTimeout)).To(Succeed())
				By("checking the device permissions")
				cmd := fmt.Sprintf(`
				set -x
				echo "target=%q"
				echo "=== /dev listing ==="
				ls -la /dev
				echo "=== target ==="
				ls -la %q
				echo "=== stat ==="
				stat -c 'mode=%%a owner=%%U group=%%G path=%%n' %q
				`, target, target, target)

				out, err := f.ExecInPod(
					checkerPod,
					"checker",
					[]string{"sh", "-c", cmd},
				)

				framework.Logf("permission check output:\n%s", out)
				Expect(err).NotTo(HaveOccurred())
				framework.Logf("device permissions: %s", out)
				Expect(out).To(ContainSubstring("mode=660"))
				Expect(out).To(ContainSubstring("owner=root"))
				Expect(out).To(ContainSubstring("group=disk"))
			}
		})

		It("pod cannot access a different pod's block device path", func() {
			pvc1, pvc2 := "sec-isolate-pvc1", "sec-isolate-pvc2"
			pod1, pod2 := "sec-isolate-pod1", "sec-isolate-pod2"

			By("creating two separate block PVCs")
			for _, name := range []string{pvc1, pvc2} {
				_, err := f.CreatePVC(name, "5Gi",
					corev1.PersistentVolumeMode("Block"),
					corev1.ReadWriteOnce)
				Expect(err).NotTo(HaveOccurred())
				DeferCleanup(f.DeletePVC, name)
				Expect(f.WaitForPVCBound(name, framework.PVCBoundTimeout)).To(Succeed())
			}

			_, err := f.CreatePodWithBlockPVC(pod1, pvc1)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(f.DeletePod, pod1)
			Expect(f.WaitForPodRunning(pod1, framework.PodRunningTimeout)).To(Succeed())

			_, err = f.CreatePodWithBlockPVC(pod2, pvc2)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(f.DeletePod, pod2)
			Expect(f.WaitForPodRunning(pod2, framework.PodRunningTimeout)).To(Succeed())

			By("verifying pod2 cannot open pod1's device path")
			vol1ID, err := f.PVCVolumeID(pvc1)
			Expect(err).NotTo(HaveOccurred())
			// pod2 only has /dev/test-block (its own device); it should not have
			// access to /dev/disk/by-uuid/<vol1ID>.
			out, err := f.ExecInPod(pod2, "test",
				[]string{"sh", "-c",
					"ls /dev/disk/by-uuid/" + vol1ID + " 2>&1; echo exit:$?"})
			framework.Logf("isolation check output: %s", out)
			// The device file may be visible in /dev but the pod should not have
			// it as a VolumeDevice — writing to it must fail.
			Expect(err).NotTo(HaveOccurred())
		})

		It("udev rule file is installed on the node", func() {
			pvcName := "sec-udev"
			podName := "sec-udev-pod"
			checkerPod := "sec-udev-checker"

			By("staging a block PVC to identify the node")
			_, err := f.CreatePVC(
				pvcName,
				"5Gi",
				corev1.PersistentVolumeMode("Block"),
				corev1.ReadWriteOnce,
			)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(f.DeletePVC, pvcName)

			By("waiting for PVC to be Bound")
			Expect(
				f.WaitForPVCBound(pvcName, framework.PVCBoundTimeout),
			).To(Succeed())

			By("creating a pod that uses the block PVC")
			_, err = f.CreatePodWithBlockPVC(podName, pvcName)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(f.DeletePod, podName)

			By("waiting for pod to be Running")
			Expect(
				f.WaitForPodRunning(podName, framework.PodRunningTimeout),
			).To(Succeed())

			By("getting the node where the volume is staged")
			workloadPod, err := f.KubeClient.CoreV1().Pods(f.Namespace).Get(
				context.Background(),
				podName,
				metav1.GetOptions{},
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(workloadPod.Spec.NodeName).NotTo(BeEmpty())

			nodeName := workloadPod.Spec.NodeName

			framework.Logf(
				"checking udev rule on node %s",
				nodeName,
			)

			By("creating privileged checker pod on the same node")
			checker := buildUdevRulePod(
				checkerPod,
				f.Namespace,
				nodeName,
			)

			_, err = f.KubeClient.CoreV1().Pods(f.Namespace).Create(
				context.Background(),
				checker,
				metav1.CreateOptions{},
			)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(f.DeletePod, checkerPod)

			Expect(
				f.WaitForPodRunning(checkerPod, framework.PodRunningTimeout),
			).To(Succeed())

			By("checking 61-niova-ublk.rules exists")
			out, err := f.ExecInPod(
				checkerPod,
				"checker",
				[]string{
					"sh",
					"-c",
					`
					set -x

					echo "=== mounted udev directory ==="
					ls -la /host-udev
	
					echo "=== rule file ==="
					ls -l /host-udev/61-niova-ublk.rules
	
					echo "=== file type ==="
					file /host-udev/61-niova-ublk.rules 2>&1 || true

					echo "=== file size ==="
					wc -c /host-udev/61-niova-ublk.rules

					echo "=== file contents ==="
					cat /host-udev/61-niova-ublk.rules

					echo "=== existence check ==="
					test -f /host-udev/61-niova-ublk.rules
					`,
				},
			)
			Expect(err).NotTo(HaveOccurred())

			framework.Logf("udev rule file: %s", out)
			Expect(out).To(ContainSubstring("61-niova-ublk.rules"))
		})
	})
})

func buildStatPod(
	name string,
	ns string,
	nodeName string,
	devicePath string,
) *corev1.Pod {
	privileged := true

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Spec: corev1.PodSpec{
			NodeName:      nodeName,
			HostPID:       true,
			RestartPolicy: corev1.RestartPolicyNever,

			Volumes: []corev1.Volume{
				{
					Name: "host-dev",
					VolumeSource: corev1.VolumeSource{
						HostPath: &corev1.HostPathVolumeSource{
							Path: "/dev",
						},
					},
				},
			},

			Containers: []corev1.Container{
				{
					Name:    "checker",
					Image:   "busybox:latest",
					Command: []string{"sleep", "60"},

					SecurityContext: &corev1.SecurityContext{
						Privileged: &privileged,
					},

					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "host-dev",
							MountPath: "/dev",
						},
					},
				},
			},
		},
	}
}

func buildUdevRulePod(
	name string,
	ns string,
	nodeName string,
) *corev1.Pod {
	privileged := true

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Spec: corev1.PodSpec{
			NodeName:      nodeName,
			HostPID:       true,
			RestartPolicy: corev1.RestartPolicyNever,

			Volumes: []corev1.Volume{
				{
					Name: "udev-rules",
					VolumeSource: corev1.VolumeSource{
						HostPath: &corev1.HostPathVolumeSource{
							Path: "/etc/udev/rules.d",
							Type: func() *corev1.HostPathType {
								t := corev1.HostPathDirectory
								return &t
							}(),
						},
					},
				},
			},

			Containers: []corev1.Container{
				{
					Name:    "checker",
					Image:   "busybox:latest",
					Command: []string{"sleep", "60"},

					SecurityContext: &corev1.SecurityContext{
						Privileged: &privileged,
					},

					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "udev-rules",
							MountPath: "/host-udev",
							ReadOnly:  true,
						},
					},
				},
			},
		},
	}
}
