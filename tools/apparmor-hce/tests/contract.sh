#!/usr/bin/env bash
set -euo pipefail
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
BUILD="$ROOT/build.sh"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
img='registry.example.invalid/hce-builder@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
ok() { "$BUILD" --arch amd64 --builder-image "$img" --output-dir "$tmp/out" >/dev/null 2>&1; return $?; }
expect_fail() { if "$@" >/dev/null 2>&1; then printf 'unexpected success: %s\n' "$*" >&2; exit 1; fi; }
[[ -f "$ROOT/source.lock" ]] || { echo 'missing source lock' >&2; exit 1; }
expect_fail "$BUILD" --arch nope --builder-image "$img" --output-dir "$tmp/out"
expect_fail "$BUILD" --arch amd64 --builder-image registry.example.invalid/hce-builder:latest --output-dir "$tmp/out"
expect_fail "$BUILD" --arch amd64 --output-dir "$tmp/out"
secret_output=$(env APPARMOR_SECRET=sentinel-secret "$BUILD" --arch amd64 --builder-image "$img" --output-dir "$tmp/out" 2>&1 || true)
[[ "$secret_output" != *sentinel-secret* && "$secret_output" != *APPARMOR_SECRET* ]] || { echo 'secret leaked' >&2; exit 1; }
missing_output=$(env APPARMOR_HCE_SOURCE_LOCK="$tmp/no-such-lock" "$BUILD" --arch amd64 --builder-image "$img" --output-dir "$tmp/out" 2>&1 || true)
[[ "$missing_output" == *'source.lock is missing'* ]] || { echo 'missing lock was not rejected' >&2; exit 1; }
# Exercise a positive fixture directly against validate_inputs, without network
# access or changing the checked-in (intentionally unverified) lock.
cp "$ROOT/source.lock" "$tmp/source.lock"
sed -i.bak 's/archive_sha256=UNVERIFIED_REQUIRED_FROM_OFFICIAL_RELEASE/archive_sha256='"$(printf 'b%.0s' {1..64})"'/' "$tmp/source.lock"
sed -i.bak 's/trusted_public_key_fingerprint=.*/trusted_public_key_fingerprint=0123456789ABCDEF0123456789ABCDEF01234567/' "$tmp/source.lock"
sed -i.bak 's/source_revision=.*/source_revision=0123456789abcdef0123456789abcdef01234567/' "$tmp/source.lock"
expect_fail env APPARMOR_HCE_SOURCE_LOCK="$tmp/source.lock" "$BUILD" --arch amd64 --builder-image "$img" --output-dir "$tmp/negative"
sed -i.bak 's/archive_sha256=.*/archive_sha256=bad/' "$tmp/source.lock"
expect_fail env APPARMOR_HCE_SOURCE_LOCK="$tmp/source.lock" "$BUILD" --arch amd64 --builder-image "$img" --output-dir "$tmp/negative"
sed -i.bak 's/archive_sha256=.*/archive_sha256='"$(printf 'b%.0s' {1..64})"'/' "$tmp/source.lock"
sed -i.bak 's/trusted_public_key_fingerprint=.*/trusted_public_key_fingerprint=bad/' "$tmp/source.lock"
expect_fail env APPARMOR_HCE_SOURCE_LOCK="$tmp/source.lock" "$BUILD" --arch amd64 --builder-image "$img" --output-dir "$tmp/negative"
sed -i.bak 's/trusted_public_key_fingerprint=.*/trusted_public_key_fingerprint=0123456789ABCDEF0123456789ABCDEF01234567/' "$tmp/source.lock"
sed -i.bak 's/source_revision=.*/source_revision=bad-revision/' "$tmp/source.lock"
expect_fail env APPARMOR_HCE_SOURCE_LOCK="$tmp/source.lock" "$BUILD" --arch amd64 --builder-image "$img" --output-dir "$tmp/negative"
sed -i.bak 's/source_revision=.*/source_revision=0123456789abcdef0123456789abcdef01234567/' "$tmp/source.lock"
for key in version release_archive_url signature_url archive_sha256 trusted_public_key_fingerprint source_revision; do
  cp "$tmp/source.lock" "$tmp/missing-$key.lock"
  sed -i.bak "/^${key}=/d" "$tmp/missing-$key.lock"
  expect_fail env APPARMOR_HCE_SOURCE_LOCK="$tmp/missing-$key.lock" "$BUILD" --arch amd64 --builder-image "$img" --output-dir "$tmp/negative"
done
cp "$tmp/source.lock" "$tmp/duplicate-version.lock"
printf '%s\n' 'version=4.1.7' >> "$tmp/duplicate-version.lock"
expect_fail env APPARMOR_HCE_SOURCE_LOCK="$tmp/duplicate-version.lock" "$BUILD" --arch amd64 --builder-image "$img" --output-dir "$tmp/negative"
expect_fail "$BUILD" --arch amd64 --builder-image "$img" --output-dir relative-output
cp "$BUILD" "$tmp/build.sh"
sed -i.bak "s#SCRIPT_DIR=\$(CDPATH= cd -- \"\$(dirname -- \"\$0\")\" && pwd)#SCRIPT_DIR=$tmp#" "$tmp/build.sh"
source "$tmp/build.sh"
validate_inputs --arch arm64 --builder-image "$img" --output-dir "$tmp/positive-out" >/dev/null
amd_output=$("$tmp/build.sh" --arch amd64 --builder-image "$img" --output-dir "$tmp/positive-amd" 2>&1 || true)
[[ "$amd_output" == *'build stages are unavailable'* ]] || { echo 'valid amd64 entry path failed before build stage' >&2; exit 1; }
arm_output=$("$tmp/build.sh" --arch arm64 --builder-image "$img" --output-dir "$tmp/positive-arm" 2>&1 || true)
[[ "$arm_output" == *'build stages are unavailable'* ]] || { echo 'valid arm64 entry path failed before build stage' >&2; exit 1; }
echo 'contract checks passed (validation negatives; build remains fail-closed)'
