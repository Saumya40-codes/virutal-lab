package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
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
		{"/actions", map[string]string{"action_type": "InstanceStart"}},
	}
	for _, s := range steps {
		if _, err := c.Do(ctx, http.MethodPut, s.path, s.body); err != nil {
			return err
		}
	}
	return nil
}
