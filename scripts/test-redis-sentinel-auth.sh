#!/usr/bin/env bash
set -euo pipefail

verify_failure_cleanup=false
case "${1:-}" in
  "") [[ $# == 0 ]] ;;
  --verify-failure-cleanup) [[ $# == 1 ]]; verify_failure_cleanup=true ;;
  *) printf 'usage: test-redis-sentinel-auth.sh [--verify-failure-cleanup]\n' >&2; exit 2 ;;
esac

repo_root="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
fixture="$repo_root/testdata/sentinel-auth"
project="sandbox-sentinel-auth-$(date +%Y%m%d%H%M%S)-$$-$RANDOM"
[[ "$project" =~ ^sandbox-sentinel-auth-[0-9]{14}-[0-9]+-[0-9]+$ ]]

# These are cache-only checks, never implicit pulls/builds or business lookups.
docker image inspect redis:7-alpine golang:1.25-alpine >/dev/null
docker compose version >/dev/null
for resource in container volume network; do
  case "$resource" in
    container) existing="$(docker ps -aq --filter "label=com.docker.compose.project=$project")" ;;
    volume) existing="$(docker volume ls -q --filter "label=com.docker.compose.project=$project")" ;;
    network) existing="$(docker network ls -q --filter "label=com.docker.compose.project=$project")" ;;
  esac
  if [[ -n "$existing" ]]; then
    printf 'refusing existing fixture project resources\n' >&2
    exit 1
  fi
done

export SENTINEL_AUTH_REPO_ROOT="$repo_root"
export SENTINEL_AUTH_GOMODCACHE="$(go env GOMODCACHE)"
[[ -d "$SENTINEL_AUTH_GOMODCACHE" ]]
export SENTINEL_AUTH_CONFIG_DIR="$(mktemp -d "${TMPDIR:-/tmp}/sandbox-sentinel-auth.XXXXXX")"
compose=(docker compose --env-file /dev/null --profile tools --project-directory "$fixture" --project-name "$project" --file "$fixture/compose.yaml")

verify_project_targets() {
  local id labels project_label container_name service volume network containers volumes networks
  containers="$(docker ps -aq --filter "label=com.docker.compose.project=$project")" || return 1
  volumes="$(docker volume ls -q --filter "label=com.docker.compose.project=$project")" || return 1
  networks="$(docker network ls --format '{{.Name}}' --filter "label=com.docker.compose.project=$project")" || return 1
  while IFS= read -r id; do
    [[ -z "$id" ]] && continue
    labels="$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project"}} {{index .Config.Labels "com.docker.compose.service"}} {{.Name}}' "$id")" || return 1
    read -r project_label service container_name <<<"$labels"
    [[ "$project_label" == "$project" && "$container_name" == "/$project-"* ]] || return 1
    case "$service" in
      redis-0|redis-1|redis-2|sentinel-0|sentinel-1|sentinel-2|config-generator|test-runner) ;;
      *) return 1 ;;
    esac
  done <<<"$containers"
  while IFS= read -r volume; do
    [[ -z "$volume" ]] && continue
    case "$volume" in
      "${project}_redis-0"|"${project}_redis-1"|"${project}_redis-2"|"${project}_sentinel-0"|"${project}_sentinel-1"|"${project}_sentinel-2") ;;
      *) return 1 ;;
    esac
  done <<<"$volumes"
  while IFS= read -r network; do
    [[ -z "$network" ]] && continue
    [[ "$network" == "${project}_default" ]] || return 1
  done <<<"$networks"
}

cleanup() {
  local result="$?" containers volumes networks
  trap - EXIT INT TERM
  set +e
  if ! verify_project_targets; then
    printf 'refusing cleanup: fixture project target validation failed\n' >&2
    exit 1
  fi
  "${compose[@]}" down --volumes --timeout 10
  if [[ $? != 0 ]]; then
    printf 'fixture cleanup failed; no success claim\n' >&2
    exit 1
  fi
  if ! containers="$(docker ps -aq --filter "label=com.docker.compose.project=$project")" ||
     ! volumes="$(docker volume ls -q --filter "label=com.docker.compose.project=$project")" ||
     ! networks="$(docker network ls -q --filter "label=com.docker.compose.project=$project")"; then
    printf 'cannot verify fixture cleanup; no success claim\n' >&2
    exit 1
  fi
  if [[ -n "$containers" || -n "$volumes" || -n "$networks" ]]; then
    printf 'fixture resources remain after cleanup\n' >&2
    exit 1
  fi
  # Six exact generated files in the mktemp-owned directory; no recursive delete.
  rm -f -- "$SENTINEL_AUTH_CONFIG_DIR/redis-0.conf" "$SENTINEL_AUTH_CONFIG_DIR/redis-1.conf" "$SENTINEL_AUTH_CONFIG_DIR/redis-2.conf" \
    "$SENTINEL_AUTH_CONFIG_DIR/sentinel-0.conf" "$SENTINEL_AUTH_CONFIG_DIR/sentinel-1.conf" "$SENTINEL_AUTH_CONFIG_DIR/sentinel-2.conf"
  rmdir -- "$SENTINEL_AUTH_CONFIG_DIR"
  if [[ $? != 0 ]]; then
    printf 'private fixture temporary directory cleanup failed\n' >&2
    exit 1
  fi
  printf 'fixture cleanup: zero project containers, volumes and networks\n'
  if [[ "$result" == 0 ]]; then
    printf 'real external Sentinel auth tests: PASS\n'
  fi
  exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

printf 'isolated project: %s\n' "$project"
"${compose[@]}" run --rm --no-deps config-generator
"${compose[@]}" up --detach --no-build --pull never redis-0 redis-1 redis-2 sentinel-0 sentinel-1 sentinel-2
verify_project_targets
"${compose[@]}" exec -T redis-0 redis-server --version
if [[ "$verify_failure_cleanup" == true ]]; then
  printf 'injecting fixture runner exit 42 to verify failure cleanup\n'
  "${compose[@]}" run --rm --no-deps test-runner /bin/sh -ec 'exit 42'
else
  "${compose[@]}" run --rm --no-deps test-runner
fi
