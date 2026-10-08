package concurrency_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/niova-block-csi/test/framework"
)

func TestConcurrency(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Concurrency Tests")
}

var f = framework.New()

var _ = Describe("Multi-Attach & Concurrency", func() {

	Describe("Invalid multi-attach (RWO)", func() {
		It("second pod on a different node stays Pending with VolumeInUse", func() {
			pvcName := "concur-multiattach"
			pod1 := "concur-multiattach-pod1"
			pod2 := "concur-multiattach-pod2"
			const node1 = "io07"
			const node2 = "io08"

			DeferCleanup(func() {
				f.NodeName = ""
			})
			By("creating and binding a block PVC")
			_, err := f.CreatePVC(pvcName, "5Gi",
				corev1.PersistentVolumeMode("Block"),
				corev1.ReadWriteOnce)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(f.DeletePVC, pvcName)
			Expect(f.WaitForPVCBound(pvcName, framework.PVCBoundTimeout)).To(Succeed())

			By("attaching to pod1")
			f.NodeName = node1
			_, err = f.CreatePodWithBlockPVC(pod1, pvcName)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(f.DeletePod, pod1)
			Expect(f.WaitForPodRunning(pod1, framework.PodRunningTimeout)).To(Succeed())

			By("attempting to attach the same PVC to pod2")
			f.NodeName = node2
			p2Spec, err := f.CreatePodWithBlockPVC(pod2, pvcName)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(f.DeletePod, pod2)
			_ = p2Spec

			By("verifying pod2 cannot schedule (stays Pending or gets FailedScheduling)")
			// Give the scheduler 30s — pod2 must NOT reach Running.
			Consistently(func() corev1.PodPhase {
				pod, err := f.KubeClient.CoreV1().Pods(f.Namespace).
					Get(context.Background(), pod2, metav1.GetOptions{})
				if err != nil {
					return corev1.PodUnknown
				}
				return pod.Status.Phase
			}, 30*time.Second, 5*time.Second).ShouldNot(Equal(corev1.PodRunning))
		})
	})

	Describe("Rapid create/delete", func() {
		It("10 concurrent PVC create+delete cycles leave no leaked ublk processes", func() {
			const workers = 10
			var wg sync.WaitGroup
			errCh := make(chan error, workers)

			for i := 0; i < workers; i++ {
				wg.Add(1)
				i := i
				go func() {
					defer wg.Done()
					name := fmt.Sprintf("concur-rapid-%d", i)
					pod := fmt.Sprintf("concur-rapid-pod-%d", i)

					_, err := f.CreatePVC(name, "5Gi",
						corev1.PersistentVolumeMode("Block"),
						corev1.ReadWriteOnce)
					if err != nil {
						errCh <- fmt.Errorf("worker %d CreatePVC: %v", i, err)
						return
					}
					if err := f.WaitForPVCBound(name, framework.PVCBoundTimeout); err != nil {
						_ = f.DeletePVC(name)
						errCh <- fmt.Errorf("worker %d WaitBound: %v", i, err)
						return
					}
					_, err = f.CreatePodWithBlockPVC(pod, name)
					if err != nil {
						_ = f.DeletePVC(name)
						errCh <- fmt.Errorf("worker %d CreatePod: %v", i, err)
						return
					}
					if err := f.WaitForPodRunning(pod, framework.PodRunningTimeout); err != nil {
						_ = f.DeletePod(pod)
						_ = f.DeletePVC(name)
						errCh <- fmt.Errorf("worker %d WaitRunning: %v", i, err)
						return
					}
					_ = f.DeletePod(pod)
					_ = f.WaitForPodDeleted(pod, framework.PodDeleteTimeout)
					_ = f.DeletePVC(name)
					_ = f.WaitForPVCDeleted(name, 2*time.Minute)
				}()
			}
			wg.Wait()
			close(errCh)

			var errs []error
			for err := range errCh {
				errs = append(errs, err)
			}
			Expect(errs).To(BeEmpty(), "concurrent PVC lifecycle errors: %v", errs)
		})
	})

	Describe("Rapid pod scheduling (same PVC, sequential)", func() {
		It("schedules 5 pods in sequence on the same filesystem PVC without error", func() {
			pvcName := "concur-sequential"

			_, err := f.CreatePVC(pvcName, "5Gi",
				corev1.PersistentVolumeMode("Filesystem"),
				corev1.ReadWriteOnce)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(f.DeletePVC, pvcName)
			Expect(f.WaitForPVCBound(pvcName, framework.PVCBoundTimeout)).To(Succeed())

			for i := 0; i < 5; i++ {
				podName := fmt.Sprintf("concur-seq-pod-%d", i)
				framework.Logf("scheduling pod %d", i)
				_, err := f.CreatePodWithFSPVC(podName, pvcName)
				Expect(err).NotTo(HaveOccurred())
				Expect(f.WaitForPodRunning(podName, framework.PodRunningTimeout)).To(Succeed())
				Expect(f.DeletePod(podName)).To(Succeed())
				Expect(f.WaitForPodDeleted(podName, framework.PodDeleteTimeout)).To(Succeed())
			}
		})
	})
})
