#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
fixture_file="$repo_root/testdata/etcd-state/compose.yaml"
fixture_project="sandbox-etcd-state-test-$$-$(date +%s)"
compose=(docker compose --env-file /dev/null -p "$fixture_project" -f "$fixture_file")

cleanup() {
  result=$?
  trap - EXIT INT TERM
  if [ "$result" -ne 0 ]; then
    "${compose[@]}" logs --tail 80 >&2 || true
  fi
  "${compose[@]}" down --volumes --remove-orphans >/dev/null || true
  exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

"${compose[@]}" up -d --wait --wait-timeout 90
endpoints=()
containers=()
for member in etcd-1 etcd-2 etcd-3; do
  address=$("${compose[@]}" port "$member" 2379)
  endpoints+=("http://$address")
  containers+=("$("${compose[@]}" ps -q "$member")")
done
TEST_ETCD_ENDPOINTS=$(IFS=,; printf '%s' "${endpoints[*]}")
TEST_ETCD_CONTAINERS=$(IFS=,; printf '%s' "${containers[*]}")
TEST_ETCD_FOREIGN_ENDPOINT="http://$("${compose[@]}" port foreign 2379)"
export TEST_ETCD_ENDPOINTS TEST_ETCD_CONTAINERS TEST_ETCD_FOREIGN_ENDPOINT
cd "$repo_root"
go test -race -count=1 "$@" ./internal/storage/state/etcd
