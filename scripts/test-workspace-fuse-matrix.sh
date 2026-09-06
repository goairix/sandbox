#!/usr/bin/env bash
set -euo pipefail

repo_root="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
tmp_dir="$(mktemp -d)"
cleanup() { rm -rf -- "$tmp_dir"; }
trap cleanup EXIT INT TERM
fail() { printf 'test-workspace-fuse-matrix: %s\n' "$*" >&2; exit 1; }

profiles=(minio-sigv4-path-style-v1 huawei-obs-public-v1 huawei-obs-private-2023-v1)
common_mounter="registry.invalid/mounter@sha256:$(printf '1%.0s' {1..64})"
common_docker="registry.invalid/docker@sha256:$(printf '2%.0s' {1..64})"
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
    printf 'mounter_image_digest: %s\n' "$common_mounter"
    printf 'docker_image_digest: %s\n' "$common_docker"
  } >"$tmp_dir/profiles/$profile.yaml"
done

cat >"$tmp_dir/preflight" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[[ "${WORKSPACE_FUSE_RUN_INTEGRATION:-}" == 1 ]]
[[ "${WORKSPACE_FUSE_REQUIRE_FAULTS:-}" == 1 ]]
[[ "${WORKSPACE_FUSE_REQUIRE_OBJECT_COUNTER:-}" == 1 ]]
printf '%s %s\n' "$1" "$3" >>"$WORKSPACE_FUSE_TEST_CALLS"
printf '{"passed":true}\n' >"$WORKSPACE_FUSE_EVIDENCE_OUTPUT"
EOF
chmod +x "$tmp_dir/preflight"

cp "$tmp_dir/profiles/huawei-obs-private-2023-v1.yaml" "$tmp_dir/private-profile.yaml"
sed 's#registry.invalid/mounter@sha256:1\{64\}#registry.invalid/mounter@sha256:3333333333333333333333333333333333333333333333333333333333333333#' \
  "$tmp_dir/private-profile.yaml" >"$tmp_dir/profiles/huawei-obs-private-2023-v1.yaml"
if WORKSPACE_FUSE_PROFILE_DIR="$tmp_dir/profiles" \
  WORKSPACE_FUSE_EVIDENCE_DIR="$tmp_dir/evidence" \
  WORKSPACE_FUSE_PREFLIGHT="$tmp_dir/preflight" \
  WORKSPACE_FUSE_TEST_CALLS="$tmp_dir/calls" \
  "$repo_root/scripts/workspace-fuse-matrix.sh" >/dev/null 2>&1; then
  fail "matrix accepted backend-specific mounter image digests"
fi
cp "$tmp_dir/private-profile.yaml" "$tmp_dir/profiles/huawei-obs-private-2023-v1.yaml"
: >"$tmp_dir/calls"
rm -rf "$tmp_dir/evidence"

WORKSPACE_FUSE_PROFILE_DIR="$tmp_dir/profiles" \
WORKSPACE_FUSE_EVIDENCE_DIR="$tmp_dir/evidence" \
WORKSPACE_FUSE_PREFLIGHT="$tmp_dir/preflight" \
WORKSPACE_FUSE_TEST_CALLS="$tmp_dir/calls" \
  "$repo_root/scripts/workspace-fuse-matrix.sh" >/dev/null
[[ "$(wc -l <"$tmp_dir/calls" | tr -d ' ')" == 6 ]] || fail "matrix did not run six combinations"
for profile in "${profiles[@]}"; do
  grep -Fq "kubernetes $tmp_dir/profiles/$profile.yaml" "$tmp_dir/calls" || fail "missing kubernetes/$profile"
  grep -Fq "docker $tmp_dir/profiles/$profile.yaml" "$tmp_dir/calls" || fail "missing docker/$profile"
done
[[ "$(find "$tmp_dir/evidence" -type f | wc -l | tr -d ' ')" == 6 ]] || fail "matrix did not retain six API evidence files"

cat >"$tmp_dir/evidence.json" <<'EOF'
{
  "profile_id": "minio-sigv4-path-style-v1",
  "preset": "minio",
  "passed": true,
  "mounter_image_digest": "registry.invalid/mounter@sha256:0000000000000000000000000000000000000000000000000000000000000001",
  "docker_image_digest": "registry.invalid/docker@sha256:0000000000000000000000000000000000000000000000000000000000000001",
  "sandbox_image_digest": "registry.invalid/sandbox@sha256:0000000000000000000000000000000000000000000000000000000000000001",
  "service_version": "RELEASE.1",
  "everest_version": "",
  "s3fs_version": "1.95",
  "directory_marker": "trailing-slash-zero-byte",
  "tls_verify": true,
  "options": ["use_path_request_style", "sigv4"],
  "combinations": [
    {"runtime": "kubernetes", "functional": true, "faults": true, "cleanup_confirmed": true, "api_paths": {"ephemeral_no_workspace": true, "ephemeral_sync_workspace": true, "persistent_sync_workspace": true, "ephemeral_fuse_workspace": true, "persistent_fuse_workspace": true, "same_prefix_sync_fuse_conflict": true, "different_prefix_sync_fuse_concurrency": true}},
    {"runtime": "docker", "functional": true, "faults": true, "cleanup_confirmed": true, "api_paths": {"ephemeral_no_workspace": true, "ephemeral_sync_workspace": true, "persistent_sync_workspace": true, "ephemeral_fuse_workspace": true, "persistent_fuse_workspace": true, "same_prefix_sync_fuse_conflict": true, "different_prefix_sync_fuse_concurrency": true}}
  ]
}
EOF
digest=sha256:$(printf '%064d' 1)
"$repo_root/scripts/workspace-fuse-preflight.sh" record-profile \
  --profile-id minio-sigv4-path-style-v1 \
  --image-digest "registry.invalid/mounter@$digest" \
  --docker-image-digest "registry.invalid/docker@$digest" \
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
grep -Fq "docker_image_digest: registry.invalid/docker@$digest" "$tmp_dir/recorded.yaml" || fail "recorder omitted common Docker FUSE image"
grep -Eq '^evidence_sha256: "[0-9a-f]{64}"$' "$tmp_dir/recorded.yaml" || fail "recorder omitted evidence digest"

python3 - "$tmp_dir/evidence.json" "$tmp_dir/incomplete-evidence.json" <<'PY'
import json, sys
with open(sys.argv[1], encoding="utf-8") as stream:
    evidence = json.load(stream)
del evidence["combinations"][0]["api_paths"]["ephemeral_no_workspace"]
with open(sys.argv[2], "w", encoding="utf-8") as stream:
    json.dump(evidence, stream)
PY
if "$repo_root/scripts/workspace-fuse-preflight.sh" record-profile \
  --profile-id minio-sigv4-path-style-v1 \
  --image-digest "registry.invalid/mounter@$digest" \
  --docker-image-digest "registry.invalid/docker@$digest" \
  --sandbox-image-digest "registry.invalid/sandbox@$digest" \
  --service-version RELEASE.1 \
  --s3fs-version 1.95 \
  --directory-marker trailing-slash-zero-byte \
  --tls-verify true \
  --option use_path_request_style \
  --option sigv4 \
  --evidence "$tmp_dir/incomplete-evidence.json" \
  --output "$tmp_dir/incomplete-recorded.yaml" >/dev/null 2>&1; then
  fail "recorder accepted incomplete API path evidence"
fi

for api_path in \
  ephemeral_no_workspace \
  ephemeral_sync_workspace \
  persistent_sync_workspace \
  ephemeral_fuse_workspace \
  persistent_fuse_workspace \
  same_prefix_sync_fuse_conflict \
  different_prefix_sync_fuse_concurrency; do
  grep -Fq "\"$api_path\"" "$repo_root/test/integration/workspacefuse/workspace_fuse_test.go" \
    || fail "Go integration suite omits $api_path"
  grep -Fq "\"$api_path\"" "$repo_root/scripts/workspace-fuse-preflight.sh" \
    || fail "preflight evidence validator omits $api_path"
done

printf 'workspace-fuse matrix script tests passed\n'
