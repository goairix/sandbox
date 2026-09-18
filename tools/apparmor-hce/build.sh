#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)

die() { printf 'error: %s\n' "$1" >&2; exit 2; }

validate_inputs() {
  local arch='' builder_image='' output_dir='' source_lock="${APPARMOR_HCE_SOURCE_LOCK:-$SCRIPT_DIR/source.lock}"
  while (($#)); do
    case "$1" in
      --arch) (($# >= 2)) || die '--arch requires a value'; arch=$2; shift 2 ;;
      --builder-image) (($# >= 2)) || die '--builder-image requires a value'; builder_image=$2; shift 2 ;;
      --output-dir) (($# >= 2)) || die '--output-dir requires a value'; output_dir=$2; shift 2 ;;
      *) die "unknown argument: $1" ;;
    esac
  done
  [[ -n "$arch" ]] || die '--arch is required'
  [[ "$arch" == amd64 || "$arch" == arm64 ]] || die '--arch must be amd64 or arm64'
  case "$arch" in amd64) target_arch=x86_64 ;; arm64) target_arch=aarch64 ;; esac
  [[ -n "$builder_image" ]] || die '--builder-image is required'
  [[ "$builder_image" =~ ^[^[:space:]@]+@sha256:[0-9a-fA-F]{64}$ ]] || die '--builder-image must be digest-pinned'
  [[ -n "$output_dir" ]] || die '--output-dir is required'
  [[ "$output_dir" == /* ]] || die '--output-dir must be absolute'
  if [[ -e "$output_dir" && ! -d "$output_dir" ]]; then die '--output-dir is not a directory'; fi
  mkdir -p -- "$output_dir"
  [[ -f "$source_lock" ]] || die 'source.lock is missing'
  awk -F= '
    /^[[:space:]]*#/ || /^[[:space:]]*$/ { next }
    NF != 2 || $1 !~ /^(version|release_archive_url|archive_sha256|signature_url|trusted_public_key_fingerprint|source_revision)$/ { exit 2 }
    { count[$1]++ }
    END { for (k in count) if (count[k] != 1) exit 3; if (!count["version"] || !count["release_archive_url"] || !count["archive_sha256"] || !count["signature_url"] || !count["trusted_public_key_fingerprint"] || !count["source_revision"]) exit 4 }
  ' "$source_lock" || die 'source.lock has malformed, duplicate, unknown, or missing keys'
  grep -q '^version=4\.1\.7$' "$source_lock" || die 'source.lock version is invalid'
  local archive_url signature_url archive_sha fingerprint revision
  archive_url=$(sed -n 's/^release_archive_url=//p' "$source_lock")
  signature_url=$(sed -n 's/^signature_url=//p' "$source_lock")
  archive_sha=$(sed -n 's/^archive_sha256=//p' "$source_lock")
  fingerprint=$(sed -n 's/^trusted_public_key_fingerprint=//p' "$source_lock")
  revision=$(sed -n 's/^source_revision=//p' "$source_lock")
  [[ "$archive_url" == https://github.com/apparmor/apparmor/releases/download/v4.1.7/apparmor-4.1.7.tar.gz ]] || die 'release_archive_url is not the locked AppArmor 4.1.7 archive'
  [[ "$signature_url" == https://github.com/apparmor/apparmor/releases/download/v4.1.7/apparmor-4.1.7.tar.gz.asc ]] || die 'signature_url is not the matching locked archive signature'
  [[ "$archive_sha" =~ ^[0-9a-fA-F]{64}$ ]] || die 'archive_sha256 must be 64 hexadecimal characters'
  [[ "$fingerprint" =~ ^[0-9a-fA-F]{40}$ ]] || die 'trusted_public_key_fingerprint must be 40 hexadecimal characters'
  [[ "$revision" =~ ^[0-9a-fA-F]{40}$ ]] || die 'source_revision must be a 40-hex signed revision'
  printf 'validated arch=%s target_arch=%s output_dir=%s\n' "$arch" "$target_arch" "$output_dir"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  validate_inputs "$@"
  die 'build stages are unavailable until the verified source contract is populated'
fi
