#!/usr/bin/env bash
# Run only while every VM using the source image is stopped.
# Optional arguments: source.ext4 output.ext4 (paths relative to the repository).
set -euo pipefail
umask 077

repo_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd -- "$repo_dir"
if (( $# > 2 )); then
    echo "Usage: bash examples/prepare-node-exporter.sh [source.ext4 [output.ext4]]" >&2
    exit 1
fi
source_image=${1:-images/ubuntu-24.04.ext4}
output_image=${2:-images/ubuntu-24.04-node-exporter.ext4}
version=1.12.1
archive_name="node_exporter-${version}.linux-amd64.tar.gz"
archive="$repo_dir/downloads/$archive_name"
manifest="$repo_dir/downloads/node_exporter-${version}.sha256sums.txt"

for tool in debugfs e2fsck sha256sum tar cp cmp mktemp realpath awk; do
    command -v "$tool" >/dev/null || { echo "Missing command: $tool" >&2; exit 1; }
done
if [[ ! -f "$source_image" || -L "$source_image" ]]; then
    echo "Source must be a regular disk image file, not a device or symlink" >&2
    exit 1
fi
if [[ -e "$output_image" || -L "$output_image" ]]; then
    echo "Refusing to overwrite $output_image" >&2
    exit 1
fi
source_image=$(realpath -- "$source_image")
output_image=$(realpath -m -- "$output_image")
checksum=$(awk -v name="$archive_name" '$2 == name || $2 == "*" name { print $1 }' "$manifest")
if [[ ! "$checksum" =~ ^[[:xdigit:]]{64}$ ]]; then
    echo "Expected exactly one SHA256 entry for $archive_name" >&2
    exit 1
fi
if ! printf '%s  %s\n' "$checksum" "$archive" | sha256sum --check --status; then
    echo "Node Exporter archive checksum verification failed" >&2
    exit 1
fi
e2fsck -fn "$source_image"
source_checksum=$(sha256sum < "$source_image")

stage=$(mktemp -d /tmp/lab-node-exporter.XXXXXX)
image=""
cleanup() {
    rm -rf -- "$stage"
    if [[ -n "$image" ]]; then rm -f -- "$image"; fi
}
trap cleanup EXIT
image=$(mktemp -- "$(dirname -- "$output_image")/.node-exporter-XXXXXX.ext4")
cp --reflink=auto --sparse=always -- "$source_image" "$image"
tar -xOf "$archive" "node_exporter-${version}.linux-amd64/node_exporter" > "$stage/node_exporter"
cp -- "$repo_dir/examples/node-exporter.service" "$stage/node-exporter.service"

# These destinations are paths INSIDE the copied ext4 filesystem.
# Fixed staging filenames avoid interpolating host paths into debugfs commands.
cat > "$stage/commands" <<'COMMANDS'
write node_exporter /usr/local/bin/node_exporter
set_inode_field /usr/local/bin/node_exporter uid 0
set_inode_field /usr/local/bin/node_exporter gid 0
set_inode_field /usr/local/bin/node_exporter mode 0100755
write node-exporter.service /etc/systemd/system/node-exporter.service
set_inode_field /etc/systemd/system/node-exporter.service uid 0
set_inode_field /etc/systemd/system/node-exporter.service gid 0
set_inode_field /etc/systemd/system/node-exporter.service mode 0100644
COMMANDS
(cd -- "$stage" && debugfs -w -f commands "$image")

# debugfs can return success after an individual command fails. Verify the
# actual bytes and metadata before publishing the finished image.
verify_file() {
    local guest_path=$1 expected_mode=$2 expected_file=$3 stat
    debugfs -R "dump $guest_path $stage/check" "$image" > "$stage/check.log" 2>&1
    cmp -- "$expected_file" "$stage/check"
    rm -- "$stage/check"
    stat=$(debugfs -R "stat $guest_path" "$image" 2>/dev/null)
    if [[ ! "$stat" =~ Mode:[[:space:]]+$expected_mode ||
          ! "$stat" =~ User:[[:space:]]+0[[:space:]]+Group:[[:space:]]+0 ]]; then
        echo "Unexpected ownership or mode for $guest_path" >&2
        exit 1
    fi
}
verify_file /usr/local/bin/node_exporter 0755 "$stage/node_exporter"
verify_file /etc/systemd/system/node-exporter.service 0644 "$stage/node-exporter.service"
e2fsck -fn "$image"
if [[ "$source_checksum" != "$(sha256sum < "$source_image")" ]]; then
    echo "Source changed during preparation; refusing to publish the copy" >&2
    exit 1
fi
# Linking publishes the image without overwriting an existing destination.
# The temporary image is on the same filesystem and is removed by cleanup.
ln -- "$image" "$output_image"
printf 'Prepared %s\nOriginal image unchanged. Create the service user and enable Node Exporter inside the guest after boot.\n' "$output_image"
