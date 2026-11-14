package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Saumya40-codes/virutal-lab/pkg/firecracker"
	"github.com/Saumya40-codes/virutal-lab/pkg/telemetry"
	"github.com/prometheus/client_golang/prometheus"
)

func TestPrepareSocketPreservesFilesAndSymlinks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "api.socket")
	if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{path, link} {
		if err := prepareSocket(target); err == nil {
			t.Fatalf("accepted non-socket %s", target)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "keep" {
		t.Fatalf("existing file changed: %q, %v", data, err)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatal(err)
	}
}

func TestControllerStatusSurvivesGuestExit(t *testing.T) {
	code := 0
	m := &manager{logger: slog.New(slog.NewTextHandler(io.Discard, nil)), state: State{ID: "test-vm", State: "Stopped", ExitCode: &code}}
	handler := m.handler()
	for _, tc := range []struct {
		method, path string
		code         int
	}{{"GET", "/vm", 200}, {"PUT", "/vm/reboot", 409}, {"GET", "/vm", 200}, {"GET", "/missing", 404}} {
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequest(tc.method, tc.path, nil))
		if rr.Code != tc.code {
			t.Fatalf("%s %s: %d: %s", tc.method, tc.path, rr.Code, rr.Body.String())
		}
	}
}

func TestControllerRejectsDuplicateStart(t *testing.T) {
	m := &manager{cfg: Config{ID: "test-vm"}, current: &instance{}, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	rr := httptest.NewRecorder()
	m.handler().ServeHTTP(rr, httptest.NewRequest("PUT", "/vm/start", nil))
	if rr.Code != http.StatusConflict {
		t.Fatal(rr.Code, rr.Body.String())
	}
}

func TestWaitReadyStopsWhenProcessExits(t *testing.T) {
	done := make(chan struct{})
	close(done)
	// A closed process channel must return before attempting a real API request.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := waitReady(ctx, &firecracker.Client{}, done); err == nil {
		t.Fatal("accepted a dead process")
	}
}

func TestConfigPathsAndValidation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lab.json")
	for _, tc := range []struct {
		extra     string
		vcpus     int
		wantError bool
	}{
		{"", 1, false},
		{`,"unknown":true`, 1, true},
		{`,"id":"../escape"`, 1, true},
		{`,"metrics_addr":"0.0.0.0:9091"`, 1, true},
		{`,"metrics_addr":"localhost:9091"`, 1, true},
		{`,"metrics_interval_seconds":0`, 1, true},
		{`,"guest_metrics":true`, 1, false},
		{`,"guest_metrics":"true"`, 1, true},
		{"", 0, true},
	} {
		data := fmt.Sprintf(`{"firecracker":"firecracker","socket":"run/api.socket","kernel":"kernel","rootfs":"disk","vcpus":%d,"memory_mib":256%s}`, tc.vcpus, tc.extra)
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		c, err := LoadConfig(path)
		if (err != nil) != tc.wantError {
			t.Fatalf("config %s: %v", data, err)
		}
		if err == nil && (c.Rootfs != filepath.Join(dir, "disk") || c.Socket != filepath.Join(dir, "run/api.socket") || c.Firecracker != "firecracker") {
			t.Fatalf("incorrect path resolution: %+v", c)
		}
	}
}

func TestActivePIDFollowsManagerLifecycle(t *testing.T) {
	m := &manager{state: State{State: "Not started"}}
	if _, err := m.activePID(); err == nil {
		t.Fatal("reported a PID without a guest")
	}
	exited := make(chan struct{})
	m.current = &instance{cmd: &exec.Cmd{Process: &os.Process{Pid: 123}}, processDone: exited}
	m.state.State = "Starting"
	if _, err := m.activePID(); err == nil {
		t.Fatal("collected a guest before boot acceptance")
	}
	m.state.State = "Running"
	if pid, err := m.activePID(); err != nil || pid != 123 {
		t.Fatalf("active PID: %d %v", pid, err)
	}
	close(exited)
	// The lifecycle can still be draining FIFO records, but its old PID must
	// no longer be inspected: the OS may have reused it for another process.
	if _, err := m.activePID(); err == nil {
		t.Fatal("reported an exited PID while cleanup was pending")
	}
}

func TestMetricsEndpointDoesNotExposeVMControl(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := telemetry.NewMetrics()
	if err := metrics.RegisterMetrics(registry); err != nil {
		t.Fatal(err)
	}
	handler := metricsHandler(registry, metrics, (&manager{}).guestMetricsHandler())
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/metrics", 200},
		{"POST", "/metrics", 405},
		{"GET", "/guest/metrics", 503},
		{"POST", "/guest/metrics", 405},
		{"GET", "/guest/metrics/debug/pprof", 404},
		{"PUT", "/vm/start", 404},
		{"PUT", "/vm/reboot", 404},
	} {
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequest(tc.method, tc.path, nil))
		if rr.Code != tc.status {
			t.Fatalf("%s %s: got %d, want %d", tc.method, tc.path, rr.Code, tc.status)
		}
	}
}

func TestGuestMetricsProxyRewritesOnlyToExporterMetrics(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy := newGuestMetricsProxy("unused.socket", logger)
	const contentType = "application/openmetrics-text; version=1.0.0"
	const body = "node_memory_MemAvailable_bytes 123\n# EOF\n"
	proxy.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "http" || r.URL.Host != "guest" || r.URL.Path != "/metrics" || r.URL.RawQuery != "" {
			t.Fatalf("unexpected guest request: %s", r.URL)
		}
		if r.Header.Get("Accept") != contentType {
			t.Fatal("scrape content negotiation was lost")
		}
		if deadline, ok := r.Context().Deadline(); !ok || time.Until(deadline) > 4*time.Second {
			t.Fatal("guest scrape has no overall deadline")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	m := &manager{cfg: Config{GuestMetrics: true}, state: State{State: "Running"}, current: &instance{processDone: make(chan struct{}), guestMetrics: proxy}}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/guest/metrics?collect[]=cpu", nil)
	req.Header.Set("Accept", contentType)
	m.guestMetricsHandler().ServeHTTP(rr, req)
	if rr.Code != 200 || rr.Header().Get("Content-Type") != contentType || rr.Body.String() != body {
		t.Fatalf("scrape response changed: %d %v %q", rr.Code, rr.Header(), rr.Body.String())
	}
	proxy.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("exporter is not listening")
	})
	rr = httptest.NewRecorder()
	m.guestMetricsHandler().ServeHTTP(rr, httptest.NewRequest("GET", "/guest/metrics", nil))
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("failed exporter returned %d", rr.Code)
	}
}

func TestGuestMetricsFollowsGuestLifecycle(t *testing.T) {
	m := &manager{cfg: Config{GuestMetrics: true}, state: State{State: "Not started"}}
	handler := m.guestMetricsHandler()
	check := func(wantCode int, wantBody string) {
		t.Helper()
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequest("GET", "/guest/metrics", nil))
		if rr.Code != wantCode || (wantBody != "" && rr.Body.String() != wantBody) {
			t.Fatalf("guest scrape: %d %q", rr.Code, rr.Body.String())
		}
	}
	check(503, "")
	for _, guest := range []string{"first", "second"} {
		m.current = &instance{processDone: make(chan struct{}), guestMetrics: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, guest)
		})}
		m.state.State = "Starting"
		check(503, "")
		m.state.State = "Running"
		check(200, guest)
		m.cfg.GuestMetrics = false
		check(503, "")
		m.cfg.GuestMetrics = true
		m.closing = true
		check(503, "")
		m.closing = false
		close(m.current.processDone)
		check(503, "")
		m.current = nil
		m.state.State = "Stopped"
		check(503, "")
	}
}
