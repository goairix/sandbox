#!/bin/sh
set -eu

# Both binaries are official 14.1.1 release assets. The pinned SHA-256 values
# were checked against the corresponding release .sha256 assets.
case "${TARGETARCH:?TARGETARCH is required}" in
    arm64)
        asset_id=191354198
        archive_dir=ripgrep-14.1.1-aarch64-unknown-linux-gnu
        expected=c827481c4ff4ea10c9dc7a4022c8de5db34a5737cb74484d62eb94a95841ab2f
        ;;
    amd64)
        asset_id=191354314
        archive_dir=ripgrep-14.1.1-x86_64-unknown-linux-musl
        expected=4cf9f2741e6c465ffdb7c26f38056a59e2a2544b51f7cc128ef28337eeae4d8e
        ;;
    *)
        echo "unsupported ripgrep architecture" >&2
        exit 1
        ;;
esac

archive=/tmp/ripgrep-14.1.1.tar.gz
curl -fsSL --retry 3 --max-time 90 -H 'Accept: application/octet-stream' \
    "https://api.github.com/repos/BurntSushi/ripgrep/releases/assets/${asset_id}" -o "$archive"
printf '%s  %s\n' "$expected" "$archive" | sha256sum -c -
tar -xzf "$archive" -C /tmp "$archive_dir/rg"
install -m 0755 "/tmp/$archive_dir/rg" /out/rg
rm -rf "$archive" "/tmp/$archive_dir"
