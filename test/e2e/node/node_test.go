package node_test

import (
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"

	"github.com/niova-block-csi/test/framework"
)

func TestNode(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Node-Level Tests")
}

var f = framework.New()

var _ = Describe("Node-Level", func() {

	Describe("Device naming stability (udev by-uuid)", func() {
		It("creates /dev/disk/by-uuid/<volumeID> symlink after staging", func() {
			pvcName := "node-byuuid"
			podName := "node-byuuid-pod"

			By("creating and binding a block PVC")
			_, err := f.CreatePVC(pvcName, "5Gi",
				corev1.PersistentVolumeMode("Block"),
				corev1.ReadWriteOnce)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(f.DeletePVC, pvcName)
			Expect(f.WaitForPVCBound(pvcName, framework.PVCBoundTimeout)).To(Succeed())

			By("scheduling a pod to trigger NodeStageVolume")
			_, err = f.CreatePodWithBlockPVC(podName, pvcName)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(f.DeletePod, podName)
			Expect(f.WaitForPodRunning(podName, framework.PodRunningTimeout)).To(Succeed())

			By("retrieving the CSI volume handle")
			volumeID, err := f.PVCVolumeID(pvcName)
			Expect(err).NotTo(HaveOccurred())

			By("verifying /dev/disk/by-uuid/<volumeID> exists on the node")
			target, err := f.CheckByUUIDSymlink(volumeID)
			Expect(err).NotTo(HaveOccurred())
			Expect(target).To(ContainSubstring("ublkb"),
				"by-uuid symlink should resolve to an ublkb device")
		})

		It("by-uuid symlink resolves correctly after niova-ublk restart", func() {
			pvcName := "node-byuuid-restart"
			podName := "node-byuuid-restart-pod"

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

			volumeID, err := f.PVCVolumeID(pvcName)
			Expect(err).NotTo(HaveOccurred())

			By("recording the initial symlink target")
			target1, err := f.CheckByUUIDSymlink(volumeID)
			Expect(err).NotTo(HaveOccurred())

			By("killing niova-ublk to simulate a daemon crash")
			Expect(f.KillUblkProcess(volumeID)).To(Succeed())

			By("Starting the niova-ublk")
                        Expect(f.StartUblkProcessOnPodNode(podName, volumeID)).To(Succeed())

			By("waiting for the by-uuid symlink to reappear")
			var target2 string
			Eventually(func() error {
				target2, err = f.CheckByUUIDSymlink(volumeID)
				return err
			}, 60*time.Second, 2*time.Second).Should(Succeed())

			By("verifying the symlink still points to an ublkb device (may have new N)")
			Expect(target2).To(ContainSubstring("ublkb"))
			framework.Logf("symlink: %s -> %s (was %s)", volumeID, target2, target1)
		})
	})

	Describe("CSI plugin restart handling", func() {
		It("PVC remains accessible after the CSI node pod restarts", func() {
			pvcName := "node-csirestart"
			podName := "node-csirestart-pod"

			By("staging a filesystem PVC")
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

			By("writing a marker file before restart")
			_, err = f.ExecInPod(podName, "test",
				[]string{"sh", "-c", "echo before-restart > /data/marker"})
			Expect(err).NotTo(HaveOccurred())

			By("restarting the CSI DaemonSet pod")
			Expect(f.RestartCSIDaemonSetPod()).To(Succeed())

			By("verifying the workload pod and data are unaffected")
			out, err := f.ExecInPod(podName, "test",
				[]string{"cat", "/data/marker"})
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(ContainSubstring("before-restart"))
		})
	})

	Describe("Kubelet restart recovery", func() {
		// This test is marked Pending because it requires root on the node to
		// restart kubelet. Enable by removing the P prefix when running on a
		// dedicated test node where kubelet restart is safe.
		PIt("VolumeAttachment survives kubelet restart", func() {
			// Implementation: shell out to node via privileged pod,
			// systemctl restart kubelet, wait 30s, verify PVC still Bound.
		})
	})
})
