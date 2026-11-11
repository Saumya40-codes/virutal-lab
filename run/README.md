# Firecracker runtime

From the repository root, started Firecracker in a foreground terminal:

```bash
firecracker --api-sock "$PWD/run/firecracker.socket"
```

The pathname Unix socket carries HTTP requests between local processes. It is a management endpoint, not a TCP listener on port 80 or a guest network interface.

In a second terminal at the repository root:

```bash
curl --unix-socket "$PWD/run/firecracker.socket" http://localhost/

curl -i --unix-socket "$PWD/run/firecracker.socket" \
  -X PUT http://localhost/machine-config \
  -H 'Content-Type: application/json' \
  -d '{"vcpu_count":1,"mem_size_mib":256}'
```

Initially observed instance state `Not started` and configuration response `204`.

## First boot completed

With that same Firecracker process running, configured the kernel and writable disk, then started the guest:

```bash
curl -i --unix-socket "$PWD/run/firecracker.socket" \
  -X PUT http://localhost/boot-source \
  -H 'Content-Type: application/json' \
  -d "{\"kernel_image_path\":\"$PWD/images/vmlinux-6.1.188\",\"boot_args\":\"console=ttyS0 reboot=k panic=0 root=/dev/vda rw\"}"

curl -i --unix-socket "$PWD/run/firecracker.socket" \
  -X PUT http://localhost/drives/rootfs \
  -H 'Content-Type: application/json' \
  -d "{\"drive_id\":\"rootfs\",\"path_on_host\":\"$PWD/images/ubuntu-24.04.ext4\",\"is_root_device\":true,\"is_read_only\":false}"

curl -i --unix-socket "$PWD/run/firecracker.socket" \
  -X PUT http://localhost/actions \
  -H 'Content-Type: application/json' \
  -d '{"action_type":"InstanceStart"}'
```

All three requests returned `204`. The kernel booted, mounted the root filesystem and started Ubuntu userspace. Reached a root shell in the terminal running Firecracker. No guest networking was configured.

The guest kernel interprets `console=ttyS0` to use the emulated serial console, `root=/dev/vda rw` to mount the root disk read/write, `panic=0` to wait on panic and `reboot=k` to select the reset mechanism.

Verified inside the guest:

| Command | Observation |
| --- | --- |
| `uname -r` | `6.1.188+` |
| `cat /etc/os-release` | Ubuntu `24.04.5 LTS` |
| `free -h` | 230 MiB usable RAM, 197 MiB available at observation, no swap |
| `df` | Root filesystem mounted at `/` |
| `nproc` | 1 available vCPU |

Configured memory includes kernel reservations, so usable RAM is below 256 MiB. The vCPU runs through a host thread scheduled onto host logical CPUs; it does not reserve a physical core. Guest virtual addresses translate through guest page tables to guest physical addresses, then through EPT to host physical memory.

Ctrl+C stops the process, abruptly terminating a running guest. Prefer a clean guest shutdown to avoid filesystem damage; shutdown behavior has not yet been tested in this lab. Configuration must be reapplied after restarting. A stale socket path may remain: check for a listener with `ss -xlpn` and remove only the stale `run/firecracker.socket` when no process is listening there. Runtime files are ignored by Git.
