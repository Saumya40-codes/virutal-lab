# Personal Systems Infrastructure Lab

A hands-on lab for learning Linux virtualization, networking and distributed systems through small experiments on a personal Linux machine.

We start with Firecracker and KVM. Firecracker configures the microVM, loads its kernel and provides virtual devices. KVM provides hardware-assisted guest execution through `/dev/kvm`.

## Progress

- Checked Intel VT-x/EPT support, available memory and user access to `/dev/kvm`.
- Installed Firecracker v1.17.0 in `~/.local/bin`.
- Started its local API and configured 1 vCPU and 256 MiB guest RAM.
- Downloaded Linux 6.1.188 and Ubuntu 24.04 userspace, then prepared a 1 GiB ext4 guest disk.
- Booted the guest and reached its root shell through the serial console, without configuring networking.
- Verified kernel `6.1.188+`, Ubuntu `24.04.5`, 1 available vCPU, 230 MiB usable RAM and the root filesystem mounted at `/`.

Restarting Firecracker clears its API configuration. The disk image persists on the host; persistence across a guest restart has not yet been tested.

## Files

- [downloads](downloads/README.md): Firecracker release archive and extracted executables.
- [images](images/README.md): guest kernel and filesystem preparation.
- [run](run/README.md): local API socket and process lifecycle.

Later experiments may cover networking, multiple guests and distributed workloads. Architecture choices stay open. Commands are executed manually; host changes are discussed first. Generated artifacts stay outside Git.
