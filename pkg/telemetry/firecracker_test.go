package telemetry

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFIFOStreamsAndDrainsOnExit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.fifo")
	metrics, registry := newTestMetrics(t, "test-vm")
	var raw bytes.Buffer
	fifo, err := OpenFIFO(path, metrics, &raw)
	if err != nil {
		t.Fatal(err)
	}
	defer fifo.Finish()
	info, err := os.Stat(path)
	if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("expected FIFO: %v %v", info, err)
	}
	writer, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.WriteString(`{"utc_timestamp_ms":1000,"vcpu":{"exit_io_out":3}}` + "\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.WriteString(`{"utc_timestamp_ms":2000,"vcpu":{"exit_io_out":2}}` + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	// Finish must consume pending records before final counters are scraped.
	if err := fifo.Finish(); err != nil {
		t.Fatal(err)
	}
	assertMetric(t, registry, "lab_firecracker_metrics_samples_total", 2)
	assertMetric(t, registry, "lab_firecracker_vcpu_io_out_exits_total", 5)
	if bytes.Count(raw.Bytes(), []byte{'\n'}) != 2 {
		t.Fatalf("raw capture lost records: %q", raw.Bytes())
	}
}

func TestFIFOShutdownWithoutVMMWriter(t *testing.T) {
	metrics := NewMetrics()
	fifo, err := OpenFIFO(filepath.Join(t.TempDir(), "metrics.fifo"), metrics, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- fifo.Finish() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("FIFO cleanup blocked without a VMM writer")
	}
	if err := fifo.Finish(); err != nil {
		t.Fatal(err)
	} // Idempotent cleanup.
}

func TestDecoderDeltasAndPartialWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.jsonl")
	writer, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	reader, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	m, registry := newTestMetrics(t, "test-vm")
	tail := Decoder{Reader: reader, Metrics: m}
	appendData := func(data string) {
		t.Helper()
		if _, err := writer.WriteString(data); err != nil {
			t.Fatal(err)
		}
		if err := tail.Read(); err != nil {
			t.Fatal(err)
		}
	}
	appendData(`{"utc_timestamp_ms":1000,"vcpu":{"exit_io_out":3},"block":{"read_bytes":64},"net":{"tx_bytes_count":8},"block_rootfs":{"read_bytes":64},"api_server":{"process_startup_time_us":123}}` + "\n")
	assertMetric(t, registry, "lab_firecracker_vcpu_io_out_exits_total", 3)
	assertMetric(t, registry, "lab_firecracker_block_read_bytes_total", 64) // No aggregate/per-device double count.
	// Repeated reads at EOF cannot replay a delta.
	for i := 0; i < 3; i++ {
		if err := tail.Read(); err != nil {
			t.Fatal(err)
		}

	}
	assertMetric(t, registry, "lab_firecracker_metrics_samples_total", 1)
	appendData(`{"utc_timestamp_ms":2000,"vcpu":{"exit_io_out":2}`)
	assertMetric(t, registry, "lab_firecracker_metrics_samples_total", 1)
	appendData(`,"net":{"tx_bytes_count":4}}` + "\nnot-json\n{}\n")
	assertMetric(t, registry, "lab_firecracker_vcpu_io_out_exits_total", 5)
	assertMetric(t, registry, "lab_firecracker_net_tx_bytes_total", 12)
	assertMetric(t, registry, "lab_firecracker_metrics_samples_total", 2)
	assertMetric(t, registry, "lab_firecracker_metrics_parse_errors_total", 2)
	appendData("{\"utc_timestamp_ms\":3000}\n") // Zero-delta flush retains previous totals.
	assertMetric(t, registry, "lab_firecracker_vcpu_io_out_exits_total", 5)
	assertMetric(t, registry, "lab_firecracker_metrics_last_timestamp_seconds", 3)
	if strings.Contains(snapshotText(t, registry), "process_startup_time_us") {
		t.Fatal("unclassified store metric exported as a counter")
	}
}

func TestDecoderSkipsOversizedLineAndRecovers(t *testing.T) {
	input := strings.Repeat("x", maxRecord+1) + "\n{\"utc_timestamp_ms\":1000}\n"
	m, registry := newTestMetrics(t, "test-vm")
	tail := Decoder{Reader: strings.NewReader(input), Metrics: m}
	if err := tail.Read(); err != nil {
		t.Fatal(err)
	}
	assertMetric(t, registry, "lab_firecracker_metrics_parse_errors_total", 1)
	assertMetric(t, registry, "lab_firecracker_metrics_samples_total", 1)
}
