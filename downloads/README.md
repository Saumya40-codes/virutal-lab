# Firecracker download

Downloaded the official x86_64 [v1.17.0 release](https://github.com/firecracker-microvm/firecracker/releases/tag/v1.17.0), checked its SHA-256 digest and extracted the archive here.

```text
06094a1108ae9e82aa4c23a775aa92758f53f1175d422270d9d6162cb9ade558
```

Copied the Firecracker executable to `~/.local/bin/firecracker` with executable permissions. `firecracker --version` confirmed it runs through the user's PATH, without a system-wide installation.

The archive and extracted release are local artifacts, ignored by Git.
