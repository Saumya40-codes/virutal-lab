# Virtual Lab

A Linux infrastructure lab built around Firecracker microVMs and KVM. A Go controller provides guest lifecycle management and local observability for virtualization and distributed systems experiments.

Each controller manages one microVM with a serial console, a persistent guest disk and Prometheus metrics.

## Requirements

- Linux with read/write access to `/dev/kvm`.
- Go 1.27 and Firecracker v1.17.0.
- An x86_64 guest kernel and writable ext4 disk image.

Guest artifacts are supplied separately and excluded from version control.

## CLI

```bash
go build -o bin/lab .
./bin/lab --help
```

| Command | Purpose |
| --- | --- |
| `lab serve` | Run the persistent controller and metrics endpoint |
| `lab start` | Boot the configured guest through the controller, or run it standalone |
| `lab status` | Report VM lifecycle state |
| `lab reboot` | Request guest reboot; Firecracker exits on guest reset |

In `serve` mode, the guest console attaches to the controller terminal and the controller survives guest exit. Standalone `start` exits with the guest.

## Configuration

[lab.json](lab.json) defines resources, artifact paths and runtime settings. The sample configuration allocates 1 vCPU and 256 MiB RAM, with paths for Linux 6.1.188 and an Ubuntu 24.04 disk image. `--config` / `-c` selects another configuration; relative paths resolve from that file.

## Guest image preparation

An ext4 disk image is a file containing the guest's filesystem. With the VM stopped, the [preparation script](examples/prepare-node-exporter.sh) copies the image and uses `debugfs` to place Node Exporter and its service file inside the copy. These become ordinary guest files when the guest kernel mounts the disk. The original image is preserved; no host filesystem mount or root privileges are needed.

The script expects the Node Exporter 1.12.1 Linux amd64 archive and its checksum manifest named `node_exporter-1.12.1.sha256sums.txt` in `downloads/`. It verifies the checksum and resulting filesystem, producing `images/ubuntu-24.04-node-exporter.ext4`:

```bash
bash examples/prepare-node-exporter.sh
```

The first boot requires service account creation and activation inside the guest:

```bash
useradd --system --user-group --no-create-home --shell /usr/sbin/nologin node_exporter
systemctl enable --now node-exporter.service
```

Rollback uses `images/ubuntu-24.04.ext4` as `rootfs` in `lab.json` after stopping the VM.

## Observability

Prometheus metrics, including native histograms, are exposed at `127.0.0.1:9091/metrics`. A sample [scrape configuration](examples/prometheus.yml) is included.

Firecracker streams JSON metrics through a per-VM FIFO: a named pipe that transfers bytes through kernel buffers. The controller continuously reads records, requests a flush every five seconds and accumulates counter deltas. Host process and thread measurements are collected separately.

With `guest_metrics` enabled, `/guest/metrics` proxies Node Exporter over virtio-vsock. The guest runs Node Exporter with `--web.listen-address=vsock://:9100`; the example [systemd service](examples/node-exporter.service) requires `/usr/local/bin/node_exporter` and a dedicated `node_exporter` user in the guest. Prometheus scrapes guest metrics as a separate job. The endpoint returns an error while the guest or exporter is unavailable.

Runtime logs are stored under `run/`, with optional raw metric capture through `raw_metrics`. Prometheus stores historical time series.
