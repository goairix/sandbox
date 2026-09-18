#!/usr/bin/env bash
set -euo pipefail

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
    local parser_arg=$2 parser_arch=$3 parser_candidate parser magic size elf_class elf_data elf_version machine
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
    size=$(wc -c < "$parser")
    [[ "$size" -ge 64 ]] || die 'parser is not a complete ELF64 executable'
    magic=$(od -An -tx1 -N4 -- "$parser" | tr -d '[:space:]')
    [[ "$magic" == 7f454c46 ]] || die 'parser is not an ELF executable'
    elf_class=$(od -An -tu1 -j4 -N1 -- "$parser" | tr -d '[:space:]')
    elf_data=$(od -An -tu1 -j5 -N1 -- "$parser" | tr -d '[:space:]')
    elf_version=$(od -An -tu1 -j6 -N1 -- "$parser" | tr -d '[:space:]')
    elf_version_field=$(od -An -tx1 -j20 -N4 -- "$parser" | tr -d '[:space:]')
    elf_header_size=$(od -An -tx1 -j52 -N2 -- "$parser" | tr -d '[:space:]')
    [[ "$elf_class" == 2 && "$elf_data" == 1 && "$elf_version" == 1 ]] || die 'parser ELF header is not ELF64 little-endian'
    [[ "$elf_version_field" == 01000000 && "$elf_header_size" == 4000 ]] || die 'parser ELF header version or size is invalid'
    machine=$(od -An -tx1 -j18 -N2 -- "$parser" | tr -d '[:space:]')
    case "$parser_arch:$machine" in
      x86_64:3e00|aarch64:b700) ;;
      *) die 'parser ELF machine does not match requested architecture' ;;
    esac
  fi
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  check_layout "$@"
fi
