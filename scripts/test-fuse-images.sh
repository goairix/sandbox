#!/usr/bin/env bash
set -euo pipefail

repo_root="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
verify="$repo_root/scripts/verify-fuse-image.sh"
real_docker="$(command -v docker || true)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

expect_failure() {
  if "$@" >"$tmp/stdout" 2>"$tmp/stderr"; then
    fail "command unexpectedly succeeded: $*"
  fi
}

expect_success() {
  if ! "$@" >"$tmp/stdout" 2>"$tmp/stderr"; then
    sed -n '1,120p' "$tmp/stderr" >&2
    fail "command failed: $*"
  fi
}

mkdir -p "$tmp/bin"
cat >"$tmp/bin/docker" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${MOCK_DOCKER_LOG:?}"

case "$*" in
  *'health prepared --release-check-image'*)
    test "${MOCK_PROFILE_STATUS:-blocked-pending-flush-spike}" = release-verified
    ;;
  *)
    exit 0
    ;;
esac
MOCK
chmod +x "$tmp/bin/docker"

digest="registry.example.com/sandbox@sha256:$(printf 'a%.0s' {1..64})"
fuse_digest="registry.example.com/mounter@sha256:$(printf 'b%.0s' {1..64})"
export PATH="$tmp/bin:$PATH"
export MOCK_DOCKER_LOG="$tmp/docker.log"

# RED/GREEN behavior tests for the executable verifier.
expect_failure env FUSE_IMAGE=repo/mounter:latest SANDBOX_IMAGE="$digest" \
  "$verify" kubernetes package-check
test ! -s "$tmp/docker.log" || fail "invalid digest reached docker"

: >"$tmp/docker.log"
expect_failure env FUSE_IMAGE="repo/mounter@sha256:abc" SANDBOX_IMAGE="$digest" \
  "$verify" kubernetes package-check
test ! -s "$tmp/docker.log" || fail "short digest reached docker"

: >"$tmp/docker.log"
MOCK_PROFILE_STATUS=blocked-pending-flush-spike expect_success env \
  FUSE_IMAGE="$fuse_digest" SANDBOX_IMAGE="$digest" \
  "$verify" kubernetes package-check
grep -Fq -- '--entrypoint /usr/local/bin/workspace-mounter' "$tmp/docker.log" || fail "mounter self-check missing"
grep -Fq -- '--user 1000:1000 --entrypoint /usr/local/bin/workspace-probe' "$tmp/docker.log" || fail "UID 1000 probe self-check missing"
grep -Fq -- 'ordinary sandbox contract' "$tmp/docker.log" || fail "ordinary sandbox negative contract missing"
grep -Fq -- 'image package contract' "$tmp/docker.log" || fail "mounter package contract missing"

expect_success env MOCK_PROFILE_STATUS=release-verified \
  FUSE_IMAGE="$fuse_digest" SANDBOX_IMAGE="$digest" \
  "$verify" kubernetes release-check
grep -Fq -- 'health prepared --release-check-image' "$tmp/docker.log" || fail "Go release gate missing"

: >"$tmp/docker.log"
MOCK_PROFILE_STATUS=blocked-pending-provider-spike expect_success env SANDBOX_IMAGE="$digest" \
  "$verify" docker package-check
grep -Fq -- '--entrypoint /usr/local/bin/workspace-mounter' "$tmp/docker.log" || fail "docker mounter self-check missing"
grep -Fq -- '--user 1000:1000 --entrypoint /usr/local/bin/workspace-probe' "$tmp/docker.log" || fail "docker UID 1000 probe self-check missing"
grep -Fq -- 'image package contract' "$tmp/docker.log" || fail "docker package contract missing"

expect_success env MOCK_PROFILE_STATUS=release-verified SANDBOX_IMAGE="$digest" \
  "$verify" docker release-check

expect_failure env SANDBOX_IMAGE="$digest" \
  "$verify" invalid-runtime package-check
expect_failure env SANDBOX_IMAGE="$digest" \
  "$verify" docker invalid-check
expect_failure env SANDBOX_IMAGE="$digest" \
  "$verify" docker package-check unexpected-argument

# Static image contracts. These intentionally avoid builds so they also run in
# CI workers without a Docker daemon.
mounter="$repo_root/docker/images/workspace-mounter/Dockerfile"
fuse="$repo_root/docker/images/sandbox-fuse/Dockerfile"
ordinary="$repo_root/docker/images/sandbox/Dockerfile"
ordinary_ignore="$repo_root/docker/images/sandbox/Dockerfile.dockerignore"
compose="$repo_root/docker/docker-compose.yml"
compose_env="$repo_root/docker/.env.example"
operator_config="$repo_root/configs/config.yaml"

if [[ -n "$real_docker" ]]; then
  compose_json="$("$real_docker" compose --env-file "$repo_root/docker/.env.example" -f "$compose" config --format json)"
  python3 -c '
import json, sys
environment = json.load(sys.stdin)["services"]["sandbox-api"]["environment"]
required = {
    "SANDBOX_WORKSPACE_DEFAULT_MOUNT_MODE",
    "SANDBOX_WORKSPACE_ENABLED_MOUNT_MODES",
    "SANDBOX_WORKSPACE_BACKEND_PRESET",
    "SANDBOX_WORKSPACE_BACKEND_MOUNTER_IMAGE",
    "SANDBOX_WORKSPACE_BACKEND_DOCKER_IMAGE",
}
missing = sorted(required.difference(environment))
if missing:
    raise SystemExit("Compose omits hybrid backend environment: " + ", ".join(missing))
legacy = sorted(key for key in environment if key.startswith("SANDBOX_WORKSPACE_PROVIDERS_"))
if legacy:
    raise SystemExit("Compose still exports legacy provider maps: " + ", ".join(legacy))
' <<<"$compose_json"
fi

grep -Fq 'SANDBOX_WORKSPACE_DEFAULT_MOUNT_MODE=${WORKSPACE_DEFAULT_MOUNT_MODE:-sync}' "$compose" || fail "Compose omits the default mount mode"
grep -Fq 'SANDBOX_WORKSPACE_ENABLED_MOUNT_MODES=${WORKSPACE_ENABLED_MOUNT_MODES:-sync}' "$compose" || fail "Compose omits enabled mount modes"
grep -Fq 'SANDBOX_WORKSPACE_BACKEND_PRESET=${STORAGE_PRESET:-minio}' "$compose" || fail "Compose omits the storage preset"
grep -Fq 'SANDBOX_WORKSPACE_BACKEND_MOUNTER_IMAGE=${FUSE_MOUNTER_IMAGE:-}' "$compose" || fail "Compose omits the common mounter image"
grep -Fq 'SANDBOX_WORKSPACE_BACKEND_DOCKER_IMAGE=${FUSE_SANDBOX_IMAGE:-}' "$compose" || fail "Compose omits the common Docker FUSE image"
! grep -Fq 'SANDBOX_WORKSPACE_PROVIDERS_MINIO_' "$compose" || fail "Compose still exports the MinIO provider map"
! grep -Fq 'SANDBOX_WORKSPACE_PROVIDERS_OBS_' "$compose" || fail "Compose still exports the OBS provider map"
! grep -Fq 'SANDBOX_STORAGE_FILESYSTEM_ACCESS_KEY=' "$compose" || fail "Compose exposes the access key in process environment"
! grep -Fq 'SANDBOX_STORAGE_FILESYSTEM_SECRET_KEY=' "$compose" || fail "Compose exposes the secret key in process environment"
grep -Fq 'export SANDBOX_STORAGE_FILESYSTEM_PROVIDER=minio' "$compose" || fail "Compose does not derive the MinIO provider"
grep -Fq 'export SANDBOX_WORKSPACE_BACKEND_PROFILE=huawei-obs-public-v1' "$compose" || fail "Compose does not derive the public OBS profile"
grep -Fq 'export SANDBOX_WORKSPACE_BACKEND_PROFILE=huawei-obs-private-2023-v1' "$compose" || fail "Compose does not derive the private OBS profile"
grep -Fq '*,fuse,*)' "$compose" || fail "Compose image helper does not gate common FUSE image pulls"
grep -Fxq 'STORAGE_PRESET=minio' "$compose_env" || fail "example environment omits its single preset selector"
! grep -Eq '^(STORAGE_PROVIDER|WORKSPACE_PROFILE)=' "$compose_env" || fail "example environment permits preset mapping drift"
grep -Fq 'enabled_mount_modes: ["sync"]' "$operator_config" || fail "repository config does not default execution requests to sync"
! grep -Eq '^  providers:' "$operator_config" || fail "repository config still declares provider maps"

for file in "$mounter" "$fuse"; do
  test -f "$file" || fail "missing $file"
  grep -Eq '^ARG BASE_IMAGE$' "$file" || fail "$file does not require BASE_IMAGE"
  grep -Fq '@sha256:' "$file" || fail "$file does not enforce a digest base"
  grep -Eq '^ARG S3FS_PACKAGE_URL$' "$file" || fail "$file does not require S3FS_PACKAGE_URL"
  grep -Eq '^ARG S3FS_PACKAGE_SHA256$' "$file" || fail "$file does not require S3FS_PACKAGE_SHA256"
  grep -Fq 'sha256sum -c' "$file" || fail "$file does not verify the s3fs artifact"
  grep -Fq 'https://*@*|*\?*|*\#*' "$file" || fail "$file allows credentialed or mutable artifact URLs"
  grep -Fq '/workspace' "$file" || fail "$file has no workspace anchor"
  grep -Fq '/run/s3fs' "$file" || fail "$file has no supervisor run directory"
  grep -Fq '/var/cache/s3fs/tmp' "$file" || fail "$file has no bounded-cache path"
  grep -Fq 'find / -xdev -type f' "$file" || fail "$file does not remove setuid/setgid files"
  ! grep -Eiq '(access.?key|secret.?key|passwd-s3fs).*=|AKIA[0-9A-Z]+' "$file" || fail "$file may embed credentials"
done

grep -Fq 'setuid/setgid files are forbidden' "$verify" || fail "runtime image verification does not reject setuid/setgid files"

grep -Fq 'COPY workspace-mounter /usr/local/bin/workspace-mounter' "$mounter" || fail "mounter binary missing"
! grep -Fq 'workspace-probe' "$mounter" || fail "Kubernetes mounter image must not contain workspace-probe"
grep -Fq 'COPY workspace-mounter /usr/local/bin/workspace-mounter' "$fuse" || fail "special image mounter missing"
grep -Fq 'COPY workspace-probe /usr/local/bin/workspace-probe' "$fuse" || fail "special image probe missing"
grep -Fq 'ENTRYPOINT ["/usr/local/bin/workspace-mounter", "supervise"]' "$fuse" || fail "special image supervisor entrypoint missing"
grep -Fq 'command -v python3' "$fuse" || fail "special image does not enforce Python tooling in its base"
grep -Fq 'command -v node' "$fuse" || fail "special image does not enforce Node.js tooling in its base"
grep -Fq 'test "$(id -u sandbox)" = 1000' "$fuse" || fail "special image does not enforce the sandbox UID"

! grep -Eq '(workspace-mounter|/usr/bin/s3fs|S3FS_PACKAGE)' "$ordinary" || fail "ordinary image contains privileged FUSE payload"
grep -Fq 'FROM ${WORKSPACE_PROBE_BUILDER} AS workspace-probe-builder' "$ordinary" || fail "ordinary image does not build the probe from source"
grep -Fq 'COPY --from=workspace-probe-builder /out/workspace-probe /usr/local/bin/workspace-probe' "$ordinary" || fail "ordinary image probe stage is not wired"
grep -Fq '../:/repo:ro' "$compose" || fail "Compose does not expose the repository build context"
grep -Fq 'docker build -f /repo/docker/images/sandbox/Dockerfile -t sandbox:latest /repo' "$compose" || fail "Compose still uses the probe-less sandbox context"
test -f "$ordinary_ignore" || fail "ordinary root-context build has no Dockerfile-specific ignore policy"
grep -Fxq '**' "$ordinary_ignore" || fail "ordinary build context is not deny-by-default"
grep -Fxq '!cmd/workspace-probe/**' "$ordinary_ignore" || fail "ordinary build context omits workspace-probe source"
grep -Fxq '!internal/workspaceprobe/**' "$ordinary_ignore" || fail "ordinary build context omits workspaceprobe package"

profile_bundle="$repo_root/docker/images/workspace-mounter/profile-bundle.json"
test -f "$profile_bundle" || fail "missing common profile bundle"
for profile_id in \
  minio-sigv4-path-style-v1 \
  huawei-obs-public-v1 \
  huawei-obs-private-2023-v1; do
  test "$(grep -Fc '"id": "'"$profile_id"'"' "$profile_bundle")" -eq 1 \
    || fail "$profile_bundle does not contain exactly one $profile_id descriptor"
done
test "$(grep -Fc '"durable_flush": "verified"' "$profile_bundle")" -eq 3 \
  || fail "$profile_bundle has a non-releaseable durability profile"
test "$(grep -Fc '"mount_parameters": "verified"' "$profile_bundle")" -eq 3 \
  || fail "$profile_bundle has a non-releaseable mount profile"
test "$(grep -Fc '0000000000000000000000000000000000000000000000000000000000000000' "$profile_bundle")" -eq 1 \
  || fail "$profile_bundle lacks the unique build-time s3fs hash placeholder"

! grep -Eiq '(no_check_certificate|ssl_verify_hostname|compat_dir|support_compat_dir|use_path_request_style|"options"|"extra_args"|"tls_required": false)' "$profile_bundle" \
  || fail "$profile_bundle contains executable or TLS-weakening options"

for file in "$mounter" "$fuse"; do
  grep -Fq 'PROFILE_BUNDLE=profile-bundle.json' "$file" || fail "$file does not use the common bundle"
  ! grep -Eq 'PROFILE_(ID|MANIFEST)|imageProfileID|profile\.json' "$file" || fail "$file still binds a backend-specific profile"
done

printf 'fuse image contract tests: PASS\n'
