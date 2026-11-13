package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Saumya40-codes/virutal-lab/pkg/firecracker"
	"github.com/Saumya40-codes/virutal-lab/pkg/telemetry"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
)

// TestVMProcessFixture is a child lifecycle fixture, not Firecracker or KVM.
func TestVMProcessFixture(t *testing.T) {
	if os.Getenv("LAB_FAKE_VMM") != "1" {
		return
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGUSR1)
	defer signal.Stop(stop)
	if err := os.WriteFile(os.Getenv("LAB_FAKE_READY"), []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	<-stop
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestManagerRestartsGuestAndRetainsMetrics(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "lab-manager-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("LAB_TEST_BINARY", executable)
	t.Setenv("LAB_FAKE_VMM", "1")
	readyPath := filepath.Join(dir, "ready")
	t.Setenv("LAB_FAKE_READY", readyPath)
	helper := filepath.Join(dir, "fake-vmm")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nexec \"$LAB_TEST_BINARY\" -test.run=TestVMProcessFixture -- \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	kernel, disk := filepath.Join(dir, "kernel"), filepath.Join(dir, "disk")
	for _, path := range []string{kernel, disk} {
		if err := os.WriteFile(path, []byte("fixture; do not modify"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	metrics := telemetry.NewMetrics()
	registry := prometheus.NewRegistry()
	if err := metrics.RegisterMetrics(prometheus.WrapRegistererWith(prometheus.Labels{"vm_id": "test-vm"}, registry)); err != nil {
		t.Fatal(err)
	}
	m := &manager{
		cfg:     Config{ID: "test-vm", Firecracker: helper, Socket: filepath.Join(dir, "vmm.sock"), Kernel: kernel, Rootfs: disk, VCPUs: 1, MemoryMiB: 256, MetricsIntervalSeconds: 1},
		session: dir, metrics: metrics, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), state: State{ID: "test-vm", State: "Not started"},
	}
	// Replace only HTTP transport; use real child processes and real FIFOs.
	// This keeps lifecycle tests independent of sandbox network permissions.
	var apiMu sync.Mutex
	var writer *os.File
	m.clientFactory = func(socket string, observe firecracker.Observer) *firecracker.Client {
		client := firecracker.New(socket, observe)
		client.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			apiMu.Lock()
			defer apiMu.Unlock()
			status, body := http.StatusNoContent, ""
			if r.Method == "GET" {
				if _, err := os.Stat(readyPath); err != nil {
					return nil, err
				}
				status, body = http.StatusOK, `{"state":"Not started"}`
			}
			if r.URL.Path == "/metrics" {
				var input struct {
					Path string `json:"metrics_path"`
				}
				if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
					return nil, err
				}
				var err error
				writer, err = os.OpenFile(input.Path, os.O_WRONLY, 0)
				if err != nil {
					return nil, err
				}
			}
			if r.URL.Path == "/actions" {
				var input struct {
					Action string `json:"action_type"`
				}
				if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
					return nil, err
				}
				if input.Action == "InstanceStart" {
					if _, err := fmt.Fprintln(writer, `{"utc_timestamp_ms":1000,"vcpu":{"exit_io_out":2}}`); err != nil {
						return nil, err
					}
				}
				if input.Action == "SendCtrlAltDel" {
					if writer != nil {
						writer.Close()
						writer = nil
					}
					m.mu.Lock()
					inst := m.current
					m.mu.Unlock()
					if err := inst.cmd.Process.Signal(syscall.SIGUSR1); err != nil {
						return nil, err
					}
				}
			}
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		})
		return client
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		m.close(ctx)
	}()
	for cycle := 1; cycle <= 2; cycle++ {
		_ = os.Remove(readyPath)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		inst, err := m.start(ctx)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if m.status().State != "Running" {
			t.Fatal(m.status())
		}
		if _, err := m.start(context.Background()); err == nil {
			t.Fatal("accepted duplicate start")
		}
		if err := m.reboot(context.Background()); err != nil {
			t.Fatal(err)
		}
		select {
		case <-inst.done:
		case <-time.After(5 * time.Second):
			t.Fatal("VMM did not exit")
		}
		if state := m.status(); state.State != "Stopped" || state.ExitCode == nil || *state.ExitCode != 0 {
			t.Fatal(state)
		}
		rr := metricsSnapshot(t, registry)
		for _, want := range []string{
			fmt.Sprintf("lab_vm_starts_total{vm_id=\"test-vm\"} %d", cycle),
			fmt.Sprintf("lab_vm_exits_total{vm_id=\"test-vm\"} %d", cycle),
			fmt.Sprintf("lab_firecracker_vcpu_io_out_exits_total{vm_id=\"test-vm\"} %d", cycle*2),
			"lab_vm_running{vm_id=\"test-vm\"} 0",
		} {
			if !strings.Contains(rr, want) {
				t.Fatalf("missing %q in %s", want, rr)
			}
		}
	}
	data, err := os.ReadFile(disk)
	if err != nil || string(data) != "fixture; do not modify" {
		t.Fatal("disk fixture was modified")
	}
}

func metricsSnapshot(t *testing.T, registry *prometheus.Registry) string {
	t.Helper()
	data, err := telemetry.Snapshot(registry, expfmt.NewFormat(expfmt.TypeTextPlain))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
