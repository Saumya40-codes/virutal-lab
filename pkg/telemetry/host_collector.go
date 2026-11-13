package telemetry

import (
	"errors"
	"os"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/procfs"
)

func NewFirecrackerProcessCollector(pidFn func() (int, error)) prometheus.Collector {
	return collectors.NewProcessCollector(collectors.ProcessCollectorOpts{
		Namespace: "lab_firecracker",
		PidFn:     pidFn,
		// An absent guest must not turn the entire scrape into an HTTP error.
		ReportErrors: false,
	})
}

// Only thread measurements need a custom collector. Process CPU, memory,
// descriptors and start time come from Prometheus's standard process collector.
type hostCollector struct {
	pidFn                       func() (int, error)
	threads, threadCPU, success *prometheus.Desc
}

func NewHostCollector(pidFn func() (int, error)) prometheus.Collector {
	return &hostCollector{
		pidFn:     pidFn,
		threads:   prometheus.NewDesc("lab_firecracker_threads", "Host threads in the Firecracker process.", nil, nil),
		threadCPU: prometheus.NewDesc("lab_firecracker_thread_cpu_seconds_total", "Host CPU time per Firecracker thread, including guest execution.", []string{"tid", "thread"}, nil),
		success:   prometheus.NewDesc("lab_firecracker_proc_read_success", "Whether the latest host thread inspection succeeded.", nil, nil),
	}
}

func (c *hostCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, desc := range []*prometheus.Desc{c.threads, c.threadCPU, c.success} {
		ch <- desc
	}
}

func (c *hostCollector) Collect(ch chan<- prometheus.Metric) {
	pid, err := c.pidFn()
	if err != nil {
		return
	}
	count, threads, err := readThreadStats("/proc", pid)
	if err != nil {
		ch <- prometheus.MustNewConstMetric(c.success, prometheus.GaugeValue, 0)
		return
	}
	ch <- prometheus.MustNewConstMetric(c.success, prometheus.GaugeValue, 1)
	ch <- prometheus.MustNewConstMetric(c.threads, prometheus.GaugeValue, float64(count))
	for _, thread := range threads {
		ch <- prometheus.MustNewConstMetric(c.threadCPU, prometheus.CounterValue, thread.CPUTime(), strconv.Itoa(thread.PID), thread.Comm)
	}
}

func readThreadStats(root string, pid int) (int, []procfs.ProcStat, error) {
	fs, err := procfs.NewFS(root)
	if err != nil {
		return 0, nil, err
	}
	process, err := fs.Proc(pid)
	if err != nil {
		return 0, nil, err
	}
	stat, err := process.Stat()
	if err != nil {
		return 0, nil, err
	}
	threads, err := fs.AllThreads(pid)
	if err != nil {
		return 0, nil, err
	}
	stats := make([]procfs.ProcStat, 0, len(threads))
	for _, thread := range threads {
		stat, err := thread.Stat()
		if errors.Is(err, os.ErrNotExist) {
			continue
		} // Threads may disappear between reads.
		if err != nil {
			return 0, nil, err
		}
		stats = append(stats, stat)
	}
	return stat.NumThreads, stats, nil
}
