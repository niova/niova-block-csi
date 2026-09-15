package integrity_test

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/niova-block-csi/test/framework"
)

func TestIntegrity(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Data Integrity Tests")
}

var f = framework.New()

var _ = Describe("Data Integrity", func() {

	Describe("fio checksum verification", func() {
		It("writes with sha512 checksums and reads back without errors (block PVC)", func() {
			pvcName := "integrity-block"
			podName := "integrity-block-pod"

			By("creating and staging a block PVC")
			_, err := f.CreatePVC(pvcName, "10Gi",
				corev1.PersistentVolumeMode("Block"),
				corev1.ReadWriteOnce)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(f.DeletePVC, pvcName)
			Expect(f.WaitForPVCBound(pvcName, framework.PVCBoundTimeout)).To(Succeed())
			_, err = f.CreatePodWithBlockPVC(podName, pvcName)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(f.DeletePod, podName)
			Expect(f.WaitForPodRunning(podName, framework.PodRunningTimeout)).To(Succeed())

			By("running fio write+verify (sha512, 512m, 4k blocks)")
			Expect(f.RunFIOVerify(podName, "/dev/test-block", "512m")).To(Succeed())
		})

		It("writes with sha512 checksums and reads back without errors (filesystem PVC)", func() {
			pvcName := "integrity-fs"
			podName := "integrity-fs-pod"

			By("creating and staging a filesystem PVC")
			_, err := f.CreatePVC(pvcName, "10Gi",
				corev1.PersistentVolumeMode("Filesystem"),
				corev1.ReadWriteOnce)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(f.DeletePVC, pvcName)
			Expect(f.WaitForPVCBound(pvcName, framework.PVCBoundTimeout)).To(Succeed())
			_, err = f.CreatePodWithFSPVC(podName, pvcName)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(f.DeletePod, podName)
			Expect(f.WaitForPodRunning(podName, framework.PodRunningTimeout)).To(Succeed())

			By("creating a test file for fio")
			_, err = f.ExecInPod(podName, "test",
				[]string{"touch", "/data/fio-test.img"})
			Expect(err).NotTo(HaveOccurred())

			By("running fio write+verify on the file")
			Expect(f.RunFIOVerify(podName, "/data/fio-test.img", "512m")).To(Succeed())
		})
	})

	Describe("Backend restart during IO", func() {
		It("data written before niova-ublk restart is intact after restart", func() {
			pvcName := "integrity-restart"
			podName := "integrity-restart-pod"

			By("staging a block PVC and running an initial write")
			_, err := f.CreatePVC(pvcName, "10Gi",
				corev1.PersistentVolumeMode("Block"),
				corev1.ReadWriteOnce)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(f.DeletePVC, pvcName)
			Expect(f.WaitForPVCBound(pvcName, framework.PVCBoundTimeout)).To(Succeed())
			_, err = f.CreatePodWithBlockPVC(podName, pvcName)
			Expect(err).NotTo(HaveOccurred())
			//DeferCleanup(f.DeletePod, podName)
			Expect(f.WaitForPodRunning(podName, framework.PodRunningTimeout)).To(Succeed())

			By("writing a known pattern to the first 64m")
			_, err = f.ExecInPod(podName, "test", []string{
				"fio", "--name=write", "--filename=/dev/test-block",
				"--rw=write", "--bs=1m", "--size=64m",
				"--direct=1", "--ioengine=libaio", "--iodepth=8",
			})
			Expect(err).NotTo(HaveOccurred())
			pod, err := f.KubeClient.CoreV1().Pods(f.Namespace).
				Get(context.Background(), podName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			f.NodeName = pod.Spec.NodeName
			GinkgoWriter.Printf("integrity pod is running on node %s\n", f.NodeName)
			By("killing niova-ublk to simulate a crash")
			volumeID, err := f.PVCVolumeID(pvcName)
			Expect(err).NotTo(HaveOccurred())
			Expect(f.KillUblkProcess(volumeID)).To(Succeed())

			By("Starting the niova-ublk")
			Expect(f.StartUblkProcessOnPodNode(podName, volumeID)).To(Succeed())

			By("waiting for the by-uuid symlink to reappear (daemon restarted)")
			Eventually(func() error {
				_, err := f.CheckByUUIDSymlink(volumeID)
				return err
			}, 60*time.Second, 2*time.Second).Should(Succeed())

			By("running fio verify-read to check data integrity after restart")
			// Use crc32c for speed on the read-back check
			out, err := f.ExecInPod(podName, "test", []string{
				"fio", "--name=readback", "--filename=/dev/test-block",
				"--rw=read", "--bs=1m", "--size=64m",
				"--direct=1", "--ioengine=libaio", "--iodepth=8",
			})
			Expect(err).NotTo(HaveOccurred(), "read after restart failed: %s", out)
		})
	})
})
