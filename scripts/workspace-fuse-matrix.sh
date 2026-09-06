#!/usr/bin/env bash
set -euo pipefail

repo_root="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
profile_dir="${WORKSPACE_FUSE_PROFILE_DIR:-$repo_root/testdata/fuse/profiles}"
preflight="${WORKSPACE_FUSE_PREFLIGHT:-$repo_root/scripts/workspace-fuse-preflight.sh}"
profiles=(minio-sigv4-path-style-v1 huawei-obs-public-v1 huawei-obs-private-2023-v1)
runtimes=(kubernetes docker)
evidence_dir="${WORKSPACE_FUSE_EVIDENCE_DIR:-$(dirname "$profile_dir")/evidence}"
mkdir -p "$evidence_dir"
enabled_count=0
skipped_count=0
common_mounter_digest=""
common_docker_digest=""

profile_value() {
  python3 - "$1" "$2" <<'PY'
import sys, yaml
doc = yaml.safe_load(open(sys.argv[1], encoding="utf-8")) or {}
value = doc.get(sys.argv[2], "")
print(str(value).lower() if isinstance(value, bool) else value)
PY
}

for profile_id in "${profiles[@]}"; do
  profile_file="$profile_dir/$profile_id.yaml"
  [[ -f "$profile_file" ]] || {
    printf 'matrix: required profile report is missing: %s\n' "$profile_file" >&2
    exit 1
  }
  [[ "$(profile_value "$profile_file" profile_id)" == "$profile_id" ]] || {
    printf 'matrix: profile ID does not match filename: %s\n' "$profile_file" >&2
    exit 1
  }
  if [[ "$(profile_value "$profile_file" enabled)" != "true" ]]; then
    printf 'SKIP profile=%s reason=%s\n' "$profile_id" "$(profile_value "$profile_file" blocked_reason)"
    skipped_count=$((skipped_count + 1))
    continue
  fi
  [[ "$(profile_value "$profile_file" status)" == "release-verified" ]] || {
    printf 'matrix: enabled profile is not release-verified: %s\n' "$profile_id" >&2
    exit 1
  }
  [[ "$(profile_value "$profile_file" evidence_sha256)" =~ ^[0-9a-f]{64}$ ]] || {
    printf 'matrix: enabled profile has no immutable evidence digest: %s\n' "$profile_id" >&2
    exit 1
  }
  mounter_digest="$(profile_value "$profile_file" mounter_image_digest)"
  docker_digest="$(profile_value "$profile_file" docker_image_digest)"
  [[ "$mounter_digest" =~ ^[^[:space:]@]+@sha256:[0-9a-f]{64}$ ]] || {
    printf 'matrix: enabled profile has no common mounter digest: %s\n' "$profile_id" >&2
    exit 1
  }
  [[ "$docker_digest" =~ ^[^[:space:]@]+@sha256:[0-9a-f]{64}$ ]] || {
    printf 'matrix: enabled profile has no common Docker FUSE digest: %s\n' "$profile_id" >&2
    exit 1
  }
  if [[ -z "$common_mounter_digest" ]]; then
    common_mounter_digest="$mounter_digest"
    common_docker_digest="$docker_digest"
  elif [[ "$mounter_digest" != "$common_mounter_digest" || "$docker_digest" != "$common_docker_digest" ]]; then
    printf 'matrix: all enabled profiles must use the same common FUSE image digests\n' >&2
    exit 1
  fi
  enabled_count=$((enabled_count + 1))
  for runtime_name in "${runtimes[@]}"; do
    printf 'RUN runtime=%s profile=%s\n' "$runtime_name" "$profile_id"
    WORKSPACE_FUSE_RUN_INTEGRATION=1 WORKSPACE_FUSE_REQUIRE_FAULTS=1 WORKSPACE_FUSE_REQUIRE_OBJECT_COUNTER=1 \
    WORKSPACE_FUSE_EVIDENCE_OUTPUT="$evidence_dir/$profile_id-$runtime_name.json" "$preflight" \
      "$runtime_name" --profile "$profile_file"
    [[ -s "$evidence_dir/$profile_id-$runtime_name.json" ]] || {
      printf 'matrix: preflight did not retain API evidence: %s/%s\n' "$profile_id" "$runtime_name" >&2
      exit 1
    }
  done
done

if [[ "${ALLOW_BLOCKED_FUSE_PROFILES:-0}" != "1" && "$skipped_count" -ne 0 ]]; then
  printf 'matrix: %d profile(s) remain blocked\n' "$skipped_count" >&2
  exit 1
fi
printf 'workspace-fuse matrix complete: enabled=%d blocked=%d combinations=%d\n' \
  "$enabled_count" "$skipped_count" "$((enabled_count * ${#runtimes[@]}))"
