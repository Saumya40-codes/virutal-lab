package firecracker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Observer receives results for requests made by this client, including transport errors (status 0).
type Observer func(method, path string, status int, duration time.Duration, err error)

type Client struct {
	HTTP    *http.Client
	Observe Observer
}

func New(socket string, observe Observer) *Client {
	return &Client{Observe: observe, HTTP: &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		}},
	}}
}

func (c *Client) Close() { c.HTTP.CloseIdleConnections() }

func (c *Client) Do(ctx context.Context, method, path string, body any) (data []byte, err error) {
	started, status := time.Now(), 0
	defer func() {
		if c.Observe != nil {
			c.Observe(method, path, status, time.Since(started), err)
		}
	}()
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	status = resp.StatusCode
	data, err = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("%s %s: HTTP %d: %s", method, path, status, data)
	}
	return data, nil
}

// BootConfig contains only the guest settings sent to Firecracker's API.
type BootConfig struct {
	Kernel    string
	Rootfs    string
	BootArgs  string
	VCPUs     int
	MemoryMiB int
	VSockPath string
}

type step struct {
	path string
	body any
}

func (c *Client) Configure(ctx context.Context, cfg BootConfig, logPath, metricsPath string) error {
	steps := []step{
		{"/logger", map[string]any{"log_path": logPath, "level": "Info", "show_level": true, "show_log_origin": true}},
		{"/metrics", map[string]any{"metrics_path": metricsPath, "emit_id": true}},
		{"/machine-config", map[string]any{"vcpu_count": cfg.VCPUs, "mem_size_mib": cfg.MemoryMiB}},
		{"/boot-source", map[string]any{"kernel_image_path": cfg.Kernel, "boot_args": cfg.BootArgs}},
		{"/drives/rootfs", map[string]any{"drive_id": "rootfs", "path_on_host": cfg.Rootfs, "is_root_device": true, "is_read_only": false}},
	}
	if cfg.VSockPath != "" {
		steps = append(steps, step{"/vsock", map[string]any{"guest_cid": 3, "uds_path": cfg.VSockPath}})
	}
	steps = append(steps, step{"/actions", map[string]string{"action_type": "InstanceStart"}})
	for _, s := range steps {
		if _, err := c.Do(ctx, http.MethodPut, s.path, s.body); err != nil {
			return err
		}
	}
	return nil
}

// DialVSock connects through Firecracker's host Unix socket, not host AF_VSOCK.
// https://github.com/firecracker-microvm/firecracker/blob/v1.17.0/docs/vsock.md
func DialVSock(ctx context.Context, socket string, port uint32) (net.Conn, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, err
	}
	return connectVSock(ctx, conn, port)
}

func connectVSock(ctx context.Context, conn net.Conn, port uint32) (_ net.Conn, err error) {
	defer func() {
		if err != nil {
			_ = conn.Close()
			if ctx.Err() != nil {
				err = ctx.Err()
			}
		}
	}()
	// Bound both handshake reads and writes, including when ctx has no deadline.
	deadline := time.Now().Add(3 * time.Second)
	if limit, ok := ctx.Deadline(); ok && limit.Before(deadline) {
		deadline = limit
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", port); err != nil {
		return nil, fmt.Errorf("vsock connect request: %w", err)
	}
	reader := bufio.NewReaderSize(conn, 64)
	line, err := reader.ReadSlice('\n')
	if err != nil {
		return nil, fmt.Errorf("vsock connect acknowledgement: %w", err)
	}
	fields := strings.Fields(string(line))
	if len(fields) != 2 || fields[0] != "OK" {
		return nil, fmt.Errorf("invalid vsock acknowledgement: %q", line)
	}
	// OK contains the assigned host-side port, not the requested guest port.
	if _, err := strconv.ParseUint(fields[1], 10, 32); err != nil {
		return nil, fmt.Errorf("invalid vsock acknowledgement port: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}
	// Preserve bytes buffered beyond the acknowledgement for the HTTP reader.
	return &vsockConn{Conn: conn, reader: reader}, nil
}

type vsockConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *vsockConn) Read(p []byte) (int, error) { return c.reader.Read(p) }
