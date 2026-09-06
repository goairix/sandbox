#!/usr/bin/env bash
set -euo pipefail

repo_root="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
tmp_dir="$(mktemp -d)"
cleanup() { rm -rf -- "$tmp_dir"; }
trap cleanup EXIT INT TERM

fail() { printf 'workspace-fuse preflight: %s\n' "$*" >&2; exit 1; }
require_command() { command -v "$1" >/dev/null 2>&1 || fail "missing command: $1"; }
require_digest() {
  [[ "$2" =~ ^[^[:space:]@]+@sha256:[0-9a-f]{64}$ ]] || fail "$1 must be an immutable lowercase sha256 image digest"
}
profile_value() {
  python3 - "$1" "$2" <<'PY'
import sys, yaml
doc = yaml.safe_load(open(sys.argv[1], encoding="utf-8")) or {}
value = doc.get(sys.argv[2], "")
if isinstance(value, list):
    print(",".join(str(v) for v in value))
else:
    print(value)
PY
}

runtime_preflight() {
  local runtime_name="$1"; shift
  local profile_file=""
  while (($#)); do
    case "$1" in
      --profile) (($# >= 2)) || fail "--profile requires a file"; profile_file="$2"; shift 2 ;;
      *) fail "unknown $runtime_name argument: $1" ;;
    esac
  done

  local profile_id="${WORKSPACE_FUSE_PROFILE_ID:-}"
  local fuse_image="${FUSE_IMAGE:-}"
  local sandbox_image="${SANDBOX_IMAGE:-}"
  if [[ -n "$profile_file" ]]; then
    [[ -f "$profile_file" ]] || fail "profile does not exist: $profile_file"
    require_command python3
    local report_profile_id report_fuse_image report_sandbox_image report_provider
    report_profile_id="$(profile_value "$profile_file" profile_id)"
    report_fuse_image="$(profile_value "$profile_file" mounter_image_digest)"
    report_sandbox_image="$(profile_value "$profile_file" sandbox_image_digest)"
    report_provider="$(profile_value "$profile_file" provider)"
    [[ -z "$profile_id" || "$profile_id" == "$report_profile_id" ]] || fail "WORKSPACE_FUSE_PROFILE_ID does not match the verified report"
    [[ -z "$fuse_image" || "$fuse_image" == "$report_fuse_image" ]] || fail "FUSE_IMAGE does not match the verified report"
    [[ -z "$sandbox_image" || "$sandbox_image" == "$report_sandbox_image" ]] || fail "SANDBOX_IMAGE does not match the verified report"
    profile_id="$report_profile_id"
    fuse_image="$report_fuse_image"
    sandbox_image="$report_sandbox_image"
    case "$profile_id:$report_provider" in
      minio-sigv4-path-style-v1:minio|huawei-obs-public-v1:obs|huawei-obs-private-2023-v1:obs) ;;
      *) fail "profile report provider does not match its immutable profile ID" ;;
    esac
    [[ "$(profile_value "$profile_file" schema_version)" == "1" ]] || fail "profile report schema version is unsupported"
    [[ "$(profile_value "$profile_file" enabled)" == "True" || "$(profile_value "$profile_file" enabled)" == "true" ]] || fail "profile report is not enabled"
    [[ "$(profile_value "$profile_file" status)" == "release-verified" ]] || fail "profile report is not release-verified"
    [[ "$(profile_value "$profile_file" evidence_sha256)" =~ ^[0-9a-f]{64}$ ]] || fail "profile report has no immutable evidence digest"
    [[ -n "$(profile_value "$profile_file" service_version)" && -n "$(profile_value "$profile_file" s3fs_version)" ]] || fail "profile report has no observed service or s3fs version"
    [[ "$(profile_value "$profile_file" tls_verify)" == "True" || "$(profile_value "$profile_file" tls_verify)" == "true" ]] || fail "profile report did not verify TLS"
    if [[ "$report_provider" == "obs" ]]; then
      [[ -n "$(profile_value "$profile_file" everest_version)" ]] || fail "OBS profile report has no observed Everest version"
    fi
  fi
  [[ "$profile_id" =~ ^[a-z0-9][a-z0-9._-]+$ ]] || fail "profile ID is required and must be canonical"
  require_digest FUSE_IMAGE "$fuse_image"
  require_digest SANDBOX_IMAGE "$sandbox_image"
  [[ "${FUSE_LSM_PROFILE:-}" != "" && "${FUSE_LSM_PROFILE,,}" != "unconfined" && "${FUSE_LSM_PROFILE,,}" != "label=disable" ]] || fail "FUSE_LSM_PROFILE must name a confined profile"
  [[ -z "${WORKSPACE_PROXY_URL:-}" ]] || fail "workspace proxy is forbidden"

  local image_check="package-check"
  [[ -n "$profile_file" ]] && image_check="release-check"
  FUSE_IMAGE="$fuse_image" SANDBOX_IMAGE="$sandbox_image" \
    "$repo_root/scripts/verify-fuse-image.sh" "$runtime_name" "$image_check"

  case "$runtime_name" in
    kubernetes)
      require_command kubectl
      local namespace="${WORKSPACE_RUNTIME_NAMESPACE:?WORKSPACE_RUNTIME_NAMESPACE is required}"
      local secret_name="${WORKSPACE_RUNTIME_SECRET:?WORKSPACE_RUNTIME_SECRET is required}"
      kubectl get secret -n "$namespace" "$secret_name" >/dev/null
      for permission in 'create pods' 'get pods' 'list pods' 'watch pods' 'delete pods' 'patch pods' 'create pods/exec' 'create networkpolicies.networking.k8s.io' 'patch networkpolicies.networking.k8s.io' 'delete networkpolicies.networking.k8s.io'; do
        read -r verb resource <<<"$permission"
        kubectl auth can-i --quiet "$verb" "$resource" -n "$namespace" || fail "RBAC denied: $verb $resource in $namespace"
      done
      if [[ "$(kubectl config current-context 2>/dev/null || true)" == kind-* ]]; then
        require_command docker
        require_command kind
        while IFS= read -r node; do
          docker exec "$node" test -c /dev/fuse || fail "$node does not expose /dev/fuse"
        done < <(kind get nodes --name "${KIND_CLUSTER_NAME:-kind}")
      else
        [[ -n "${FUSE_DEVICE_CHECK_CMD:-}" ]] || fail "set FUSE_DEVICE_CHECK_CMD for non-kind node /dev/fuse verification"
        bash -ceu "$FUSE_DEVICE_CHECK_CMD"
      fi
      kubectl get pods -n "$namespace" -l sandbox.managed=true,sandbox.workspace.mode=fuse -o json | python3 -c '
import json,sys
for pod in json.load(sys.stdin).get("items", []):
  for volume in pod.get("spec", {}).get("volumes", []):
    if volume.get("hostPath") and volume.get("name") == "workspace":
      raise SystemExit("FUSE Pod exposes host business /workspace")
'
      ;;
    docker)
      require_command docker
      docker info >/dev/null
      docker run --rm --device /dev/fuse "$sandbox_image" test -c /dev/fuse >/dev/null
      local staging="${WORKSPACE_SECRET_STAGING_ROOT:?WORKSPACE_SECRET_STAGING_ROOT is required}"
      [[ -d "$staging" ]] || fail "secret staging root is missing"
      [[ "$(stat -c '%u:%g:%a' "$staging" 2>/dev/null || stat -f '%u:%g:%Lp' "$staging")" == "0:0:700" ]] || fail "secret staging root must be root:root mode 0700"
      while IFS= read -r container_id; do
        [[ -z "$container_id" ]] && continue
        docker inspect "$container_id" --format '{{range .Mounts}}{{if and (eq .Destination "/workspace") (eq .Type "bind")}}{{.Source}}{{end}}{{end}}' | grep -q . \
          && fail "FUSE container bind-mounts host business /workspace"
      done < <(docker ps -aq --filter label=sandbox.managed=true --filter label=sandbox.role=fuse-runtime)
      ;;
  esac

  if [[ "${WORKSPACE_FUSE_RUN_INTEGRATION:-0}" == "1" ]]; then
    go test "$repo_root/test/integration/workspacefuse" -run TestWorkspaceFUSE -v \
      -args -runtime "$runtime_name" -profile "$profile_id"
  fi
  printf 'workspace-fuse preflight passed: runtime=%s profile=%s\n' "$runtime_name" "$profile_id"
}

record_profile() {
  local profile_id="" image_digest="" sandbox_digest="" service_version="" everest_version="" s3fs_version="" marker="" tls_verify="" output="" evidence=""
  local -a options=()
  while (($#)); do
    case "$1" in
      --profile-id) profile_id="$2"; shift 2 ;;
      --image-digest) image_digest="$2"; shift 2 ;;
      --sandbox-image-digest) sandbox_digest="$2"; shift 2 ;;
      --service-version) service_version="$2"; shift 2 ;;
      --everest-version) everest_version="$2"; shift 2 ;;
      --s3fs-version) s3fs_version="$2"; shift 2 ;;
      --directory-marker) marker="$2"; shift 2 ;;
      --tls-verify) tls_verify="$2"; shift 2 ;;
      --option) options+=("$2"); shift 2 ;;
      --evidence) evidence="$2"; shift 2 ;;
      --output) output="$2"; shift 2 ;;
      *) fail "unknown record-profile argument: $1" ;;
    esac
  done
  [[ "$profile_id" =~ ^(minio-sigv4-path-style-v1|huawei-obs-public-v1|huawei-obs-private-2023-v1)$ ]] || fail "unsupported profile ID"
  require_digest image_digest "$image_digest"
  require_digest sandbox_image_digest "$sandbox_digest"
  [[ -n "$service_version" && -n "$s3fs_version" ]] || fail "observed service and s3fs versions are required"
  [[ "$service_version" =~ ^[A-Za-z0-9._:+/-]+$ && "$s3fs_version" =~ ^[A-Za-z0-9._:+/-]+$ ]] || fail "version values must be canonical"
  if [[ "$profile_id" == huawei-obs-* ]]; then
    [[ -n "$everest_version" && "$everest_version" =~ ^[A-Za-z0-9._:+/-]+$ ]] || fail "OBS profiles require a canonical observed Everest version"
  fi
  [[ "$marker" == "trailing-slash-zero-byte" ]] || fail "unsupported directory marker"
  [[ "$tls_verify" == "true" ]] || fail "TLS verification must be true"
  [[ -n "$output" ]] || fail "--output is required"
  [[ -f "$evidence" ]] || fail "--evidence must name a matrix evidence JSON file"
  for option in "${options[@]}"; do
    [[ "$option" =~ ^[A-Za-z0-9_./:=+-]+$ ]] || fail "invalid profile option"
  done
  local joined_options
  joined_options="$(IFS=,; printf '%s' "${options[*]}")"
  local evidence_sha
  evidence_sha="$(python3 - "$evidence" "$profile_id" "$image_digest" "$sandbox_digest" "$service_version" "$everest_version" "$s3fs_version" "$marker" "$tls_verify" "$joined_options" <<'PY'
import hashlib, json, sys
with open(sys.argv[1], "rb") as stream:
    raw = stream.read()
evidence = json.loads(raw)
if evidence.get("profile_id") != sys.argv[2] or evidence.get("passed") is not True:
    raise SystemExit(1)
expected_fields = {
    "mounter_image_digest": sys.argv[3],
    "sandbox_image_digest": sys.argv[4],
    "service_version": sys.argv[5],
    "everest_version": sys.argv[6],
    "s3fs_version": sys.argv[7],
    "directory_marker": sys.argv[8],
}
if any(evidence.get(key, "") != value for key, value in expected_fields.items()):
    raise SystemExit(1)
if evidence.get("tls_verify") is not True:
    raise SystemExit(1)
if evidence.get("options") != ([value for value in sys.argv[10].split(",") if value]):
    raise SystemExit(1)
items = evidence.get("combinations")
if not isinstance(items, list):
    raise SystemExit(1)
expected = {"kubernetes", "docker"}
seen = set()
for item in items:
    if not isinstance(item, dict) or item.get("runtime") not in expected or item.get("runtime") in seen:
        raise SystemExit(1)
    if any(item.get(field) is not True for field in ("functional", "faults", "cleanup_confirmed")):
        raise SystemExit(1)
    seen.add(item["runtime"])
if seen != expected:
    raise SystemExit(1)
print(hashlib.sha256(raw).hexdigest())
PY
)" || fail "matrix evidence does not bind the exact images, parameters, both runtimes and all fault cases"
  local provider="minio"
  [[ "$profile_id" == huawei-obs-* ]] && provider="obs"
  mkdir -p -- "$(dirname -- "$output")"
  local staged="$tmp_dir/profile.yaml"
  {
    printf 'schema_version: 1\n'
    printf 'profile_id: %s\n' "$profile_id"
    printf 'provider: %s\n' "$provider"
    printf 'enabled: true\n'
    printf 'status: release-verified\n'
    printf 'mounter_image_digest: %s\n' "$image_digest"
    printf 'sandbox_image_digest: %s\n' "$sandbox_digest"
    printf 'service_version: %s\n' "$service_version"
    printf 'everest_version: %s\n' "$everest_version"
    printf 's3fs_version: %s\n' "$s3fs_version"
    printf 'evidence_sha256: "%s"\n' "$evidence_sha"
    printf 'directory_marker: %s\n' "$marker"
    printf 'tls_verify: true\n'
    if ((${#options[@]} == 0)); then
      printf 'options: []\n'
    else
      printf 'options:\n'
      for option in "${options[@]}"; do printf '  - %s\n' "$option"; done
    fi
  } >"$staged"
  chmod 0644 "$staged"
  mv -- "$staged" "$output"
  printf 'recorded verified profile: %s\n' "$output"
}

case "${1:-}" in
  kubernetes|docker) command_name="$1"; shift; runtime_preflight "$command_name" "$@" ;;
  record-profile) shift; record_profile "$@" ;;
  *) fail "usage: $0 {kubernetes|docker|record-profile} ..." ;;
esac
