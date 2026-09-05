#!/usr/bin/env bash
set -euo pipefail

repo_root="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
tmp_dir="$(mktemp -d)"
cleanup() { rm -rf -- "$tmp_dir"; }
trap cleanup EXIT INT TERM
fail() { printf 'test-workspace-fuse-matrix: %s\n' "$*" >&2; exit 1; }

profiles=(minio-sigv4-path-style-v1 huawei-obs-public-v1 huawei-obs-private-2023-v1)
mkdir -p "$tmp_dir/profiles"
for profile in "${profiles[@]}"; do
  provider=minio
  [[ "$profile" == huawei-obs-* ]] && provider=obs
  {
    printf 'schema_version: 1\n'
    printf 'profile_id: %s\n' "$profile"
    printf 'provider: %s\n' "$provider"
    printf 'enabled: true\n'
    printf 'status: release-verified\n'
    printf 'evidence_sha256: "%064d"\n' 0
  } >"$tmp_dir/profiles/$profile.yaml"
done

cat >"$tmp_dir/preflight" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[[ "${WORKSPACE_FUSE_RUN_INTEGRATION:-}" == 1 ]]
[[ "${WORKSPACE_FUSE_REQUIRE_FAULTS:-}" == 1 ]]
printf '%s %s\n' "$1" "$3" >>"$WORKSPACE_FUSE_TEST_CALLS"
EOF
chmod +x "$tmp_dir/preflight"

WORKSPACE_FUSE_PROFILE_DIR="$tmp_dir/profiles" \
WORKSPACE_FUSE_PREFLIGHT="$tmp_dir/preflight" \
WORKSPACE_FUSE_TEST_CALLS="$tmp_dir/calls" \
  "$repo_root/scripts/workspace-fuse-matrix.sh" >/dev/null
[[ "$(wc -l <"$tmp_dir/calls" | tr -d ' ')" == 6 ]] || fail "matrix did not run six combinations"
for profile in "${profiles[@]}"; do
  grep -Fq "kubernetes $tmp_dir/profiles/$profile.yaml" "$tmp_dir/calls" || fail "missing kubernetes/$profile"
  grep -Fq "docker $tmp_dir/profiles/$profile.yaml" "$tmp_dir/calls" || fail "missing docker/$profile"
done

cat >"$tmp_dir/evidence.json" <<'EOF'
{
  "profile_id": "minio-sigv4-path-style-v1",
  "passed": true,
  "mounter_image_digest": "registry.invalid/mounter@sha256:0000000000000000000000000000000000000000000000000000000000000001",
  "sandbox_image_digest": "registry.invalid/sandbox@sha256:0000000000000000000000000000000000000000000000000000000000000001",
  "service_version": "RELEASE.1",
  "everest_version": "",
  "s3fs_version": "1.95",
  "directory_marker": "trailing-slash-zero-byte",
  "tls_verify": true,
  "options": ["use_path_request_style", "sigv4"],
  "combinations": [
    {"runtime": "kubernetes", "functional": true, "faults": true, "cleanup_confirmed": true},
    {"runtime": "docker", "functional": true, "faults": true, "cleanup_confirmed": true}
  ]
}
EOF
digest=sha256:$(printf '%064d' 1)
"$repo_root/scripts/workspace-fuse-preflight.sh" record-profile \
  --profile-id minio-sigv4-path-style-v1 \
  --image-digest "registry.invalid/mounter@$digest" \
  --sandbox-image-digest "registry.invalid/sandbox@$digest" \
  --service-version RELEASE.1 \
  --s3fs-version 1.95 \
  --directory-marker trailing-slash-zero-byte \
  --tls-verify true \
  --option use_path_request_style \
  --option sigv4 \
  --evidence "$tmp_dir/evidence.json" \
  --output "$tmp_dir/recorded.yaml" >/dev/null
grep -Fq 'enabled: true' "$tmp_dir/recorded.yaml" || fail "recorder did not enable verified profile"
grep -Fq 'provider: minio' "$tmp_dir/recorded.yaml" || fail "recorder omitted provider"
grep -Eq '^evidence_sha256: "[0-9a-f]{64}"$' "$tmp_dir/recorded.yaml" || fail "recorder omitted evidence digest"

printf 'workspace-fuse matrix script tests passed\n'
