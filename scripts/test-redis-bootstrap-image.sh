#!/usr/bin/env bash
set -euo pipefail
[[ $# == 0 ]] || { printf 'usage: test-redis-bootstrap-image.sh\n' >&2; exit 2; }
repo_root="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
api="$repo_root/docker/Dockerfile"
bootstrap="$repo_root/docker/images/redis-bootstrap/Dockerfile"
test -f "$bootstrap"
grep -Fq './cmd/redis-bootstrap' "$bootstrap"
grep -Fq '/app/redis-bootstrap' "$bootstrap"
! grep -Fq '/app/sandbox' "$bootstrap"
! grep -Fq 'docker-cli' "$bootstrap"
! grep -Fq './cmd/redis-bootstrap' "$api"
! grep -Fq '/app/redis-bootstrap' "$api"
tmp="$(mktemp -d "${TMPDIR:-/tmp}/sandbox-bootstrap-build.XXXXXX")"
trap 'rm -f -- "$tmp/redis-bootstrap-amd64" "$tmp/redis-bootstrap-arm64"; rmdir -- "$tmp"' EXIT
for arch in amd64 arm64; do
  GOPROXY=off CGO_ENABLED=0 GOOS=linux GOARCH="$arch" \
    go build -trimpath -ldflags='-s -w' -o "$tmp/redis-bootstrap-$arch" ./cmd/redis-bootstrap
  test -x "$tmp/redis-bootstrap-$arch"
done
printf 'redis bootstrap image contract: PASS\n'
