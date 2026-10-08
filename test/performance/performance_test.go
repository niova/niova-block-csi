package performance_test

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"

	"github.com/niova-block-csi/test/framework"
)

func TestPerformance(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Performance Tests")
}

var f = framework.New()

// thresholds holds minimum acceptable performance values.
// Override by setting PERF_THRESHOLDS_FILE to a JSON file path.
type Thresholds struct {
	SeqReadBWMiBs    float64 `json:"seq_read_bw_mibs"`
	SeqWriteBWMiBs   float64 `json:"seq_write_bw_mibs"`
	RandReadIOPS     float64 `json:"rand_read_iops"`
	RandWriteIOPS    float64 `json:"rand_write_iops"`
	RandReadP99UsLat float64 `json:"rand_read_p99_us_lat"`
}

var defaultThresholds = Thresholds{
	SeqReadBWMiBs:    200, // MiB/s
	SeqWriteBWMiBs:   150,
	RandReadIOPS:     5000,
	RandWriteIOPS:    3000,
	RandReadP99UsLat: 5000, // 5ms
}

func loadThresholds() Thresholds {
	path := os.Getenv("PERF_THRESHOLDS_FILE")
	if path == "" {
		return defaultThresholds
	}
	data, err := os.ReadFile(path)
	if err != nil {
		framework.Logf("cannot read thresholds file %s: %v; using defaults", path, err)
		return defaultThresholds
	}
	t := defaultThresholds
	if err := json.Unmarshal(data, &t); err != nil {
		framework.Logf("cannot parse thresholds filei %s: %v; using defaults", path, err)
		return defaultThresholds
	}
	framework.Logf("performance thresholds: seq_read=%.0f MiB/s, seq_write=%.0f MiB/s, "+"rand_read=%.0f IOPS, rand_write=%.0f IOPS, rand_read_p99=%.0f us",
		t.SeqReadBWMiBs,
		t.SeqWriteBWMiBs,
		t.RandReadIOPS,
		t.RandWriteIOPS,
		t.RandReadP99UsLat,
	)
	return t
}

var (
	pvcName = "perf-block"
	podName = "perf-block-pod"
)

var _ = Describe("Performance", Ordered, Label("performance"), func() {
	var (
		thresh Thresholds
	)
	BeforeAll(func() {
		By("creating and staging a 64Gi block PVC for benchmarks")

		_, err := f.CreatePVC(
			pvcName,
			"64Gi",
			corev1.PersistentVolumeMode("Block"),
			corev1.ReadWriteOnce,
		)
		Expect(err).NotTo(HaveOccurred())

		Expect(
			f.WaitForPVCBound(
				pvcName,
				framework.PVCBoundTimeout,
			),
		).To(Succeed())

		_, err = f.CreatePodWithBlockPVC(
			podName,
			pvcName,
		)
		Expect(err).NotTo(HaveOccurred())

		Expect(
			f.WaitForPodRunning(
				podName,
				framework.PodRunningTimeout,
			),
		).To(Succeed())
	})

	AfterAll(func() {
		By("cleaning up performance benchmark pod")

		if err := f.DeletePod(podName); err != nil {
			framework.Logf(
				"failed to delete performance pod %s: %v",
				podName,
				err,
			)
		}

		if err := f.WaitForPodDeleted(
			podName,
			framework.PodDeleteTimeout,
		); err != nil {
			framework.Logf(
				"performance pod %s was not deleted cleanly: %v",
				podName,
				err,
			)
		}

		By("cleaning up performance benchmark PVC")

		if err := f.DeletePVC(pvcName); err != nil {
			framework.Logf(
				"failed to delete performance PVC %s: %v",
				pvcName,
				err,
			)
		}

		if err := f.WaitForPVCDeleted(
			pvcName,
			2*time.Minute,
		); err != nil {
			framework.Logf(
				"performance PVC %s was not deleted cleanly: %v",
				pvcName,
				err,
			)
		}
	})

	BeforeEach(func() {
		thresh = loadThresholds()
	})

	Describe("Sequential I/O", func() {
		It("sequential read meets bandwidth threshold", func() {
			By("running sequential read benchmark")
			result, err := f.RunFIOBenchmark(podName, "/dev/test-block",
				"read", "1m", "32Gi", 64)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Jobs).NotTo(BeEmpty())

			bwMiBs := result.Jobs[0].Read.BW / 1024
			framework.Logf("sequential read: %.0f MiB/s (threshold: %.0f)", bwMiBs, thresh.SeqReadBWMiBs)
			publishMetric("seq_read_bw_mibs", bwMiBs)
			Expect(bwMiBs).To(BeNumerically(">=", thresh.SeqReadBWMiBs),
				"sequential read BW below threshold")
		})

		It("sequential write meets bandwidth threshold", func() {
			result, err := f.RunFIOBenchmark(podName, "/dev/test-block",
				"write", "1m", "32Gi", 64)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Jobs).NotTo(BeEmpty())

			bwMiBs := result.Jobs[0].Write.BW / 1024
			framework.Logf("sequential write: %.0f MiB/s (threshold: %.0f)", bwMiBs, thresh.SeqWriteBWMiBs)
			publishMetric("seq_write_bw_mibs", bwMiBs)
			Expect(bwMiBs).To(BeNumerically(">=", thresh.SeqWriteBWMiBs))
		})
	})

	Describe("Random I/O", func() {
		It("random read meets IOPS threshold", func() {
			result, err := f.RunFIOBenchmark(podName, "/dev/test-block",
				"randread", "4k", "32Gi", 128)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Jobs).NotTo(BeEmpty())

			iops := result.Jobs[0].Read.IOPS
			p99us := float64(0)
			if value, ok := result.Jobs[0].Read.LatNs.Percentile["99.000000"]; ok {
				p99us = value / 1000
			}
			framework.Logf("random read: %.0f IOPS, p99=%.0fµs (thresholds: %.0f IOPS, %.0fµs)",
				iops, p99us, thresh.RandReadIOPS, thresh.RandReadP99UsLat)
			publishMetric("rand_read_iops", iops)
			publishMetric("rand_read_p99_us", p99us)
			Expect(iops).To(BeNumerically(">=", thresh.RandReadIOPS))
			Expect(p99us).To(BeNumerically("<=", thresh.RandReadP99UsLat))
		})

		It("random write meets IOPS threshold", func() {
			result, err := f.RunFIOBenchmark(podName, "/dev/test-block",
				"randwrite", "4k", "2Gi", 128)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Jobs).NotTo(BeEmpty())

			iops := result.Jobs[0].Write.IOPS
			framework.Logf("random write: %.0f IOPS (threshold: %.0f)", iops, thresh.RandWriteIOPS)
			publishMetric("rand_write_iops", iops)
			Expect(iops).To(BeNumerically(">=", thresh.RandWriteIOPS))
		})
	})

	Describe("Scaling", func() {
		It("maintains acceptable latency with 10 concurrent PVCs", func() {
			const n = 10
			const maxConcurrent = 3

			type benchmarkResult struct {
				idx  int
				iops float64
				err  error
			}

			results := make(chan benchmarkResult, n)
			sem := make(chan struct{}, maxConcurrent)

			pvcs := make([]string, n)
			pods := make([]string, n)

			By("creating 10 block PVCs and pods")

			for i := 0; i < n; i++ {
				pvcs[i] = fmt.Sprintf("perf-scale-%d", i)
				pods[i] = fmt.Sprintf("perf-scale-pod-%d", i)

				_, err := f.CreatePVC(
					pvcs[i],
					"10Gi",
					corev1.PersistentVolumeMode("Block"),
					corev1.ReadWriteOnce,
				)
				Expect(err).NotTo(HaveOccurred())

				DeferCleanup(f.DeletePVC, pvcs[i])

				Expect(
					f.WaitForPVCBound(
						pvcs[i],
						framework.PVCBoundTimeout,
					),
				).To(Succeed())

				_, err = f.CreatePodWithBlockPVC(
					pods[i],
					pvcs[i],
				)
				Expect(err).NotTo(HaveOccurred())

				DeferCleanup(f.DeletePod, pods[i])

				Expect(
					f.WaitForPodRunning(
						pods[i],
						framework.PodRunningTimeout,
					),
				).To(Succeed())
			}

			By("running fio benchmarks with limited concurrency")

			var wg sync.WaitGroup
			wg.Add(n)

			for i := 0; i < n; i++ {
				i := i

				go func() {
					defer wg.Done()

					// Allow only maxConcurrent fio executions at once.
					sem <- struct{}{}
					defer func() {
						<-sem
					}()

					framework.Logf(
						"starting scaling benchmark %d on pod %s",
						i,
						pods[i],
					)

					r, err := f.RunFIOBenchmark(
						pods[i],
						"/dev/test-block",
						"randread",
						"4k",
						"1Gi",
						32,
					)

					if err != nil {
						results <- benchmarkResult{
							idx: i,
							err: err,
						}
						return
					}

					if len(r.Jobs) == 0 {
						results <- benchmarkResult{
							idx: i,
							err: fmt.Errorf("fio returned no jobs"),
						}
						return
					}

					iops := r.Jobs[0].Read.IOPS

					framework.Logf(
						"scaling benchmark %d: %.0f IOPS",
						i,
						iops,
					)

					results <- benchmarkResult{
						idx:  i,
						iops: iops,
					}
				}()
			}

			wg.Wait()
			close(results)

			var totalIOPS float64
			completed := 0

			for r := range results {
				Expect(r.err).NotTo(
					HaveOccurred(),
					"scaling benchmark %d failed",
					r.idx,
				)

				totalIOPS += r.iops
				completed++
			}

			Expect(completed).To(
				Equal(n),
				"all 10 scaling benchmarks should complete",
			)

			framework.Logf(
				"scaling test: %.0f aggregate IOPS across %d PVCs",
				totalIOPS,
				n,
			)

			publishMetric(
				"scale_aggregate_iops",
				totalIOPS,
			)

			Expect(totalIOPS).To(
				BeNumerically(">", 0),
				"aggregate IOPS should be greater than zero",
			)
		})
	})
})

// publishMetric writes a metric line to the pipeline artifact file if
// PERF_RESULTS_FILE is set (CI collects this for trend graphs).
func publishMetric(name string, value float64) {
	path := os.Getenv("PERF_RESULTS_FILE")
	if path == "" {
		return
	}
	file, err := os.OpenFile(
		path,
		os.O_APPEND|os.O_CREATE|os.O_WRONLY,
		0644,
	)
	if err != nil {
		framework.Logf(
			"cannot open performance results file %s: %v",
			path,
			err,
		)
		return
	}
	defer file.Close()
	if _, err := fmt.Fprintf(
		file,
		"%s=%.2f\n",
		name,
		value,
	); err != nil {
		framework.Logf(
			"cannot write performance metric %s: %v",
			name,
			err,
		)
	}
}
