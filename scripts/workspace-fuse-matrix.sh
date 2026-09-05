#!/usr/bin/env bash
set -euo pipefail

repo_root="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
profile_dir="${WORKSPACE_FUSE_PROFILE_DIR:-$repo_root/testdata/fuse/profiles}"
preflight="${WORKSPACE_FUSE_PREFLIGHT:-$repo_root/scripts/workspace-fuse-preflight.sh}"
profiles=(minio-sigv4-path-style-v1 huawei-obs-public-v1 huawei-obs-private-2023-v1)
runtimes=(kubernetes docker)
enabled_count=0
skipped_count=0

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
  enabled_count=$((enabled_count + 1))
  for runtime_name in "${runtimes[@]}"; do
    printf 'RUN runtime=%s profile=%s\n' "$runtime_name" "$profile_id"
    WORKSPACE_FUSE_RUN_INTEGRATION=1 WORKSPACE_FUSE_REQUIRE_FAULTS=1 "$preflight" \
      "$runtime_name" --profile "$profile_file"
  done
done

if [[ "${ALLOW_BLOCKED_FUSE_PROFILES:-0}" != "1" && "$skipped_count" -ne 0 ]]; then
  printf 'matrix: %d profile(s) remain blocked\n' "$skipped_count" >&2
  exit 1
fi
printf 'workspace-fuse matrix complete: enabled=%d blocked=%d combinations=%d\n' \
  "$enabled_count" "$skipped_count" "$((enabled_count * ${#runtimes[@]}))"
