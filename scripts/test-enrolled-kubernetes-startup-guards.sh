#!/usr/bin/env bash
set -uo pipefail
repo=$(cd "$(dirname "$0")/.." && pwd) || exit 1
script="$repo/scripts/test-enrolled-kubernetes-startup.sh"

# These fail before Go, Docker, input collection, or any artifact creation.
# Explicit exits also work on macOS bash3.2, where bare [[ ]] is not protected
# by errexit. Do not infer a passed native run from a wrapper's status alone.
reject() {
  local expected=$1 name=$2 status
  shift 2
  "$@"
  status=$?
  if [[ "$status" != "$expected" ]]; then
    printf 'FAIL %s: exit=%s expected=%s\n' "$name" "$status" "$expected"
    exit 1
  fi
  printf 'PASS %s: original exit=%s\n' "$name" "$status"
}

reject 1 invalid-mode env ENROLLED_STARTUP_EPOCH=guard-check bash "$script" invalid
reject 1 invalid-epoch env ENROLLED_STARTUP_EPOCH=INVALID bash "$script" host
reject 2 invalid-target env ENROLLED_STARTUP_EPOCH=guard-check ENROLLED_STARTUP_TARGET=unapproved bash "$script" host
