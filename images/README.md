# Guest kernel and disk

Downloaded these x86_64 artifacts from Firecracker's public test bucket:

```text
https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/20261105-6ea6634994fa-0/x86_64/
```

| Local path | Role |
| --- | --- |
| `vmlinux-6.1.188` | Linux kernel that Firecracker will load into guest RAM |
| `ubuntu-24.04.squashfs` | Original compressed Ubuntu userspace filesystem |
| `rootfs-tree/` | Extracted filesystem contents |
| `ubuntu-24.04.ext4` | Prepared writable guest disk image |

## Preparation completed

1. Extracted SquashFS using `unsquashfs`, skipping extended attributes.
2. Set the extracted tree's ownership to root for the lab image.
3. Used `truncate -s 1G` to create a sparse disk file.
4. Used `sudo mke2fs -t ext4 -F -d images/rootfs-tree images/ubuntu-24.04.ext4` from the repository root to create ext4 and copy in the files. Root access was needed to read restricted source files after the ownership change.

The image has 1 GiB logical disk capacity; sparse allocation means host storage usage can be smaller. This is separate from the configured 256 MiB guest RAM.

## First boot observations

The guest booted successfully with kernel `6.1.188+` and Ubuntu `24.04.5` userspace. `df` showed `/dev/root` mounted at `/`, with 996780 KiB total, 298472 KiB used and 629496 KiB available. Filesystem metadata and reserved blocks account for space unavailable to ordinary files.

The kernel executes in guest RAM; the Ubuntu files remain on the virtual disk and are read as needed. Guest writes can change this image. Disk persistence across a restart has not yet been tested.

Formatting overwrites the target image. Always target the lab file, never a host device under `/dev`. All contents here except this note are ignored by Git.
