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
