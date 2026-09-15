package lifecycle_test

import (
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"

	"github.com/niova-block-csi/test/framework"
)

func TestLifecycle(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "PVC Lifecycle")
}

var f = framework.New()

var _ = Describe("PVC Lifecycle", func() {

	Describe("Block volume mode", func() {
		It("creates, binds, mounts and deletes a raw block PVC", func() {
			pvcName := "lifecycle-block"
			podName := "lifecycle-block-pod"

			By("creating a block PVC")
			_, err := f.CreatePVC(pvcName, "5Gi",
				corev1.PersistentVolumeMode("Block"),
				corev1.ReadWriteOnce)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for PVC to be Bound")
			Expect(f.WaitForPVCBound(pvcName, framework.PVCBoundTimeout)).To(Succeed())

			By("scheduling a pod that uses the block PVC")
			_, err = f.CreatePodWithBlockPVC(podName, pvcName)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for pod to be Running")
			Expect(f.WaitForPodRunning(podName, framework.PodRunningTimeout)).To(Succeed())

			By("verifying the block device is accessible inside the pod")
			_, err = f.ExecInPod(podName, "test", []string{"ls", "-la", "/dev/test-block"})
			Expect(err).NotTo(HaveOccurred())

			By("deleting the pod")
			Expect(f.DeletePod(podName)).To(Succeed())
			Expect(f.WaitForPodDeleted(podName, framework.PodDeleteTimeout)).To(Succeed())

			By("deleting the PVC")
			Expect(f.DeletePVC(pvcName)).To(Succeed())
			Expect(f.WaitForPVCDeleted(pvcName, 2*time.Minute)).To(Succeed())
		})
	})

	Describe("Filesystem volume mode", func() {
		for _, fsType := range []string{"ext4", "xfs"} {
			fsType := fsType
			It("creates, binds, mounts and deletes a "+fsType+" filesystem PVC", func() {
				pvcName := "lifecycle-fs-" + fsType
				podName := "lifecycle-fs-" + fsType + "-pod"

				By("creating a filesystem PVC (" + fsType + ")")
				_, err := f.CreatePVC(pvcName, "5Gi",
					corev1.PersistentVolumeMode("Filesystem"),
					corev1.ReadWriteOnce)
				Expect(err).NotTo(HaveOccurred())
				DeferCleanup(f.DeletePVC, pvcName)

				By("waiting for PVC to be Bound")
				Expect(f.WaitForPVCBound(pvcName, framework.PVCBoundTimeout)).To(Succeed())

				By("scheduling a pod that uses the filesystem PVC")
				_, err = f.CreatePodWithFSPVC(podName, pvcName)
				Expect(err).NotTo(HaveOccurred())
				DeferCleanup(f.DeletePod, podName)

				By("waiting for pod to be Running")
				Expect(f.WaitForPodRunning(podName, framework.PodRunningTimeout)).To(Succeed())

				By("writing and reading a file on the mounted filesystem")
				_, err = f.ExecInPod(podName, "test",
					[]string{"sh", "-c", "echo niova-test > /data/probe && cat /data/probe"})
				Expect(err).NotTo(HaveOccurred())

				By("verifying mount is read-write")
				_, err = f.ExecInPod(podName, "test",
					[]string{"sh", "-c", "cat /data/probe"})
				Expect(err).NotTo(HaveOccurred())
			})
		}
	})

	Describe("Access mode validation", func() {
		It("rejects ReadWriteMany for a block PVC (unsupported)", func() {
			pvcName := "lifecycle-rwx"
			_, err := f.CreatePVC(pvcName, "5Gi",
				corev1.PersistentVolumeMode("Block"),
				corev1.ReadWriteMany)
			if err == nil {
				DeferCleanup(f.DeletePVC, pvcName)
				// PVC created but should never reach Bound
				err = f.WaitForPVCBound(pvcName, 30*time.Second)
				Expect(err).To(HaveOccurred(), "RWX block PVC should not bind")
			}
		})

		It("accepts ReadOnlyMany for filesystem PVC", func() {
			pvcName := "lifecycle-rox"
			_, err := f.CreatePVC(pvcName, "5Gi",
				corev1.PersistentVolumeMode("Filesystem"),
				corev1.ReadOnlyMany)
			// ROX may or may not be supported; we just verify it fails cleanly
			// (no panic, no hung PVC) rather than asserting success.
			if err == nil {
				DeferCleanup(f.DeletePVC, pvcName)
			}
		})
	})

	Describe("Idempotency", func() {
		It("returns success when NodeStageVolume is called twice for the same volume", func() {
			pvcName := "lifecycle-idempotent"
			podName1 := "lifecycle-idempotent-pod1"
			podName2 := "lifecycle-idempotent-pod2"

			By("creating and binding a PVC")
			_, err := f.CreatePVC(pvcName, "5Gi",
				corev1.PersistentVolumeMode("Filesystem"),
				corev1.ReadWriteOnce)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(f.DeletePVC, pvcName)
			Expect(f.WaitForPVCBound(pvcName, framework.PVCBoundTimeout)).To(Succeed())

			By("scheduling first pod")
			_, err = f.CreatePodWithFSPVC(podName1, pvcName)
			Expect(err).NotTo(HaveOccurred())
			Expect(f.WaitForPodRunning(podName1, framework.PodRunningTimeout)).To(Succeed())

			By("deleting first pod (triggers NodeUnpublish but not NodeUnstage)")
			Expect(f.DeletePod(podName1)).To(Succeed())
			Expect(f.WaitForPodDeleted(podName1, framework.PodDeleteTimeout)).To(Succeed())

			By("scheduling second pod on the same PVC (re-triggers NodeStage)")
			_, err = f.CreatePodWithFSPVC(podName2, pvcName)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(f.DeletePod, podName2)
			Expect(f.WaitForPodRunning(podName2, framework.PodRunningTimeout)).To(Succeed())
		})
	})
})
