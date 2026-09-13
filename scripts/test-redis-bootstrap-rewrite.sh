#!/usr/bin/env bash
set -euo pipefail
[[ $# == 0 ]] || { printf 'usage: test-redis-bootstrap-rewrite.sh\n' >&2; exit 2; }

repo_root="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
project="sandbox-sentinel-rewrite-$(date +%Y%m%d%H%M%S)-$$-$RANDOM"
[[ "$project" =~ ^sandbox-sentinel-rewrite-[0-9]{14}-[0-9]+-[0-9]+$ ]]
architecture="$(docker image inspect redis:7-alpine --format '{{.Architecture}}')"
case "$architecture" in arm64|amd64) ;; *) printf 'unsupported cached Redis architecture\n' >&2; exit 1 ;; esac
[[ -z "$(docker ps -aq --filter "name=^/$project$")" ]]
fixture_dir="$(mktemp -d "${TMPDIR:-/tmp}/sandbox-sentinel-rewrite.XXXXXX")"

cleanup() {
  local result="$?" container owner
  trap - EXIT INT TERM
  set +e
  container="$(docker ps -aq --filter "name=^/$project$")"
  if [[ $? != 0 ]]; then printf 'cannot verify fixture container cleanup\n' >&2; exit 1; fi
  if [[ -n "$container" ]]; then
    owner="$(docker inspect --format '{{index .Config.Labels "sandbox.test.owner"}}' "$project")"
    if [[ $? != 0 || "$owner" != "$project" ]]; then printf 'refusing unowned fixture cleanup\n' >&2; exit 1; fi
    docker rm -f -- "$project" >/dev/null || exit 1
  fi
  container="$(docker ps -aq --filter "name=^/$project$")"
  if [[ $? != 0 || -n "$container" ]]; then printf 'fixture container remains\n' >&2; exit 1; fi
  rm -f -- "$fixture_dir/redisbootstrap.test" || exit 1
  rm -f -- "$fixture_dir/redis-bootstrap" || exit 1
  rmdir -- "$fixture_dir" || exit 1
  printf 'rewrite fixture cleanup: zero owned containers; no volumes or networks created\n'
  exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

cd -- "$repo_root"
# Compile an isolated test binary; never build/pull/push a release image.
GOPROXY=off GOOS=linux GOARCH="$architecture" CGO_ENABLED=0 go test -c -o "$fixture_dir/redisbootstrap.test" ./internal/redisbootstrap
GOPROXY=off GOOS=linux GOARCH="$architecture" CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o "$fixture_dir/redis-bootstrap" ./cmd/redis-bootstrap
printf 'isolated rewrite fixture: %s\n' "$project"
docker run --rm --pull never --init --name "$project" --hostname "$project" \
  --label "sandbox.test.owner=$project" --network none \
  --add-host redis-0:127.0.0.1 --add-host redis-1:192.0.2.1 --add-host redis-2:192.0.2.2 \
  --tmpfs /data:rw,nosuid,nodev,size=64m \
  --mount "type=bind,src=$fixture_dir/redisbootstrap.test,dst=/fixture/redisbootstrap.test,readonly" \
  --mount "type=bind,src=$fixture_dir/redis-bootstrap,dst=/fixture/redis-bootstrap,readonly" \
  --env "TEST_REDIS_BOOTSTRAP_REWRITE_OWNER=$project" \
  --entrypoint /fixture/redisbootstrap.test redis:7-alpine \
  -test.run '^TestPersistentRealRewriteIntegration$' -test.count=1 -test.timeout=60s -test.v
