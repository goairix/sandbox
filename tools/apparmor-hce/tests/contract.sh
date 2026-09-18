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
CONTAINERFILE="$ROOT/Containerfile"
SPEC="$ROOT/rpm/sandbox-apparmor-parser.spec"
SBIN_CHECK="$ROOT/check-sbin-layout.sh"
[[ -f "$CONTAINERFILE" && -f "$SPEC" && -x "$SBIN_CHECK" ]] || { echo 'missing packaging inputs' >&2; exit 1; }
source "$SBIN_CHECK"
[[ "$(root_join / /sbin)" == /sbin && "$(root_join / /usr/sbin)" == /usr/sbin ]] || { echo 'root path joining is not canonical' >&2; exit 1; }
# Packaging policy is intentionally static: these checks do not require a
# container engine, external packages, or a live HCE builder.
grep -Eq '^ARG HCE_BUILDER_IMAGE$' "$CONTAINERFILE" || { echo 'builder image must be explicit' >&2; exit 1; }
grep -Eq '^FROM \$\{HCE_BUILDER_IMAGE\}$' "$CONTAINERFILE" || { echo 'builder image has an implicit default' >&2; exit 1; }
grep -Eq 'VERSION_ID:-.*2\.0|VERSION_ID.*2\.0' "$CONTAINERFILE" || { echo 'HCE 2.0 check missing' >&2; exit 1; }
grep -Eq '^COPY check-sbin-layout\.sh /usr/local/bin/check-sbin-layout\.sh$' "$CONTAINERFILE" || { echo '/sbin checker COPY missing' >&2; exit 1; }
grep -Eq '^RUN /usr/local/bin/check-sbin-layout\.sh /$' "$CONTAINERFILE" || { echo 'anchored /sbin checker RUN missing' >&2; exit 1; }
grep -Eq 'check-sbin-layout\.sh "\$rootfs" /usr/sbin/apparmor_parser "\$\(uname -m\)"' "$CONTAINERFILE" || { echo 'post-build parser layout check missing' >&2; exit 1; }
schema_line=$(grep -n 'awk -F=.*source.lock' "$CONTAINERFILE" | head -1 | cut -d: -f1)
deps_line=$(grep -n '^RUN dnf install' "$CONTAINERFILE" | head -1 | cut -d: -f1)
early_checker_line=$(grep -n '^RUN /usr/local/bin/check-sbin-layout.sh /$' "$CONTAINERFILE" | head -1 | cut -d: -f1)
(( schema_line < deps_line && deps_line < early_checker_line )) || { echo 'layout checker runs before lock/dependency gates' >&2; exit 1; }
if grep -Eq '! grep -Eq .*(UNVERIFIED|REQUIRED_FROM_)' "$CONTAINERFILE"; then
  echo 'broad source-lock marker grep must not be used' >&2
  exit 1
fi
grep -Eq 'TARGETARCH|uname -m' "$CONTAINERFILE" || { echo 'architecture check missing' >&2; exit 1; }
grep -Eq '^ARG APPARMOR_TRUSTED_KEYRING$' "$CONTAINERFILE" || { echo 'trusted keyring build arg missing' >&2; exit 1; }
grep -Eq 'signature_url=.*source\.lock|sed .*signature_url' "$CONTAINERFILE" || { echo 'locked signature URL is not consumed' >&2; exit 1; }
grep -Eq -- '--no-default-keyring' "$CONTAINERFILE" || { echo 'verification must avoid default keyrings' >&2; exit 1; }
grep -Eq -- '--status-fd' "$CONTAINERFILE" || { echo 'machine-readable signature status missing' >&2; exit 1; }
grep -Eq 'expected_fingerprint|trusted_public_key_fingerprint' "$CONTAINERFILE" || { echo 'locked signer fingerprint is not consumed' >&2; exit 1; }
grep -Eq 'VALIDSIG' "$CONTAINERFILE" || { echo 'signature fingerprint is not checked' >&2; exit 1; }
grep -Eq 'trusted_public_key_fingerprint|source_revision' "$CONTAINERFILE" || { echo 'full lock schema check missing' >&2; exit 1; }
grep -Eq 'git ls-remote.*github\.com/apparmor/apparmor\.git' "$CONTAINERFILE" || { echo 'source revision provenance check missing' >&2; exit 1; }
grep -Eq 'od -An -tx1 -j20 -N4' "$SBIN_CHECK" || { echo 'ELF version field check missing' >&2; exit 1; }
grep -Eq 'od -An -tx1 -j52 -N2' "$SBIN_CHECK" || { echo 'ELF header-size check missing' >&2; exit 1; }
grep -Eq 'make -C libraries/libapparmor|libapparmor' "$CONTAINERFILE" || { echo 'libapparmor build missing' >&2; exit 1; }
grep -Eq 'make -C libraries/libapparmor check|make check' "$CONTAINERFILE" || { echo 'libapparmor tests missing' >&2; exit 1; }
grep -Eq 'rpmbuild -bb' "$CONTAINERFILE" || { echo 'rpmbuild invocation missing' >&2; exit 1; }
grep -Eq '^Source0:[[:space:]]+apparmor-%\{version\}\.tar\.gz$' "$SPEC" || { echo 'source archive metadata missing' >&2; exit 1; }
grep -Eq 'rpmbuild/SOURCES/apparmor-4\.1\.7\.tar\.gz' "$CONTAINERFILE" || { echo 'source archive was not staged for rpmbuild' >&2; exit 1; }
grep -Eq '^%autosetup -n apparmor-%\{version\}$' "$SPEC" || { echo 'source unpack step missing' >&2; exit 1; }
payload_policy_patterns=(
  'systemd' 'apparmor\.service'
  '^%(pre|post)(un|trans)?([[:space:]]|$)|^%(trigger|filetrigger|transfiletrigger)[[:alnum:]_]*([[:space:]]|$)|^%verifyscript([[:space:]]|$)'
  '--nodeps'
  'containerd' '/etc/apparmor\.d'
  'profile[[:space:]]+[A-Za-z0-9_.-]+[[:space:]]*\{'
  'usr\.bin\.' '(^|[[:space:]])local/'
)
payload_policy_scan() {
  local path pattern
  for path in "$@"; do
    for pattern in "${payload_policy_patterns[@]}"; do
      if grep -Ein -- "$pattern" "$path" >/dev/null; then
        return 1
      fi
    done
  done
  return 0
}
if ! payload_policy_scan "$SPEC" "$CONTAINERFILE"; then
  echo 'forbidden host mutation or policy payload in packaging inputs' >&2
  exit 1
fi
# Exercise the shared layout gate with local rootfs fixtures, without
# inspecting or modifying the host's /sbin path.
valid_root="$tmp/valid-root"
mkdir -p "$valid_root/usr/sbin"
ln -s usr/sbin "$valid_root/sbin"
"$SBIN_CHECK" "$valid_root"
elf_source=$(mktemp "$tmp/elf-source.XXXXXX")
dd if=/dev/zero of="$elf_source" bs=64 count=1 2>/dev/null
printf '\177ELF\002\001\001\000' | dd of="$elf_source" bs=1 seek=0 conv=notrunc 2>/dev/null
printf '\267\000' | dd of="$elf_source" bs=1 seek=18 conv=notrunc 2>/dev/null
printf '\001\000\000\000' | dd of="$elf_source" bs=1 seek=20 conv=notrunc 2>/dev/null
printf '\100\000' | dd of="$elf_source" bs=1 seek=52 conv=notrunc 2>/dev/null
cp "$elf_source" "$valid_root/usr/sbin/apparmor_parser"
chmod 0755 "$valid_root/usr/sbin/apparmor_parser"
"$SBIN_CHECK" "$valid_root" /usr/sbin/apparmor_parser aarch64
expect_fail "$SBIN_CHECK" "$valid_root" /usr/sbin/apparmor_parser x86_64
missing_parser_root="$tmp/missing-parser-root"
mkdir -p "$missing_parser_root/usr/sbin"
ln -s usr/sbin "$missing_parser_root/sbin"
expect_fail "$SBIN_CHECK" "$missing_parser_root" /usr/sbin/apparmor_parser aarch64
nonexec_root="$tmp/nonexec-parser-root"
mkdir -p "$nonexec_root/usr/sbin"
ln -s usr/sbin "$nonexec_root/sbin"
cp "$elf_source" "$nonexec_root/usr/sbin/apparmor_parser"
chmod 0644 "$nonexec_root/usr/sbin/apparmor_parser"
expect_fail "$SBIN_CHECK" "$nonexec_root" /usr/sbin/apparmor_parser aarch64
wrong_root="$tmp/wrong-root"
mkdir -p "$wrong_root/usr/sbin" "$wrong_root/usr/bin"
ln -s usr/bin "$wrong_root/sbin"
expect_fail "$SBIN_CHECK" "$wrong_root"
directory_root="$tmp/directory-root"
mkdir -p "$directory_root/usr/sbin" "$directory_root/sbin"
expect_fail "$SBIN_CHECK" "$directory_root"
# Exercise the rejection policy itself with temporary payload fixtures.  This
# stays entirely local and catches accidental weakening of the static scan.
for forbidden in systemd apparmor.service '%post' '%pre' '%postun' '%filetriggerin' '%transfiletriggerun' '%verifyscript' '%trigger' --nodeps containerd /etc/apparmor.d 'profile test {' 'usr.bin.test' local/; do
  fixture="$tmp/forbidden-payload"
  printf '%s\n' "$forbidden" > "$fixture"
  if payload_policy_scan "$fixture"; then
    echo "forbidden payload fixture was not detected: $forbidden" >&2
    exit 1
  fi
done
grep -Eq '^%files$' "$SPEC" || { echo 'spec files section missing' >&2; exit 1; }
grep -Eq '^%license ' "$SPEC" || { echo 'spec license metadata missing' >&2; exit 1; }
grep -Eq '^ExclusiveArch: +x86_64 aarch64$' "$SPEC" || { echo 'spec architecture restriction missing' >&2; exit 1; }
grep -Eq '%\{_sbindir\}/apparmor_parser' "$SPEC" || { echo 'parser payload missing' >&2; exit 1; }
if grep -Eq 'LDFLAGS=.*-Wl,-rpath|sandbox-apparmor-parser/libapparmor\.so|%\{_libdir\}/sandbox-apparmor-parser' "$SPEC" "$CONTAINERFILE"; then
  echo 'dead private shared-library payload or RPATH present' >&2
  exit 1
fi
if grep -Eq '^/(usr/)?(sbin|bin)/apparmor_parser$|^/etc/|^/var/' "$SPEC"; then
  echo 'spec contains an uncontrolled host path' >&2
  exit 1
fi
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
