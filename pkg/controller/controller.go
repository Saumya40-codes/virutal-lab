package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Saumya40-codes/virutal-lab/pkg/firecracker"
	"github.com/Saumya40-codes/virutal-lab/pkg/telemetry"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/expfmt"
)

// Start retains the foreground single-VM lifecycle. Serve is an explicit
// long-lived mode that accepts repeated starts and survives guest exits.
func Start(ctx context.Context, c Config) error { return run(ctx, c, true) }
func Serve(ctx context.Context, c Config) error { return run(ctx, c, false) }

func run(ctx context.Context, c Config, boot bool) (err error) {
	if err := prepareSocket(c.ControlSocket()); err != nil {
		return err
	}
	if err := os.MkdirAll(c.RuntimeDir, 0700); err != nil {
		return err
	}
	session, err := os.MkdirTemp(c.RuntimeDir, "session-")
	if err != nil {
		return err
	}
	log, err := os.OpenFile(filepath.Join(session, "controller.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	logger := slog.New(slog.NewJSONHandler(io.MultiWriter(os.Stderr, log), nil)).With("vm_id", c.ID, "session", filepath.Base(session))
	metrics := telemetry.NewMetrics()
	m := &manager{foreground: boot, cfg: c, logger: logger, metrics: metrics, session: session, state: State{ID: c.ID, State: "Not started"}}
	registry := prometheus.NewRegistry()
	reg := prometheus.WrapRegistererWith(prometheus.Labels{"vm_id": c.ID}, registry)
	if err := metrics.RegisterMetrics(reg); err != nil {
		return err
	}
	for _, collector := range []prometheus.Collector{
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		telemetry.NewFirecrackerProcessCollector(m.activePID),
		telemetry.NewHostCollector(m.activePID),
	} {
		if err := reg.Register(collector); err != nil {
			return fmt.Errorf("register host collectors: %w", err)
		}
	}
	defer func() {
		snapshot, saveErr := telemetry.Snapshot(registry, expfmt.NewFormat(expfmt.TypeTextPlain))
		if saveErr == nil {
			saveErr = os.WriteFile(filepath.Join(session, "final.prom"), snapshot, 0600)
		}
		if saveErr != nil {
			logger.Error("final_metrics_write_failed", "error", saveErr)
		}
		protobuf, protoErr := telemetry.Snapshot(registry, expfmt.NewFormat(expfmt.TypeProtoDelim))
		if protoErr == nil {
			protoErr = os.WriteFile(filepath.Join(session, "final.pb"), protobuf, 0600)
		}
		if protoErr != nil {
			logger.Error("final_native_metrics_write_failed", "error", protoErr)
		}
		logger.Info("controller_stopped", "error", err)
	}()
	listener, err := net.Listen("tcp", c.MetricsAddr)
	if err != nil {
		return fmt.Errorf("metrics listener: %w", err)
	}
	defer listener.Close()
	control, err := net.Listen("unix", c.ControlSocket())
	if err != nil {
		return fmt.Errorf("controller listener: %w", err)
	}
	defer control.Close()
	if err := os.Chmod(c.ControlSocket(), 0600); err != nil {
		return err
	}
	metricsServer := &http.Server{Handler: metricsHandler(registry, metrics, m.guestMetricsHandler()), ReadHeaderTimeout: 3 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}
	controlServer := &http.Server{Handler: m.handler(), ReadHeaderTimeout: 3 * time.Second, WriteTimeout: 20 * time.Second, IdleTimeout: 30 * time.Second}
	serverErrors := make(chan error, 2)
	for _, pair := range []struct {
		s *http.Server
		l net.Listener
	}{{metricsServer, listener}, {controlServer, control}} {
		go func(s *http.Server, l net.Listener) {
			err := s.Serve(l)
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				serverErrors <- err
			}
		}(pair.s, pair.l)
	}
	defer func() {
		m.mu.Lock()
		m.closing = true
		m.mu.Unlock()
		shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := controlServer.Shutdown(shutdown); err != nil {
			_ = controlServer.Close()
		}
		guestShutdown, guestCancel := context.WithTimeout(context.Background(), 10*time.Second)
		m.close(guestShutdown)
		guestCancel()
		shutdownMetrics, cancelMetrics := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancelMetrics()
		if err := metricsServer.Shutdown(shutdownMetrics); err != nil {
			_ = metricsServer.Close()
		}
	}()
	logger.Info("controller_ready", "mode", map[bool]string{true: "foreground", false: "serve"}[boot], "metrics_url", "http://"+c.MetricsAddr+"/metrics", "control_socket", c.ControlSocket(), "artifacts", session)
	var done <-chan struct{}
	var inst *instance
	if boot {
		inst, err = m.start(ctx)
		if err != nil {
			return err
		}
		done = inst.done
	}
	select {
	case <-ctx.Done():
		return nil
	case err := <-serverErrors:
		return err
	case <-done:
		return inst.waitErr
	}
}

func waitReady(ctx context.Context, client *firecracker.Client, done <-chan struct{}) error {
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for Firecracker API: %w", ctx.Err())
		case <-done:
			return errors.New("Firecracker exited before API readiness")
		case <-tick.C:
			if _, err := client.Do(ctx, http.MethodGet, "/", nil); err == nil {
				return nil
			}
		}
	}
}

// Only remove an abandoned socket, never an active listener or another file.
func prepareSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return os.MkdirAll(filepath.Dir(path), 0755)
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("refusing to remove non-socket %s", path)
	}
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err == nil {
		conn.Close()
		return fmt.Errorf("another process is listening on %s", path)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Errorf("cannot establish whether socket is stale: %w", err)
	}
	return os.Remove(path)
}

// Mutating endpoints are served only on the user-owned Unix socket.
// The loopback TCP listener exposes metrics, never VM control operations.
func (m *manager) handler() http.Handler {
	mux := http.NewServeMux()
	respond := func(w http.ResponseWriter, status int, body any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}
	mux.HandleFunc("GET /vm", func(w http.ResponseWriter, r *http.Request) { respond(w, http.StatusOK, m.status()) })
	mux.HandleFunc("PUT /vm/start", func(w http.ResponseWriter, r *http.Request) {
		if m.foreground {
			respond(w, http.StatusConflict, map[string]string{"error": "foreground mode owns one VM; use lab serve for remote starts"})
			return
		}
		if _, err := m.start(r.Context()); err != nil {
			m.logger.Warn("start_failed", "error", err)
			respond(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		respond(w, http.StatusOK, m.status())
	})
	mux.HandleFunc("PUT /vm/reboot", func(w http.ResponseWriter, r *http.Request) {
		if err := m.reboot(r.Context()); err != nil {
			respond(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		respond(w, http.StatusAccepted, map[string]string{"state": "reboot requested"})
	})
	return mux
}

var validID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

type Config struct {
	RawMetrics             bool   `json:"raw_metrics"`
	GuestMetrics           bool   `json:"guest_metrics"`
	ID                     string `json:"id"`
	RuntimeDir             string `json:"runtime_dir"`
	MetricsAddr            string `json:"metrics_addr"`
	MetricsIntervalSeconds int    `json:"metrics_interval_seconds"`
	Firecracker            string `json:"firecracker"`
	Socket                 string `json:"socket"`
	Kernel                 string `json:"kernel"`
	Rootfs                 string `json:"rootfs"`
	VCPUs                  int    `json:"vcpus"`
	MemoryMiB              int    `json:"memory_mib"`
	BootArgs               string `json:"boot_args"`
}

func LoadConfig(path string) (Config, error) {
	c := Config{ID: "lab-01", MetricsAddr: "127.0.0.1:9091", MetricsIntervalSeconds: 5}
	f, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer f.Close()
	d := json.NewDecoder(f)
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return c, errors.New("config must contain one JSON object")
	}
	if !validID.MatchString(c.ID) {
		return c, errors.New("id must contain 1-64 letters, digits, underscores or hyphens")
	}
	if c.RuntimeDir == "" {
		c.RuntimeDir = filepath.Join("run", c.ID)
	}
	if c.Socket == "" {
		c.Socket = filepath.Join(c.RuntimeDir, "firecracker.socket")
	}
	if c.MetricsIntervalSeconds < 1 || c.MetricsIntervalSeconds > 60 {
		return c, errors.New("metrics_interval_seconds must be between 1 and 60")
	}
	host, port, err := net.SplitHostPort(c.MetricsAddr)
	ip := net.ParseIP(host)
	n, portErr := strconv.Atoi(port)
	if err != nil || ip == nil || !ip.IsLoopback() || portErr != nil || n < 1 || n > 65535 {
		return c, errors.New("metrics_addr must be a numeric loopback IP and port, e.g. 127.0.0.1:9091")
	}
	if c.Firecracker == "" || c.Socket == "" || c.Kernel == "" || c.Rootfs == "" || c.VCPUs < 1 || c.MemoryMiB < 1 {
		return c, errors.New("config requires firecracker, socket, kernel, rootfs, positive vcpus and memory_mib")
	}
	base, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return c, err
	}
	resolve := func(p string) string {
		if filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(base, p)
	}
	c.RuntimeDir = resolve(c.RuntimeDir)
	c.Socket, c.Kernel, c.Rootfs = resolve(c.Socket), resolve(c.Kernel), resolve(c.Rootfs)
	if strings.ContainsRune(c.Firecracker, '/') {
		c.Firecracker = resolve(c.Firecracker)
	}

	return c, nil
}

// ControlSocket is the controller management endpoint, separate from the VMM API.
func (c Config) ControlSocket() string { return filepath.Join(c.RuntimeDir, "controller.socket") }

// Exporter wiring belongs to controller setup, not the metric definitions.
func metricsHandler(reg prometheus.Gatherer, metrics *telemetry.Metrics, guest http.Handler) http.Handler {
	handler := promhttp.InstrumentHandlerDuration(metrics.MetricsScrapeDuration,
		promhttp.InstrumentHandlerInFlight(metrics.MetricsScrapesInFlight,
			promhttp.InstrumentHandlerCounter(metrics.MetricsScrapeRequests,
				promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))))
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", handler)
	mux.Handle("GET /guest/metrics", guest)
	return mux
}

// The guest endpoint streams Node Exporter's response without ingesting its
// metrics into the controller registry. Each launch has its own vsock socket.
func newGuestMetricsProxy(socket string, logger *slog.Logger) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(&url.URL{Scheme: "http", Host: "guest"})
			r.Out.URL.Path = "/metrics"
			r.Out.URL.RawPath = ""
			r.Out.URL.RawQuery = ""
		},
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return firecracker.DialVSock(ctx, socket, 9100)
			},
			// Reconnect on every scrape; no idle connections survive guest exit.
			DisableKeepAlives:     true,
			ResponseHeaderTimeout: 3 * time.Second,
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			logger.Warn("guest_metrics_proxy_failed", "error", err)
			http.Error(w, "guest metrics unavailable", http.StatusBadGateway)
		},
		ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
}
