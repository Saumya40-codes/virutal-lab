package telemetry

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync"
	"syscall"
	"time"
)

// Only SharedIncMetric fields from Firecracker v1.17 are accumulated here.
// They contain deltas since the preceding flush, NOT lifetime totals.
// Source: https://github.com/firecracker-microvm/firecracker/blob/v1.17.0/src/vmm/src/logger/metrics.rs
// Aggregate block/net sections are used without adding their per-device copies.
type sample struct {
	Timestamp uint64 `json:"utc_timestamp_ms"`
	VCPU      struct {
		IOIn      uint64 `json:"exit_io_in"`
		IOOut     uint64 `json:"exit_io_out"`
		MMIORead  uint64 `json:"exit_mmio_read"`
		MMIOWrite uint64 `json:"exit_mmio_write"`
		Failures  uint64 `json:"failures"`
	} `json:"vcpu"`
	Block struct {
		ReadBytes  uint64 `json:"read_bytes"`
		WriteBytes uint64 `json:"write_bytes"`
		Reads      uint64 `json:"read_count"`
		Writes     uint64 `json:"write_count"`
	} `json:"block"`
	Net struct {
		RXBytes uint64 `json:"rx_bytes_count"`
		TXBytes uint64 `json:"tx_bytes_count"`
	} `json:"net"`
}

func (m *Metrics) ingest(line []byte) {
	var s sample
	err := json.Unmarshal(line, &s)
	if err != nil || s.Timestamp == 0 {
		m.MetricsParseErrors.Inc()
		return
	}
	m.MetricsSamples.Inc()
	m.MetricsLastTimestamp.Set(float64(s.Timestamp) / 1000)
	m.VCPUIOInExits.Add(float64(s.VCPU.IOIn))
	m.VCPUIOOutExits.Add(float64(s.VCPU.IOOut))
	m.VCPUMMIOReadExits.Add(float64(s.VCPU.MMIORead))
	m.VCPUMMIOWriteExits.Add(float64(s.VCPU.MMIOWrite))
	m.VCPUFailures.Add(float64(s.VCPU.Failures))
	m.BlockReadBytes.Add(float64(s.Block.ReadBytes))
	m.BlockWriteBytes.Add(float64(s.Block.WriteBytes))
	m.BlockReads.Add(float64(s.Block.Reads))
	m.BlockWrites.Add(float64(s.Block.Writes))
	m.NetRXBytes.Add(float64(s.Net.RXBytes))
	m.NetTXBytes.Add(float64(s.Net.TXBytes))

}

// Decoder consumes a JSON record stream and retains partial records across reads.
// Use one decoder per VM launch; records are never replayed on scrape.
type Decoder struct {
	Reader   io.Reader
	Metrics  *Metrics
	pending  []byte
	dropping bool
}

const maxRecord = 1 << 20

func (t *Decoder) Read() error {
	var buf [32768]byte
	for {
		n, err := t.Reader.Read(buf[:])
		data := buf[:n]
		for len(data) > 0 {
			end := bytes.IndexByte(data, '\n')
			chunk := data
			if end >= 0 {
				chunk = data[:end]
			}
			if !t.dropping {
				if len(t.pending)+len(chunk) > maxRecord {
					t.Metrics.MetricsParseErrors.Inc()
					t.pending = nil
					t.dropping = true
				} else {
					t.pending = append(t.pending, chunk...)
				}
			}
			if end < 0 {
				break
			}
			if !t.dropping && len(bytes.TrimSpace(t.pending)) > 0 {
				t.Metrics.ingest(t.pending)
			}
			t.pending = nil
			t.dropping = false
			data = data[end+1:]
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if n == 0 {
			return nil
		}
	}
}

// FIFO follows the named-pipe pattern in firecracker-microvm/firecracker-go-sdk:
// https://github.com/firecracker-microvm/firecracker-go-sdk/blob/main/machine.go
// The SDK is a reference, not a dependency. The reader opens before the VMM
// writer; a temporary writer keeps reads alive until the VMM exits.
type FIFO struct {
	reader    *os.File
	guard     *os.File
	done      chan struct{}
	once      sync.Once
	readErr   error
	finishErr error
}

func OpenFIFO(path string, metrics *Metrics, raw io.Writer) (*FIFO, error) {
	if err := syscall.Mkfifo(path, 0600); err != nil {
		return nil, err
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	reader := os.NewFile(uintptr(fd), path)
	guard, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		reader.Close()
		return nil, err
	}
	fifo := &FIFO{reader: reader, guard: guard, done: make(chan struct{})}
	var source io.Reader = reader
	if raw != nil {
		source = io.TeeReader(reader, raw)
	}
	go func() {
		defer close(fifo.done)
		decoder := Decoder{Reader: source, Metrics: metrics}
		fifo.readErr = decoder.Read()
	}()
	return fifo, nil
}

// Finish is called after the VMM exits. Close the guard, drain remaining
// records to EOF, and bound shutdown if another writer unexpectedly remains.
func (f *FIFO) Finish() error {
	f.once.Do(func() {
		_ = f.guard.Close()
		select {
		case <-f.done:
		case <-time.After(2 * time.Second):
			f.finishErr = errors.New("metrics FIFO drain timed out")
			_ = f.reader.Close()
			<-f.done
		}
		_ = f.reader.Close()
	})
	if f.finishErr != nil {
		return f.finishErr
	}
	if errors.Is(f.readErr, os.ErrClosed) {
		return nil
	}
	return f.readErr
}

// Done and Err allow the supervisor to report an unexpectedly closed stream.
func (f *FIFO) Done() <-chan struct{} { return f.done }
func (f *FIFO) Err() error {
	select {
	case <-f.done:
		return f.readErr
	default:
		return nil
	}
}
