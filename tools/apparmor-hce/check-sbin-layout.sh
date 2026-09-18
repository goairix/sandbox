#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)

die() {
  printf 'error: %s\n' "$1" >&2
  exit 1
}

root_join() {
  local root=$1 path=${2#/}
  if [[ "$root" == / ]]; then
    printf '/%s\n' "$path"
  else
    printf '%s/%s\n' "${root%/}" "$path"
  fi
}

check_layout() {
  (( $# == 0 || $# == 1 || $# == 3 )) || die 'usage: check-sbin-layout.sh [root] [parser-path parser-arch]'
  local root_arg=${1:-/}
  [[ -d "$root_arg" ]] || die 'root must be an existing directory'
  local root
  root=$(CDPATH= cd -- "$root_arg" && pwd -P)
  local sbin expected
  sbin=$(root_join "$root" /sbin)
  expected=$(root_join "$root" /usr/sbin)

  [[ -L "$sbin" ]] || die "$sbin is not a symlink"
  local resolved
  resolved=$(readlink -f -- "$sbin") || die "cannot resolve $sbin"
  [[ "$resolved" == "$expected" ]] || die "$sbin resolves to $resolved, expected $expected"

  if (( $# == 3 )); then
    local parser_arg=$2 parser_arch=$3 parser_candidate parser
    case "$parser_arch" in
      x86_64|aarch64) ;;
      *) die 'parser architecture must be x86_64 or aarch64' ;;
    esac
    parser_candidate=$(root_join "$root" "$parser_arg")
    parser=$(readlink -f -- "$parser_candidate") || die 'cannot resolve parser path'
    if [[ "$root" == / ]]; then
      [[ "$parser" == /* ]] || die 'parser path escapes root'
    else
      [[ "$parser" == "$root"/* ]] || die 'parser path escapes root'
    fi
    [[ -f "$parser" && -x "$parser" ]] || die 'parser is missing or not executable'
    PYTHONDONTWRITEBYTECODE=1 python3 "$SCRIPT_DIR/lib/elf_audit.py" "$parser" "$parser_arch"
  fi
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  check_layout "$@"
fi
