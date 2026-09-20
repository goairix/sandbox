#!/usr/bin/env bash
set -u

usage() {
  cat <<'EOF'
Usage: check-node-prerequisites.sh [--root ROOT]

Read-only check of the host prerequisites needed by containerd CRI AppArmor
and the sandbox FUSE mounter. It never installs packages, edits files, or
restarts services. ROOT is intended for offline inspection; the default is /.
EOF
}

ROOT=/
while (($# > 0)); do
  case "$1" in
    --root)
      (($# >= 2)) || { printf 'error: --root requires a directory\n' >&2; exit 2; }
      ROOT=$2
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      printf 'error: unknown argument: %s\n' "$1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

[[ "$ROOT" = /* && -d "$ROOT" ]] || {
  printf 'error: --root must be an existing absolute directory\n' >&2
  exit 2
}
ROOT=$(CDPATH= cd -- "$ROOT" && pwd -P)

root_path() {
  local path=${1#/}
  if [[ "$ROOT" == / ]]; then
    printf '/%s\n' "$path"
  else
    printf '%s/%s\n' "${ROOT%/}" "$path"
  fi
}

pass_count=0
fail_count=0
pass() {
  pass_count=$((pass_count + 1))
  printf '[PASS] %s\n' "$1"
}
fail() {
  fail_count=$((fail_count + 1))
  printf '[FAIL] %s\n' "$1"
}

arch=${SANDBOX_ARCH_OVERRIDE:-$(uname -m 2>/dev/null || printf unknown)}
case "$arch" in
  x86_64|aarch64) pass "supported node architecture: $arch" ;;
  *) fail "unsupported node architecture: $arch (expected x86_64 or aarch64)" ;;
esac

enabled_file=$(root_path /sys/module/apparmor/parameters/enabled)
if [[ -r "$enabled_file" ]] && [[ $(tr -d '[:space:]' <"$enabled_file") == Y ]]; then
  pass 'kernel AppArmor is enabled'
else
  fail "kernel AppArmor is not enabled (expected $enabled_file to contain Y)"
fi

securityfs=$(root_path /sys/kernel/security)
if mountpoint -q "$securityfs" >/dev/null 2>&1; then
  pass "securityfs is mounted at $securityfs"
else
  fail "securityfs is not mounted at $securityfs"
fi

parser=$(root_path /sbin/apparmor_parser)
if [[ -x "$parser" ]]; then
  resolved=$(readlink -f -- "$parser" 2>/dev/null || printf '')
  if [[ -n "$resolved" && ( "$ROOT" == / || "$resolved" == "$ROOT"/* ) ]]; then
    pass "host AppArmor parser exists: ${parser#$ROOT}"
  else
    fail "host AppArmor parser resolves outside the inspected root: $resolved"
  fi
  version=$("$parser" --version 2>&1)
  parser_status=$?
  if [[ $parser_status -eq 0 && -n "$version" ]]; then
    pass "host AppArmor parser runs: $(printf '%s' "$version" | head -n 1)"
  else
    fail "host AppArmor parser cannot run (exit $parser_status)"
  fi
else
  fail "host AppArmor parser is missing or not executable: $parser"
fi

if systemctl is-active --quiet containerd >/dev/null 2>&1; then
  pass 'containerd service is active'
else
  fail 'containerd service is not active'
fi

containerd_config=$(root_path /etc/containerd/config.toml)
if [[ -r "$containerd_config" ]]; then
  pass "containerd configuration is readable: ${containerd_config#$ROOT}"
else
  fail "containerd configuration is missing or unreadable: $containerd_config"
fi

cri_json=$(crictl info 2>/dev/null)
cri_status=$?
if [[ $cri_status -ne 0 || -z "$cri_json" ]]; then
  fail "crictl info failed (exit $cri_status)"
else
  disable_apparmor=$(printf '%s' "$cri_json" | python3 -c '
import json, sys
try:
    value = json.load(sys.stdin)["config"]["disableApparmor"]
except (KeyError, TypeError, ValueError, json.JSONDecodeError):
    print("missing")
else:
    print(str(value).lower())
')
  if [[ "$disable_apparmor" == false ]]; then
    pass 'running CRI has AppArmor enabled: disableApparmor=false'
  elif [[ "$disable_apparmor" == true ]]; then
    fail 'running CRI has AppArmor disabled: disableApparmor=true'
  else
    fail 'running CRI did not expose config.disableApparmor as a boolean'
  fi
fi

printf 'SUMMARY: %d passed, %d failed\n' "$pass_count" "$fail_count"
if ((fail_count > 0)); then
  printf 'RESULT: FAIL\n'
  exit 1
fi
printf 'RESULT: PASS\n'
