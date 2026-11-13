// Package telemetry instruments the controller and exports Firecracker metrics
// through the official Prometheus Go client and promhttp handler.
package telemetry

import (
	"bytes"
	"fmt"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
)

type Metrics struct {
	VMStarts               prometheus.Counter
	VMExits                prometheus.Counter
	VMFailedExits          prometheus.Counter
	VMRunning              prometheus.Gauge
	FirecrackerPID         prometheus.Gauge
	MetricsSamples         prometheus.Counter
	MetricsParseErrors     prometheus.Counter
	MetricsLastTimestamp   prometheus.Gauge
	VMStartDuration        prometheus.Histogram
	APIRequests            *prometheus.CounterVec
	APIErrors              *prometheus.CounterVec
	APIRequestDuration     *prometheus.HistogramVec
	MetricsScrapeDuration  *prometheus.HistogramVec
	MetricsScrapeRequests  *prometheus.CounterVec
	MetricsScrapesInFlight prometheus.Gauge
	VCPUIOInExits          prometheus.Counter
	VCPUIOOutExits         prometheus.Counter
	VCPUMMIOReadExits      prometheus.Counter
	VCPUMMIOWriteExits     prometheus.Counter
	VCPUFailures           prometheus.Counter
	BlockReadBytes         prometheus.Counter
	BlockWriteBytes        prometheus.Counter
	BlockReads             prometheus.Counter
	BlockWrites            prometheus.Counter
	NetRXBytes             prometheus.Counter
	NetTXBytes             prometheus.Counter
}

// NewMetrics constructs collectors without registering them or starting services.
func NewMetrics() *Metrics {
	return &Metrics{
		VMStarts: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "lab_vm_starts_total",
			Help: "Firecracker processes launched by this controller.",
		}),
		VMExits: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "lab_vm_exits_total",
			Help: "Firecracker processes that exited.",
		}),
		VMFailedExits: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "lab_vm_failed_exits_total",
			Help: "Firecracker processes with unsuccessful exits.",
		}),
		VMRunning: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "lab_vm_running",
			Help: "InstanceStart accepted and Firecracker has not exited; not guest health.",
		}),
		FirecrackerPID: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "lab_firecracker_pid",
			Help: "Firecracker process identifier for this launch.",
		}),
		MetricsSamples: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "lab_firecracker_metrics_samples_total",
			Help: "Complete Firecracker JSON records consumed exactly once.",
		}),
		MetricsParseErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "lab_firecracker_metrics_parse_errors_total",
			Help: "Malformed or oversized Firecracker metrics records skipped.",
		}),
		MetricsLastTimestamp: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "lab_firecracker_metrics_last_timestamp_seconds",
			Help: "Timestamp of the latest Firecracker record; zero until first sample.",
		}),
		VMStartDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:                           "lab_vm_start_duration_seconds",
			Help:                           "Time from process launch until InstanceStart acceptance, not guest readiness.",
			NativeHistogramBucketFactor:    1.1,
			NativeHistogramMaxBucketNumber: 100,
		}),
		APIRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "lab_firecracker_api_requests_total",
			Help: "Requests made by this controller process, excluding other CLI processes.",
		}, []string{"method", "path", "status"}),
		APIErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "lab_firecracker_api_errors_total",
			Help: "Failed HTTP or transport requests. Status 0 denotes a transport error.",
		}, []string{"method", "path", "status"}),
		APIRequestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:                           "lab_firecracker_api_request_duration_seconds",
			Help:                           "Firecracker API request duration, including failed requests.",
			NativeHistogramBucketFactor:    1.1,
			NativeHistogramMaxBucketNumber: 100,
		}, []string{"method", "path"}),
		MetricsScrapeDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:                           "lab_metrics_http_request_duration_seconds",
			Help:                           "Time spent serving metrics scrapes.",
			NativeHistogramBucketFactor:    1.1,
			NativeHistogramMaxBucketNumber: 100,
		}, []string{"code", "method"}),
		MetricsScrapeRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "promhttp_metric_handler_requests_total",
			Help: "Total number of scrapes by HTTP status code.",
		}, []string{"code"}),
		MetricsScrapesInFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "promhttp_metric_handler_requests_in_flight",
			Help: "Current number of scrapes being served.",
		}),
		VCPUIOInExits: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "lab_firecracker_vcpu_io_in_exits_total",
			Help: "Firecracker IO input exits accumulated from flush deltas.",
		}),
		VCPUIOOutExits: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "lab_firecracker_vcpu_io_out_exits_total",
			Help: "Firecracker IO output exits accumulated from flush deltas.",
		}),
		VCPUMMIOReadExits: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "lab_firecracker_vcpu_mmio_read_exits_total",
			Help: "Firecracker MMIO read exits accumulated from flush deltas.",
		}),
		VCPUMMIOWriteExits: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "lab_firecracker_vcpu_mmio_write_exits_total",
			Help: "Firecracker MMIO write exits accumulated from flush deltas.",
		}),
		VCPUFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "lab_firecracker_vcpu_failures_total",
			Help: "Firecracker vCPU failures accumulated from flush deltas.",
		}),
		BlockReadBytes: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "lab_firecracker_block_read_bytes_total",
			Help: "Block read bytes accumulated from aggregate Firecracker flush deltas.",
		}),
		BlockWriteBytes: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "lab_firecracker_block_write_bytes_total",
			Help: "Block write bytes accumulated from aggregate Firecracker flush deltas.",
		}),
		BlockReads: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "lab_firecracker_block_reads_total",
			Help: "Block reads accumulated from aggregate Firecracker flush deltas.",
		}),
		BlockWrites: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "lab_firecracker_block_writes_total",
			Help: "Block writes accumulated from aggregate Firecracker flush deltas.",
		}),
		NetRXBytes: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "lab_firecracker_net_rx_bytes_total",
			Help: "Network receive bytes accumulated from aggregate Firecracker flush deltas.",
		}),
		NetTXBytes: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "lab_firecracker_net_tx_bytes_total",
			Help: "Network transmit bytes accumulated from aggregate Firecracker flush deltas.",
		}),
	}
}

// RegisterMetrics accepts a registry or a registerer wrapped with VM labels.
// If registration fails, collectors added by this call are unregistered.
func (m *Metrics) RegisterMetrics(reg prometheus.Registerer) error {
	metrics := []prometheus.Collector{
		m.VMStarts,
		m.VMExits,
		m.VMFailedExits,
		m.VMRunning,
		m.FirecrackerPID,
		m.MetricsSamples,
		m.MetricsParseErrors,
		m.MetricsLastTimestamp,
		m.VMStartDuration,
		m.APIRequests,
		m.APIErrors,
		m.APIRequestDuration,
		m.MetricsScrapeDuration,
		m.MetricsScrapeRequests,
		m.MetricsScrapesInFlight,
		m.VCPUIOInExits,
		m.VCPUIOOutExits,
		m.VCPUMMIOReadExits,
		m.VCPUMMIOWriteExits,
		m.VCPUFailures,
		m.BlockReadBytes,
		m.BlockWriteBytes,
		m.BlockReads,
		m.BlockWrites,
		m.NetRXBytes,
		m.NetTXBytes,
	}
	registered := make([]prometheus.Collector, 0, len(metrics))
	for _, metric := range metrics {
		if err := reg.Register(metric); err != nil {
			for _, added := range registered {
				reg.Unregister(added)
			}
			return fmt.Errorf("register metrics: %w", err)
		}
		registered = append(registered, metric)
	}
	return nil
}

func (m *Metrics) ProcessStarted(pid int) {
	m.FirecrackerPID.Set(float64(pid))
	m.VMStarts.Inc()
	m.MetricsLastTimestamp.Set(0)
}

func (m *Metrics) BootAccepted(elapsed time.Duration) {
	m.VMRunning.Set(1)
	m.VMStartDuration.Observe(elapsed.Seconds())
}

func (m *Metrics) ProcessExited(failed bool) {
	m.FirecrackerPID.Set(0)
	m.VMRunning.Set(0)
	m.VMExits.Inc()
	if failed {
		m.VMFailedExits.Inc()
	}
}

func (m *Metrics) RecordAPI(method, path string, status int, duration time.Duration, err error) {
	labels := []string{method, path, strconv.Itoa(status)}
	m.APIRequests.WithLabelValues(labels...).Inc()
	if err != nil {
		m.APIErrors.WithLabelValues(labels...).Inc()
	}
	m.APIRequestDuration.WithLabelValues(method, path).Observe(duration.Seconds())
}

// Snapshot encodes a registry for the final on-disk exposition.
func Snapshot(reg prometheus.Gatherer, format expfmt.Format) ([]byte, error) {
	families, err := reg.Gather()
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	encoder := expfmt.NewEncoder(&b, format)
	for _, family := range families {
		if err := encoder.Encode(family); err != nil {
			return nil, err
		}
	}
	return b.Bytes(), nil
}
