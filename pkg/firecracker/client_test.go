package firecracker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestConfigureBootOrderAndFailure(t *testing.T) {
	for _, failAt := range []string{"", "/logger", "/metrics", "/boot-source", "/vsock"} {
		t.Run("failure="+failAt, func(t *testing.T) {
			var paths []string
			c := BootConfig{Kernel: "/kernel", Rootfs: "/disk", VCPUs: 1, MemoryMiB: 256, VSockPath: "/vsock.sock"}
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				paths = append(paths, r.URL.Path)
				if r.Method != http.MethodPut || r.Header.Get("Content-Type") != "application/json" {
					t.Fatal("invalid API request")
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				switch r.URL.Path {
				case "/logger":
					if body["log_path"] != "/logs" {
						t.Fatal(body)
					}
				case "/metrics":
					if body["metrics_path"] != "/metrics" || body["emit_id"] != true {
						t.Fatal(body)
					}
				case "/machine-config":
					if body["vcpu_count"] != float64(1) || body["mem_size_mib"] != float64(256) {
						t.Fatal(body)
					}
				case "/boot-source":
					if body["kernel_image_path"] != c.Kernel {
						t.Fatal(body)
					}
				case "/drives/rootfs":
					if body["path_on_host"] != c.Rootfs || body["is_read_only"] != false {
						t.Fatal(body)
					}
				case "/vsock":
					if body["guest_cid"] != float64(3) || body["uds_path"] != c.VSockPath {
						t.Fatal(body)
					}
				case "/actions":
					if body["action_type"] != "InstanceStart" {
						t.Fatal(body)
					}
				}
				status, response := 204, ""
				if r.URL.Path == failAt {
					status, response = 400, `{"fault_message":"rejected"}`
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(response)), Header: make(http.Header)}, nil
			})}
			err := (&Client{HTTP: client}).Configure(context.Background(), c, "/logs", "/metrics")
			want := []string{"/logger", "/metrics", "/machine-config", "/boot-source", "/drives/rootfs", "/vsock", "/actions"}
			if failAt != "" {
				if err == nil || !strings.Contains(err.Error(), "rejected") {
					t.Fatalf("expected API failure, got %v", err)
				}
				for i, path := range want {
					if path == failAt {
						want = want[:i+1]
						break
					}
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(paths, want) {
				t.Fatalf("requests %v, want %v", paths, want)
			}
		})
	}
}

func TestConfigureWithoutVSock(t *testing.T) {
	client := &Client{HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/vsock" {
			t.Fatal("configured vsock when guest metrics are disabled")
		}
		return &http.Response{StatusCode: 204, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})}}
	if err := client.Configure(context.Background(), BootConfig{}, "/logs", "/metrics"); err != nil {
		t.Fatal(err)
	}
}

func TestVSockHandshakePreservesBufferedResponse(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	release := make(chan struct{})
	defer close(release)
	want := "HTTP/1.1 200 OK\r\nContent-Length: 7\r\n\r\nmetrics"
	go func() {
		defer server.Close()
		line, err := bufio.NewReader(server).ReadString('\n')
		if err != nil || line != "CONNECT 9100\n" {
			t.Errorf("connect request: %q, %v", line, err)
			return
		}
		// The assigned host port differs from the requested guest port. HTTP
		// bytes can arrive in the same read as this acknowledgement.
		_, _ = io.WriteString(server, "OK 1073741824\n"+want)
		<-release
	}()
	conn, err := connectVSock(context.Background(), client, 9100)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	got := make([]byte, len(want))
	_, err = io.ReadFull(conn, got)
	if err != nil || string(got) != want {
		t.Fatalf("HTTP stream: %q, %v", got, err)
	}
}

func TestVSockHandshakeRejectsMalformedAcknowledgement(t *testing.T) {
	for _, response := range []string{"", "NO 9100\n", "OK nope\n", "OK 4294967296\n", strings.Repeat("x", 100) + "\n"} {
		t.Run(fmt.Sprintf("response=%q", response), func(t *testing.T) {
			client, server := net.Pipe()
			defer client.Close()
			done := make(chan struct{})
			go func() {
				defer close(done)
				defer server.Close()
				_, _ = bufio.NewReader(server).ReadString('\n')
				_, _ = io.WriteString(server, response)
			}()
			conn, err := connectVSock(context.Background(), client, 9100)
			if err == nil || conn != nil {
				t.Fatalf("accepted response %q", response)
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("failed handshake left its connection open")
			}
		})
	}
}

func TestVSockHandshakeCancellation(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		defer server.Close()
		_, _ = bufio.NewReader(server).ReadString('\n')
		cancel()
		_, _ = io.Copy(io.Discard, server)
	}()
	if conn, err := connectVSock(ctx, client, 9100); !errors.Is(err, context.Canceled) || conn != nil {
		t.Fatalf("cancelled handshake: %v, %v", conn, err)
	}
}

func TestObserverReceivesHTTPAndTransportFailures(t *testing.T) {
	for _, status := range []int{204, 400, 0} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			called := 0
			client := Client{
				HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					if status == 0 {
						return nil, errors.New("transport failed")
					}
					return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("response"))}, nil
				})},
				Observe: func(method, path string, gotStatus int, duration time.Duration, err error) {
					called++
					if method != "PUT" || path != "/actions" || gotStatus != status || duration < 0 || (err != nil) != (status != 204) {
						t.Fatalf("bad observation: %s %s %d %v", method, path, gotStatus, err)
					}
				},
			}
			_, _ = client.Do(context.Background(), "PUT", "/actions", map[string]string{"action_type": "FlushMetrics"})
			if called != 1 {
				t.Fatalf("observer called %d times", called)
			}
		})
	}
}
