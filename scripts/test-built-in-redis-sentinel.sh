#!/usr/bin/env bash
set -euo pipefail
[[ $# == 0 ]] || { printf 'usage: test-built-in-redis-sentinel.sh\n' >&2; exit 2; }
repo_root="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
project="sandbox-sentinel-runtime-$(date +%Y%m%d%H%M%S)-$$-$RANDOM"
[[ "$project" =~ ^sandbox-sentinel-runtime-[0-9]{14}-[0-9]+-[0-9]+$ ]]
architecture="$(docker image inspect redis:7-alpine --format '{{.Architecture}}')"
case "$architecture" in arm64|amd64) ;; *) exit 1 ;; esac
fixture_dir="$(mktemp -d "${TMPDIR:-/tmp}/sandbox-sentinel-runtime.XXXXXX")"
mkdir -- "$fixture_dir/bins" "$fixture_dir/control" "$fixture_dir/public" "$fixture_dir/node-0" "$fixture_dir/node-1" "$fixture_dir/node-2"
cleanup() {
  local result="$?" target owner existing
  trap - EXIT INT TERM
  set +e
  for target in "$project-controller" "$project-node-0" "$project-node-1" "$project-node-2"; do
    existing="$(docker ps -aq --filter "name=^/$target$")" || exit 1
    if [[ -n "$existing" ]]; then
      owner="$(docker inspect --format '{{index .Config.Labels "sandbox.test.owner"}}' "$target")"
      [[ "$owner" == "$project" ]] || exit 1
      docker rm -f -- "$target" >/dev/null || exit 1
    fi
  done
  for target in "$project-data-0" "$project-data-1" "$project-data-2"; do
    existing="$(docker volume ls -q --filter "name=^$target$")" || exit 1
    if [[ -n "$existing" ]]; then
      owner="$(docker volume inspect --format '{{index .Labels "sandbox.test.owner"}}' "$target")"
      [[ "$owner" == "$project" ]] || exit 1
      docker volume rm -- "$target" >/dev/null || exit 1
    fi
  done
  existing="$(docker network ls -q --filter "name=^$project$")" || exit 1
  if [[ -n "$existing" ]]; then
    owner="$(docker network inspect --format '{{index .Labels "sandbox.test.owner"}}' "$project")"
    [[ "$owner" == "$project" ]] || exit 1
    docker network rm -- "$project" >/dev/null || exit 1
  fi
  rm -f -- "$fixture_dir/bins/redisbootstrap.test" "$fixture_dir/bins/redis-bootstrap" \
    "$fixture_dir/control/cluster.json" "$fixture_dir/control/registration.json" \
    "$fixture_dir/public/public-keys.json" "$fixture_dir/node-0/seed" "$fixture_dir/node-1/seed" "$fixture_dir/node-2/seed" "$fixture_dir/expected-primary" || exit 1
  rmdir -- "$fixture_dir/bins" "$fixture_dir/control" "$fixture_dir/public" "$fixture_dir/node-0" "$fixture_dir/node-1" "$fixture_dir/node-2" "$fixture_dir" || exit 1
  existing="$(docker ps -aq --filter "label=sandbox.test.owner=$project")" || exit 1
  [[ -z "$existing" ]] || exit 1
  existing="$(docker volume ls -q --filter "label=sandbox.test.owner=$project")" || exit 1
  [[ -z "$existing" ]] || exit 1
  existing="$(docker network ls -q --filter "label=sandbox.test.owner=$project")" || exit 1
  [[ -z "$existing" ]] || exit 1
  printf 'Sentinel fixture cleanup: zero owned containers, volumes and networks\n'
  exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
cd -- "$repo_root"
GOPROXY=off GOOS=linux GOARCH="$architecture" CGO_ENABLED=0 go test -c -o "$fixture_dir/bins/redisbootstrap.test" ./internal/redisbootstrap
GOPROXY=off GOOS=linux GOARCH="$architecture" CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o "$fixture_dir/bins/redis-bootstrap" ./cmd/redis-bootstrap
docker network create --internal --label "sandbox.test.owner=$project" "$project" >/dev/null
prior_ips=()
for ordinal in 0 1 2; do
  docker volume create --label "sandbox.test.owner=$project" "$project-data-$ordinal" >/dev/null
  docker run -d --pull never --init --name "$project-node-$ordinal" --hostname "$project" \
    --label "sandbox.test.owner=$project" --network "$project" --network-alias "redis-$ordinal" \
    --mount "type=volume,src=$project-data-$ordinal,dst=/data" \
    --mount "type=bind,src=$fixture_dir/bins,dst=/fixture,readonly" \
    --mount "type=bind,src=$fixture_dir/control,dst=/bootstrap,readonly" \
    --mount "type=bind,src=$fixture_dir/public,dst=/identity-public,readonly" \
    --mount "type=bind,src=$fixture_dir/node-$ordinal,dst=/fixture-own,readonly" \
    --tmpfs /identity-private:rw,nosuid,nodev,size=1m --tmpfs /redis-tools:rw,nosuid,nodev,size=64m \
    --env "TEST_SENTINEL_OWNER=$project" --env TEST_SENTINEL_ROLE=node --env "TEST_SENTINEL_ORDINAL=$ordinal" \
    --entrypoint /fixture/redisbootstrap.test redis:7-alpine \
    -test.run '^TestSentinelRuntimeNode$' -test.count=1 -test.timeout=360s >/dev/null
  # Stopped Docker endpoints may report "invalid IP"; retain the live address.
  address="$(docker inspect --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$project-node-$ordinal")"
  [[ "$address" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]]
  prior_ips+=("$address")
done
printf 'isolated current-binary Sentinel fixture: %s\n' "$project"
docker run --pull never --init --name "$project-controller" --hostname "$project" \
  --label "sandbox.test.owner=$project" --network "$project" \
  --mount "type=bind,src=$fixture_dir/bins,dst=/fixture,readonly" \
  --mount "type=bind,src=$fixture_dir,dst=/fixture-state" \
  --env "TEST_SENTINEL_OWNER=$project" --env TEST_SENTINEL_ROLE=controller \
  --entrypoint /fixture/redisbootstrap.test redis:7-alpine \
  -test.run '^TestSentinelRuntimeThreeMembers$' -test.count=1 -test.timeout=260s -test.v

# Actual endpoint replacement without touching DNS membership, keys or PVCs.
# Rotate three already-owned IPs after disconnecting all stopped fixture nodes.
for ordinal in 0 1 2; do
  [[ "$(docker inspect --format '{{index .Config.Labels "sandbox.test.owner"}}' "$project-node-$ordinal")" == "$project" ]]
  [[ "$(docker inspect --format '{{.State.Running}}' "$project-node-$ordinal")" == false ]]
done
[[ "$(docker inspect --format '{{index .Config.Labels "sandbox.test.owner"}}' "$project-controller")" == "$project" ]]
docker rm -- "$project-controller" >/dev/null
for ordinal in 0 1 2; do docker network disconnect "$project" "$project-node-$ordinal"; done
for ordinal in 0 1 2; do
  next=$(((ordinal + 1) % 3))
  [[ "${prior_ips[$next]}" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]]
  docker network connect --ip "${prior_ips[$next]}" --alias "redis-$ordinal" "$project" "$project-node-$ordinal"
  docker start "$project-node-$ordinal" >/dev/null
done
docker run --pull never --init --name "$project-controller" --hostname "$project" \
  --label "sandbox.test.owner=$project" --network "$project" \
  --mount "type=bind,src=$fixture_dir/bins,dst=/fixture,readonly" \
  --mount "type=bind,src=$fixture_dir,dst=/fixture-state" \
  --env "TEST_SENTINEL_OWNER=$project" --env TEST_SENTINEL_ROLE=controller \
  --env "TEST_SENTINEL_OLD_IPS=${prior_ips[0]},${prior_ips[1]},${prior_ips[2]}" \
  --entrypoint /fixture/redisbootstrap.test redis:7-alpine \
  -test.run '^TestSentinelRuntimeNewIP$' -test.count=1 -test.timeout=90s -test.v
