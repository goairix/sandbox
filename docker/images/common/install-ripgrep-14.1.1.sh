#!/bin/sh
set -eu

# Both binaries are official 14.1.1 release assets. The pinned SHA-256 values
# were checked against the corresponding release .sha256 assets.
case "${TARGETARCH:?TARGETARCH is required}" in
    arm64)
        archive_dir=ripgrep-14.1.1-aarch64-unknown-linux-gnu
        expected=c827481c4ff4ea10c9dc7a4022c8de5db34a5737cb74484d62eb94a95841ab2f
        ;;
    amd64)
        archive_dir=ripgrep-14.1.1-x86_64-unknown-linux-musl
        expected=4cf9f2741e6c465ffdb7c26f38056a59e2a2544b51f7cc128ef28337eeae4d8e
        ;;
    *)
        echo "unsupported ripgrep architecture" >&2
        exit 1
        ;;
esac

# Use public release URLs rather than the rate-limited GitHub asset API.
# Internal mirrors must serve the exact official archives; the pins below
# remain authoritative regardless of the download host.
base_url=${RIPGREP_DOWNLOAD_BASE_URL:-https://github.com/BurntSushi/ripgrep/releases/download/14.1.1}
archive=/tmp/ripgrep-14.1.1.tar.gz
if curl -fsSL --retry 3 --max-time 90 \
    "${base_url%/}/${archive_dir}.tar.gz" -o "$archive"; then
    :
else
    status=$?
    printf 'ripgrep 14.1.1 download failed (%s); set RIPGREP_DOWNLOAD_BASE_URL to an accessible mirror of the official release archives\n' "$archive_dir" >&2
    rm -f "$archive"
    exit "$status"
fi
printf '%s  %s\n' "$expected" "$archive" | sha256sum -c -
tar -xzf "$archive" -C /tmp "$archive_dir/rg"
install -m 0755 "/tmp/$archive_dir/rg" /out/rg
rm -rf "$archive" "/tmp/$archive_dir"
