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
    profile_id="${profile_id:-$(profile_value "$profile_file" profile_id)}"
    fuse_image="${fuse_image:-$(profile_value "$profile_file" mounter_image_digest)}"
    sandbox_image="${sandbox_image:-$(profile_value "$profile_file" sandbox_image_digest)}"
    [[ "$(profile_value "$profile_file" status)" == "release-verified" ]] || fail "profile report is not release-verified"
  fi
  [[ "$profile_id" =~ ^[a-z0-9][a-z0-9._-]+$ ]] || fail "profile ID is required and must be canonical"
  require_digest FUSE_IMAGE "$fuse_image"
  require_digest SANDBOX_IMAGE "$sandbox_image"
  [[ "${FUSE_LSM_PROFILE:-}" != "" && "${FUSE_LSM_PROFILE,,}" != "unconfined" && "${FUSE_LSM_PROFILE,,}" != "label=disable" ]] || fail "FUSE_LSM_PROFILE must name a confined profile"
  [[ -z "${WORKSPACE_PROXY_URL:-}" ]] || fail "workspace proxy is forbidden"

  local image_check="package-check"
  [[ -n "$profile_file" ]] && image_check="release-check"
  FUSE_IMAGE="$fuse_image" SANDBOX_IMAGE="$sandbox_image" \
    "$repo_root/scripts/verify-fuse-image.sh" "$runtime_name" "$image_check" "$profile_id"

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
  local profile_id="" image_digest="" sandbox_digest="" service_version="" s3fs_version="" marker="" tls_verify="" output=""
  local -a options=()
  while (($#)); do
    case "$1" in
      --profile-id) profile_id="$2"; shift 2 ;;
      --image-digest) image_digest="$2"; shift 2 ;;
      --sandbox-image-digest) sandbox_digest="$2"; shift 2 ;;
      --service-version) service_version="$2"; shift 2 ;;
      --s3fs-version) s3fs_version="$2"; shift 2 ;;
      --directory-marker) marker="$2"; shift 2 ;;
      --tls-verify) tls_verify="$2"; shift 2 ;;
      --option) options+=("$2"); shift 2 ;;
      --output) output="$2"; shift 2 ;;
      *) fail "unknown record-profile argument: $1" ;;
    esac
  done
  [[ "$profile_id" =~ ^(minio-sigv4-path-style-v1|huawei-obs-public-v1|huawei-obs-private-2023-v1)$ ]] || fail "unsupported profile ID"
  require_digest image_digest "$image_digest"
  require_digest sandbox_image_digest "$sandbox_digest"
  [[ -n "$service_version" && -n "$s3fs_version" ]] || fail "observed service and s3fs versions are required"
  [[ "$service_version" =~ ^[A-Za-z0-9._:+/-]+$ && "$s3fs_version" =~ ^[A-Za-z0-9._:+/-]+$ ]] || fail "version values must be canonical"
  [[ "$marker" == "trailing-slash-zero-byte" ]] || fail "unsupported directory marker"
  [[ "$tls_verify" == "true" ]] || fail "TLS verification must be true"
  [[ -n "$output" ]] || fail "--output is required"
  for option in "${options[@]}"; do
    [[ "$option" =~ ^[A-Za-z0-9_./:=+-]+$ ]] || fail "invalid profile option"
  done
  mkdir -p -- "$(dirname -- "$output")"
  local staged="$tmp_dir/profile.yaml"
  {
    printf 'profile_id: %s\n' "$profile_id"
    printf 'status: release-verified\n'
    printf 'mounter_image_digest: %s\n' "$image_digest"
    printf 'sandbox_image_digest: %s\n' "$sandbox_digest"
    printf 'service_version: %s\n' "$service_version"
    printf 's3fs_version: %s\n' "$s3fs_version"
    printf 'directory_marker: %s\n' "$marker"
    printf 'tls_verify: true\n'
    printf 'options:\n'
    for option in "${options[@]}"; do printf '  - %s\n' "$option"; done
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
