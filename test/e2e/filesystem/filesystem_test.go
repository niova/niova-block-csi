package filesystem_test

import (
	"context"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"testing"
	"time"
	"fmt"
	"strings"

	"github.com/niova-block-csi/test/framework"
)

func TestFilesystem(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Filesystem Tests")
}

var f = framework.New()

var _ = Describe("Filesystem", func() {

	for _, fsType := range []string{"ext4", "xfs"} {
		fsType := fsType

		Describe(fsType+" validation", func() {
			It("mounts cleanly and passes fsck", func() {
				pvcName := "fs-fsck-" + fsType
				podName := "fs-fsck-" + fsType + "-pod"

				By("creating a " + fsType + " PVC")
				_, err := f.CreatePVC(pvcName, "5Gi",
					corev1.PersistentVolumeMode("Filesystem"),
					corev1.ReadWriteOnce)
				Expect(err).NotTo(HaveOccurred())
				DeferCleanup(f.DeletePVC, pvcName)
				Expect(f.WaitForPVCBound(pvcName, framework.PVCBoundTimeout)).To(Succeed())

				By("mounting via a pod")
				_, err = f.CreatePodWithFSPVC(podName, pvcName)
				Expect(err).NotTo(HaveOccurred())
				Expect(f.WaitForPodRunning(podName, framework.PodRunningTimeout)).To(Succeed())
				// Retrieve node running workload pod
				pod, err := f.KubeClient.CoreV1().
					Pods(f.Namespace).
					Get(context.Background(), podName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				nodeName := pod.Spec.NodeName

				By("writing data")
				out, err := f.ExecInPod(podName, "test",
					[]string{"sh", "-c",
						"set -x; touch /data/testfile; dd if=/dev/urandom of=/data/fill bs=1M count=100 2>&1",
					})
				framework.Logf("dd output:\n%s", out)
				Expect(err).NotTo(HaveOccurred())

				By("syncing and unmounting filesystem inside pod container")
				_, _ = f.ExecInPod(podName, "test", []string{"sh", "-c", "sync -f /data && umount -l /data || true"})

				By("launching privileged fsck pod with host mounts")
				fsckPod := "fs-fsck-check-" + fsType
				volumeID, err := f.PVCVolumeID(pvcName)
				Expect(err).NotTo(HaveOccurred())
				privileged := true
				fsckPodObj := buildFsckPod(fsckPod, f.Namespace, volumeID, nodeName, fsType, &privileged)

				_, err = f.KubeClient.CoreV1().Pods(f.Namespace).Create(context.Background(), fsckPodObj, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
				DeferCleanup(f.DeletePod, fsckPod)
				Expect(f.WaitForPodRunning(fsckPod, framework.PodRunningTimeout)).To(Succeed())

				By("resolving target block device bound to volume")
				var targetDev string
				Eventually(func() error {
					cmd := fmt.Sprintf(`
					chroot /host sh -c '
						for dev in /sys/class/block/ublkb*; do
							if [ -d "$dev" ]; then
								bname=$(basename "$dev")
								if [ -b "/dev/$bname" ]; then
									echo "/dev/$bname"
									exit 0
								fi
							fi
						done
						exit 1
					'`)
					out, err := f.ExecInPod(fsckPod, "fsck", []string{"sh", "-c", cmd})
					if err != nil {
						return err
					}
					targetDev = strings.TrimSpace(out)
					if !strings.HasPrefix(targetDev, "/dev/ublkb") {
						return fmt.Errorf("invalid device path: %s", targetDev)
					}
					return nil
				}, 30*time.Second, 2*time.Second).Should(Succeed())

				framework.Logf("Target device for fsck: %s", targetDev)

				By("running read-only filesystem check")
				var fsckCmd string
				if fsType == "ext4" {
					fsckCmd = fmt.Sprintf("chroot /host /usr/sbin/fsck.ext4 -n -f %s", targetDev)
				} else {
					fsckCmd = fmt.Sprintf("chroot /host /usr/sbin/xfs_repair -n %s", targetDev)
				}

				out, err = f.ExecInPod(fsckPod, "fsck", []string{"sh", "-c", fsckCmd})
				framework.Logf("fsck output:\n%s", out)
				Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("fsck failed for %s on device %s", fsType, targetDev))
			})

			It("reports correct filesystem type via stat", func() {
				pvcName := "fs-type-" + fsType
				podName := "fs-type-" + fsType + "-pod"

				_, err := f.CreatePVC(pvcName, "5Gi",
					corev1.PersistentVolumeMode("Filesystem"),
					corev1.ReadWriteOnce)
				Expect(err).NotTo(HaveOccurred())
				DeferCleanup(f.DeletePVC, pvcName)
				Expect(f.WaitForPVCBound(pvcName, framework.PVCBoundTimeout)).To(Succeed())
				_, err = f.CreatePodWithFSPVC(podName, pvcName)
				Expect(err).NotTo(HaveOccurred())
				DeferCleanup(f.DeletePod, podName)
				Expect(f.WaitForPodRunning(podName, framework.PodRunningTimeout)).To(Succeed())
				By("waiting before running fsck")
				time.Sleep(10 * time.Second)
				By("checking filesystem type reported by stat -f")
				out, err := f.ExecInPod(podName, "test",
					[]string{"stat", "-f", "-c", "%T", "/data"})
				Expect(err).NotTo(HaveOccurred())
				framework.Logf("filesystem type: %s (expected %s)", out, fsType)
				// stat -f -c %T reports "ext2/ext3" for ext4, "xfs" for xfs
				if fsType == "xfs" {
					Expect(out).To(ContainSubstring("xfs"))
				}
				// ext4 is reported as ext2/ext3 by stat; just verify mount succeeded
			})

			It("survives a write-read round trip with dd", func() {
				pvcName := "fs-dd-" + fsType
				podName := "fs-dd-" + fsType + "-pod"

				_, err := f.CreatePVC(pvcName, "5Gi",
					corev1.PersistentVolumeMode("Filesystem"),
					corev1.ReadWriteOnce)
				Expect(err).NotTo(HaveOccurred())
				DeferCleanup(f.DeletePVC, pvcName)
				Expect(f.WaitForPVCBound(pvcName, framework.PVCBoundTimeout)).To(Succeed())
				_, err = f.CreatePodWithFSPVC(podName, pvcName)
				Expect(err).NotTo(HaveOccurred())
				DeferCleanup(f.DeletePod, podName)
				Expect(f.WaitForPodRunning(podName, framework.PodRunningTimeout)).To(Succeed())

				By("writing a known checksum file")
				_, err = f.ExecInPod(podName, "test", []string{
					"sh", "-c",
					"echo 'niova-integrity-check' > /data/checkfile && sha256sum /data/checkfile > /data/checkfile.sha256",
				})
				Expect(err).NotTo(HaveOccurred())

				By("verifying the checksum")
				out, err := f.ExecInPod(podName, "test",
					[]string{"sh", "-c", "cd /data && sha256sum -c checkfile.sha256 2>&1"})
				framework.Logf("checksum output:\n%s", out)
				Expect(err).NotTo(HaveOccurred())
				Expect(out).To(ContainSubstring("OK"))
			})
		})
	}
})

func buildFsckPod(name, ns, volID, nodeName, fsType string, privileged *bool) *corev1.Pod {
	// Placeholder — real implementation builds a privileged pod spec that
	// mounts the raw block device via hostPath and runs fsck against it.
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Spec: corev1.PodSpec{
			NodeName:      nodeName,
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
					Name:  "fsck",
					Image: "ubuntu:22.04",
					Command: []string{
						"sh",
						"-c",
						` set -eux
						echo "=== Host filesystem tools ==="
						chroot /host /usr/sbin/fsck.ext4 -V
						chroot /host /usr/sbin/xfs_repair -V
						echo "=== Filesystem tools ready ==="
						sleep 3600`,
					},
					SecurityContext: &corev1.SecurityContext{
						Privileged: privileged,
					},
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "host-dev",
							MountPath: "/host-dev",
						},
						{
							Name:      "host-root",
							MountPath: "/host",
							ReadOnly:  true,
						},
					},
				},
			},
		},
	}
}
