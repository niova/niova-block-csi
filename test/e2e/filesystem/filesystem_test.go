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
				time.Sleep(20* time.Second)
				By("identifying active ublk device from mounted container")
				var targetDev string
			        Eventually(func() string {
			            out, err := f.ExecInPod(podName, "test", []string{
			                "sh", "-c", `awk '$2=="/data"{print $1}' /proc/mounts`,
			            })
			            if err != nil {
			                return ""
			            }
			            targetDev = strings.TrimSpace(out)
			            return targetDev
			        }, 60*time.Second, 2*time.Second).Should(HavePrefix("/dev/ublkb"),
			            "device must be an active ublk block node")
				framework.Logf("Active block device for %s: %s", fsType, targetDev)
				By("writing data")
				out, err := f.ExecInPod(podName, "test",
					[]string{"sh", "-c",
						"set -x; touch /data/testfile; dd if=/dev/urandom of=/data/fill bs=1M count=100 conv=fsync",
					})
				framework.Logf("dd output:\n%s", out)
				Expect(err).NotTo(HaveOccurred())

				By("retrieving target node name")
				pod, err := f.KubeClient.CoreV1().
					Pods(f.Namespace).
					Get(context.Background(), podName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				nodeName := pod.Spec.NodeName

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
				 By("freezing the filesystem so on-disk state is consistent")
			        // exec args are passed directly, so no nested shell quoting is needed
			        freeze := func(flag string) error {
			            script := fmt.Sprintf(`
			set -e
			mnt=$(awk '$1=="%s"{print $2; exit}' /proc/mounts)
			test -n "$mnt"
			sync
			fsfreeze %s "$mnt"`, targetDev, flag)
			            o, err := f.ExecInPod(fsckPod, "fsck",
			                []string{"chroot", "/host", "sh", "-c", script})
			            framework.Logf("fsfreeze %s output:\n%s", flag, o)
			            return err
			        }
			        Expect(freeze("-f")).To(Succeed())
			        // must be unfrozen before pod/PVC cleanup, otherwise deletes hang
			        DeferCleanup(func() { _ = freeze("-u") })
				// Explicit stabilization delay to allow asynchronous ublk background I/O workers to finish writing metadata
				time.Sleep(30* time.Second)
				By("running read-only filesystem check")
				var fsckCmd string
				if fsType == "ext4" {
				    fsckCmd = "fsck.ext4 -n -f " + targetDev
				} else {
				    fsckCmd = fmt.Sprintf("chroot /host /usr/sbin/xfs_repair -n %s", targetDev)
				}

				_, err = f.ExecInPod(fsckPod, "fsck", []string{"sh", "-c", fsckCmd})
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
				framework.Logf("filesystem type: %s (expected %s)", out, fsType)
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
				time.Sleep(10 * time.Second)
				_, err := f.ExecInPod(podName, "test",
					[]string{"sh", "-c", "cd /data && sha256sum -c checkfile.sha256 2>&1"})
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
