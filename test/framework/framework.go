package framework

import (
	"context"
	"fmt"
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	DefaultNamespace    = "niova-csi-test"
	DefaultStorageClass = "niova-csi-sc"
	DefaultFIOImage     = "ljishen/fio:latest"
	DefaultCSINamespace = "default"
	CSIDriverName       = "csi.niova.com"
	CSIDaemonSetName    = "niova-csi-node"

	PollInterval      = 2 * time.Second
	PVCBoundTimeout   = 2 * time.Minute
	PodRunningTimeout = 3 * time.Minute
	PodDeleteTimeout  = 2 * time.Minute
)

// Framework holds state shared across tests in a suite.
type Framework struct {
	KubeClient   kubernetes.Interface
	Namespace    string
	StorageClass string
	FIOImage     string
	NodeName     string // target node for node-level tests
}

// New builds a Framework from environment variables and registers
// namespace create/delete around the test suite via BeforeSuite/AfterSuite.
func New() *Framework {
	f := &Framework{
		Namespace:    envOr("E2E_NAMESPACE", DefaultNamespace),
		StorageClass: envOr("E2E_STORAGE_CLASS", DefaultStorageClass),
		FIOImage:     envOr("E2E_FIO_IMAGE", DefaultFIOImage),
		NodeName:     os.Getenv("E2E_NODE_NAME"),
	}

	BeforeSuite(func() {
		kubeconfig := envOr("E2E_KUBECONFIG", os.Getenv("KUBECONFIG"))
		cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
		Expect(err).NotTo(HaveOccurred(), "building kubeconfig from %s", kubeconfig)

		f.KubeClient, err = kubernetes.NewForConfig(cfg)
		Expect(err).NotTo(HaveOccurred())

		f.ensureNamespace()
	})

	AfterSuite(func() {
		if f.KubeClient == nil {
			return
		}
		if f != nil && f.Namespace != "" {
			err := f.CleanupTestNamespace(f.Namespace)
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to clean up namespace %s after test", f.Namespace))
		}

		ctx := context.Background()
		_ = f.KubeClient.CoreV1().Namespaces().Delete(ctx, f.Namespace, metav1.DeleteOptions{})

		// Synchronously wait until the namespace is completely removed from the cluster
		_ = wait.PollImmediate(PollInterval, 1*time.Minute, func() (bool, error) {
			_, err := f.KubeClient.CoreV1().Namespaces().Get(ctx, f.Namespace, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			return false, nil
		})
	})

	return f
}

func (f *Framework) ensureNamespace() {
	ctx := context.Background()
	existingNs, err := f.KubeClient.CoreV1().Namespaces().Get(ctx, f.Namespace, metav1.GetOptions{})
	if err == nil {
		if existingNs.Status.Phase == corev1.NamespaceTerminating {
			Logf("Namespace %s is terminating; waiting for completion...", f.Namespace)
			err = wait.PollImmediate(PollInterval, 1*time.Minute, func() (bool, error) {
				_, getErr := f.KubeClient.CoreV1().Namespaces().Get(ctx, f.Namespace, metav1.GetOptions{})
				if apierrors.IsNotFound(getErr) {
					return true, nil
				}
				return false, nil
			})
			Expect(err).NotTo(HaveOccurred(), "timed out waiting for old namespace %s to terminate", f.Namespace)
		} else {
			// Namespace exists and is active
			return
		}
	}

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: f.Namespace}}
	_, err = f.KubeClient.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred(), "creating test namespace %s", f.Namespace)
}

// CleanupTestNamespace deletes PVCs -> PVs -> Pods -> Namespace in strict order with patch-retries if stuck for >5s.
func (f *Framework) CleanupTestNamespace(nsName string) error {
	ctx := context.Background()
	patchPayload := []byte(`{"metadata":{"finalizers":null}}`)
	grace := int64(0)

	Logf("[CLEANUP] Starting strict ordered teardown for namespace: %s", nsName)

	// -------------------------------------------------------------
	// STEP 1: Patch, Delete & Verify PVCs
	// -------------------------------------------------------------
	pvcs, err := f.KubeClient.CoreV1().PersistentVolumeClaims(nsName).List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, pvc := range pvcs.Items {
			Logf("[CLEANUP] [PVC] Processing %s/%s", nsName, pvc.Name)

			err := wait.PollImmediateWithContext(ctx, 1*time.Second, 30*time.Second, func(ctx context.Context) (bool, error) {
				// Patch PVC finalizers
				_, _ = f.KubeClient.CoreV1().PersistentVolumeClaims(nsName).Patch(
					ctx, pvc.Name, types.MergePatchType, patchPayload, metav1.PatchOptions{})
				
				// Delete PVC
				_ = f.KubeClient.CoreV1().PersistentVolumeClaims(nsName).Delete(
					ctx, pvc.Name, metav1.DeleteOptions{GracePeriodSeconds: &grace})

				// Wait up to 5 seconds to verify if deleted
				pvcStuck := true
				_ = wait.PollImmediateWithContext(ctx, 500*time.Millisecond, 5*time.Second, func(ctx context.Context) (bool, error) {
					_, getErr := f.KubeClient.CoreV1().PersistentVolumeClaims(nsName).Get(ctx, pvc.Name, metav1.GetOptions{})
					if apierrors.IsNotFound(getErr) {
						pvcStuck = false
						return true, nil
					}
					return false, nil
				})

				if !pvcStuck {
					Logf("[CLEANUP] [PVC] Successfully deleted %s/%s", nsName, pvc.Name)
					return true, nil
				}

				Logf("[CLEANUP] [PVC] %s/%s stuck >5s, retrying patch & delete...", nsName, pvc.Name)
				return false, nil
			})
			if err != nil {
				Logf("[CLEANUP] [PVC] Warning: Failed to clear PVC %s: %v", pvc.Name, err)
			}
		}
	}

	// -------------------------------------------------------------
	// STEP 2: Patch, Delete & Verify PVs (Executed after PVCs are gone)
	// -------------------------------------------------------------
	pvs, err := f.KubeClient.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, pv := range pvs.Items {
			if pv.Spec.ClaimRef != nil && pv.Spec.ClaimRef.Namespace == nsName {
				Logf("[CLEANUP] [PV] Processing %s", pv.Name)

				err := wait.PollImmediateWithContext(ctx, 1*time.Second, 30*time.Second, func(ctx context.Context) (bool, error) {
					// Patch PV finalizers
					_, _ = f.KubeClient.CoreV1().PersistentVolumes().Patch(
						ctx, pv.Name, types.MergePatchType, patchPayload, metav1.PatchOptions{})

					// Delete PV
					_ = f.KubeClient.CoreV1().PersistentVolumes().Delete(
						ctx, pv.Name, metav1.DeleteOptions{GracePeriodSeconds: &grace})

					// Wait up to 5 seconds to verify if deleted
					pvStuck := true
					_ = wait.PollImmediateWithContext(ctx, 500*time.Millisecond, 5*time.Second, func(ctx context.Context) (bool, error) {
						_, getErr := f.KubeClient.CoreV1().PersistentVolumes().Get(ctx, pv.Name, metav1.GetOptions{})
						if apierrors.IsNotFound(getErr) {
							pvStuck = false
							return true, nil
						}
						return false, nil
					})

					if !pvStuck {
						Logf("[CLEANUP] [PV] Successfully deleted %s", pv.Name)
						return true, nil
					}

					Logf("[CLEANUP] [PV] %s stuck >5s, retrying patch & delete...", pv.Name)
					return false, nil
				})
				if err != nil {
					Logf("[CLEANUP] [PV] Warning: Failed to clear PV %s: %v", pv.Name, err)
				}
			}
		}
	}

	// -------------------------------------------------------------
	// STEP 3: Force Delete & Verify Pods (Executed after PVs/PVCs are clear)
	// -------------------------------------------------------------
	pods, err := f.KubeClient.CoreV1().Pods(nsName).List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, pod := range pods.Items {
			Logf("[CLEANUP] [POD] Processing force delete for %s/%s", nsName, pod.Name)

			err := wait.PollImmediateWithContext(ctx, 1*time.Second, 30*time.Second, func(ctx context.Context) (bool, error) {
				// Force delete Pod
				_ = f.KubeClient.CoreV1().Pods(nsName).Delete(
					ctx, pod.Name, metav1.DeleteOptions{GracePeriodSeconds: &grace})

				// Wait up to 5 seconds to verify if deleted
				podStuck := true
				_ = wait.PollImmediateWithContext(ctx, 500*time.Millisecond, 5*time.Second, func(ctx context.Context) (bool, error) {
					_, getErr := f.KubeClient.CoreV1().Pods(nsName).Get(ctx, pod.Name, metav1.GetOptions{})
					if apierrors.IsNotFound(getErr) {
						podStuck = false
						return true, nil
					}
					return false, nil
				})

				if !podStuck {
					Logf("[CLEANUP] [POD] Successfully deleted %s/%s", nsName, pod.Name)
					return true, nil
				}

				Logf("[CLEANUP] [POD] %s/%s stuck >5s, retrying force delete...", nsName, pod.Name)
				return false, nil
			})
			if err != nil {
				Logf("[CLEANUP] [POD] Warning: Failed to clear Pod %s: %v", pod.Name, err)
			}
		}
	}

	// -------------------------------------------------------------
	// STEP 4: Delete Namespace (Only after PVC, PV, and Pods are confirmed empty)
	// -------------------------------------------------------------
	Logf("[CLEANUP] [NS] Deleting namespace %s", nsName)
	_ = f.KubeClient.CoreV1().Namespaces().Delete(ctx, nsName, metav1.DeleteOptions{GracePeriodSeconds: &grace})

	return wait.PollImmediateWithContext(ctx, 2*time.Second, 30*time.Second, func(ctx context.Context) (bool, error) {
		ns, err := f.KubeClient.CoreV1().Namespaces().Get(ctx, nsName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			Logf("[CLEANUP] [NS] Namespace %s completely purged.", nsName)
			return true, nil
		}

		if err == nil && ns.Status.Phase == corev1.NamespaceTerminating {
			Logf("[CLEANUP] [NS] Namespace %s stuck in Terminating; clearing spec finalizers...", nsName)
			nsPatchPayload := []byte(`{"spec":{"finalizers":[]}}`)
			_, _ = f.KubeClient.CoreV1().Namespaces().Patch(
				ctx, nsName, types.MergePatchType, nsPatchPayload, metav1.PatchOptions{}, "finalize")
		}
		return false, nil
	})
}

// Logf emits a GinkgoWriter log line prefixed with a timestamp.
func Logf(format string, args ...interface{}) {
	fmt.Fprintf(GinkgoWriter, "[%s] "+format+"\n",
		append([]interface{}{time.Now().Format("15:04:05")}, args...)...)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
