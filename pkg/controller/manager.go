package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/Saumya40-codes/virutal-lab/pkg/firecracker"
	"github.com/Saumya40-codes/virutal-lab/pkg/telemetry"
)

type State struct {
	ID       string `json:"id"`
	State    string `json:"state"`
	PID      int    `json:"pid,omitempty"`
	Session  string `json:"session,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`
	Error    string `json:"error,omitempty"`
}

type instance struct {
	cmd          *exec.Cmd
	client       *firecracker.Client
	done         chan struct{}
	processDone  chan struct{}
	guestMetrics http.Handler
	waitErr      error // Read only after done closes.
}

type manager struct {
	foreground    bool
	clientFactory func(string, firecracker.Observer) *firecracker.Client
	cfg           Config
	logger        *slog.Logger
	metrics       *telemetry.Metrics
	session       string
	op            sync.Mutex // Serialize start, reboot and controller shutdown.
	mu            sync.Mutex
	current       *instance
	state         State
	closing       bool
}

func (m *manager) status() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

func (m *manager) start(ctx context.Context) (*instance, error) {
	m.op.Lock()
	defer m.op.Unlock()
	m.mu.Lock()
	busy := m.current != nil || m.closing
	m.mu.Unlock()
	if busy {
		return nil, errors.New("VM is already starting/running, or controller is stopping")
	}
	for _, path := range []string{m.cfg.Kernel, m.cfg.Rootfs} {
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("expected a regular file: %s", path)
		}
	}
	if m.cfg.Kernel == m.cfg.Rootfs {
		return nil, errors.New("kernel and rootfs must be different files")
	}

	if err := prepareSocket(m.cfg.Socket); err != nil {
		return nil, err
	}
	session, err := os.MkdirTemp(m.session, "guest-")
	if err != nil {
		return nil, err
	}
	logger := m.logger.With("guest_session", filepath.Base(session))
	var vsockPath string
	if m.cfg.GuestMetrics {
		vsockPath = filepath.Join(session, "vsock.sock")
		if len(vsockPath) > 107 {
			return nil, errors.New("vsock Unix socket path exceeds 107 bytes; shorten runtime_dir")
		}
	}
	logPath := filepath.Join(session, "firecracker.log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	var raw *os.File
	if m.cfg.RawMetrics {
		raw, err = os.OpenFile(filepath.Join(session, "firecracker.metrics.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return nil, err
		}
	}
	// Pass a nil interface when capture is disabled, not a typed nil *os.File.
	metricsPath := filepath.Join(session, "metrics.fifo")
	var fifo *telemetry.FIFO
	if raw != nil {
		fifo, err = telemetry.OpenFIFO(metricsPath, m.metrics, raw)
	} else {
		fifo, err = telemetry.OpenFIFO(metricsPath, m.metrics, nil)
	}
	if err != nil {
		if raw != nil {
			raw.Close()
		}
		return nil, err
	}
	factory := m.clientFactory
	if factory == nil {
		factory = firecracker.New
	}
	client := factory(m.cfg.Socket, func(method, path string, status int, duration time.Duration, err error) {
		m.metrics.RecordAPI(method, path, status, duration, err)
		if err != nil {
			logger.Warn("api_request_failed", "method", method, "path", path, "status", status, "error", err)
		}
	})
	// A request cancellation must not kill an already booted VM. Its lifetime is
	// owned by the manager; failed startup and controller shutdown explicitly reap it.
	cmd := exec.Command(m.cfg.Firecracker, "--id", m.cfg.ID, "--api-sock", m.cfg.Socket)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	started := time.Now()
	if err := cmd.Start(); err != nil {
		client.Close()
		_ = fifo.Finish()
		if raw != nil {
			raw.Close()
		}
		return nil, err
	}
	inst := &instance{cmd: cmd, client: client, done: make(chan struct{}), processDone: make(chan struct{})}
	if m.cfg.GuestMetrics {
		inst.guestMetrics = newGuestMetricsProxy(vsockPath, logger)
	}
	m.mu.Lock()
	m.current = inst
	m.state = State{ID: m.cfg.ID, State: "Starting", PID: cmd.Process.Pid, Session: session}
	m.mu.Unlock()
	m.metrics.ProcessStarted(cmd.Process.Pid)
	logger.Info("firecracker_started", "pid", cmd.Process.Pid, "artifacts", session)
	pollCtx, stopPoll := context.WithCancel(context.Background())
	var poll sync.WaitGroup
	processDone, setupDone := inst.processDone, make(chan struct{})
	go func() {
		inst.waitErr = cmd.Wait()
		close(processDone)
		<-setupDone
		stopPoll()
		poll.Wait()
		if err := fifo.Finish(); err != nil {
			logger.Warn("metrics_stream_failed", "error", err)
		}
		if raw != nil {
			_ = raw.Close()
		}
		client.Close()
		m.metrics.ProcessExited(inst.waitErr != nil)
		code := cmd.ProcessState.ExitCode()
		m.mu.Lock()
		m.current = nil
		m.state = State{ID: m.cfg.ID, State: "Stopped", Session: session, ExitCode: &code}
		if inst.waitErr != nil {
			m.state.Error = inst.waitErr.Error()
		}
		m.mu.Unlock()
		logger.Info("firecracker_exited", "exit_code", code, "error", inst.waitErr)
		close(inst.done)
	}()
	abort := func(startErr error) (*instance, error) {
		_ = cmd.Process.Kill()
		close(setupDone)
		<-inst.done
		return nil, startErr
	}
	readyCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := waitReady(readyCtx, client, processDone); err != nil {
		return abort(err)
	}
	boot := firecracker.BootConfig{
		Kernel:    m.cfg.Kernel,
		Rootfs:    m.cfg.Rootfs,
		BootArgs:  m.cfg.BootArgs,
		VCPUs:     m.cfg.VCPUs,
		MemoryMiB: m.cfg.MemoryMiB,
		VSockPath: vsockPath,
	}
	if err := client.Configure(ctx, boot, logPath, metricsPath); err != nil {
		return abort(err)
	}
	m.metrics.BootAccepted(time.Since(started))
	m.mu.Lock()
	m.state.State = "Running"
	m.mu.Unlock()
	logger.Info("instance_start_accepted", "duration_seconds", time.Since(started).Seconds(), "guest_ready_verified", false)
	poll.Add(1)
	go func() {
		defer poll.Done()
		tick := time.NewTicker(time.Duration(m.cfg.MetricsIntervalSeconds) * time.Second)
		defer tick.Stop()
		collect := func() {
			if _, err := client.Do(pollCtx, http.MethodPut, "/actions", map[string]string{"action_type": "FlushMetrics"}); err != nil && pollCtx.Err() == nil {
				logger.Warn("metrics_flush_failed", "error", err)
			}
		}
		collect()
		streamDone := fifo.Done()
		for {
			select {
			case <-pollCtx.Done():
				return
			case <-streamDone:
				logger.Warn("metrics_stream_closed", "error", fifo.Err())
				streamDone = nil
			case <-tick.C:
				collect()
			}
		}
	}()
	close(setupDone)
	return inst, nil
}

func (m *manager) reboot(ctx context.Context) error {
	m.op.Lock()
	defer m.op.Unlock()
	m.mu.Lock()
	inst := m.current
	m.mu.Unlock()
	if inst == nil {
		return errors.New("no running VM")
	}
	_, err := inst.client.Do(ctx, http.MethodPut, "/actions", map[string]string{"action_type": "SendCtrlAltDel"})
	if err == nil {
		m.logger.Info("guest_reboot_requested", "shutdown_complete", false)
	}
	return err
}

func (m *manager) close(ctx context.Context) {
	m.op.Lock()
	defer m.op.Unlock()
	m.mu.Lock()
	m.closing = true
	inst := m.current
	m.mu.Unlock()
	if inst == nil {
		return
	}
	if _, err := inst.client.Do(ctx, http.MethodPut, "/actions", map[string]string{"action_type": "SendCtrlAltDel"}); err != nil {
		m.logger.Warn("shutdown_request_failed", "error", err)
	}
	select {
	case <-inst.done:
	case <-ctx.Done():
		m.logger.Warn("forcing_vm_stop", "reason", "graceful shutdown timeout; pending disk writes may be lost")
		_ = inst.cmd.Process.Kill()
		<-inst.done
	}
}

// activePID reads the manager's existing lifecycle state. Checking processDone
// stops collection as soon as Wait returns, before FIFO cleanup can finish.
func (m *manager) activePID() (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current == nil || m.state.State != "Running" {
		return 0, errors.New("no running Firecracker process")
	}
	select {
	case <-m.current.processDone:
		return 0, errors.New("Firecracker process has exited")
	default:
		return m.current.cmd.Process.Pid, nil
	}
}

func (m *manager) guestMetricsHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		var proxy http.Handler
		if m.cfg.GuestMetrics && !m.closing && m.current != nil && m.state.State == "Running" {
			select {
			case <-m.current.processDone:
			default:
				proxy = m.current.guestMetrics
			}
		}
		m.mu.Unlock()
		w.Header().Set("Cache-Control", "no-store")
		if proxy == nil {
			http.Error(w, "guest metrics require an enabled, running VM", http.StatusServiceUnavailable)
			return
		}
		// Bound the entire scrape, including a guest that stalls in its body.
		ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
		defer cancel()
		proxy.ServeHTTP(w, r.WithContext(ctx))
	})
}
