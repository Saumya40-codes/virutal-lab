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

Observed instance state `Not started` and configuration response `204`. No guest kernel or disk has been attached yet.

Ctrl+C stops the process. Configuration must be reapplied after restarting. A stale socket path may remain: check for a listener with `ss -xlpn` and remove only the stale `run/firecracker.socket` when no process is listening there. Runtime files are ignored by Git.
