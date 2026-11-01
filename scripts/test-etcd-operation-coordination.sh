#!/usr/bin/env bash
set -euo pipefail

repo_root="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
mode="${1:-safety}"
case "$mode" in safety|bench) ;; *) printf 'usage: %s [safety|bench]\n' "$0" >&2; exit 2;; esac
fixture_id="sandbox-coordination-test-$(date +%s)-$$"
network="$fixture_id"
members=()
cleanup() {
  if ((${#members[@]})); then docker rm -f "${members[@]}" >/dev/null 2>&1 || true; fi
  docker network rm "$network" >/dev/null 2>&1 || true
}
trap cleanup EXIT
docker network create "$network" >/dev/null
initial_cluster=""
for i in 1 2 3; do
  member="$fixture_id-$i"
  members+=("$member")
  initial_cluster="${initial_cluster:+$initial_cluster,}$member=http://$member:2380"
done
for member in "${members[@]}"; do
  docker run -d --name "$member" --network "$network" --cpus 1 --memory 512m \
    -p 127.0.0.1::2379 quay.io/coreos/etcd:v3.6.14 /usr/local/bin/etcd \
    --name "$member" --data-dir /tmp/etcd-data \
    --listen-client-urls http://0.0.0.0:2379 \
    --advertise-client-urls "http://$member:2379" \
    --listen-peer-urls http://0.0.0.0:2380 \
    --initial-advertise-peer-urls "http://$member:2380" \
    --initial-cluster "$initial_cluster" --initial-cluster-token "$fixture_id" \
    --auto-compaction-mode periodic --auto-compaction-retention 1m >/dev/null
done
endpoints=""
for member in "${members[@]}"; do
  endpoint="http://$(docker port "$member" 2379/tcp)"
  endpoints="${endpoints:+$endpoints,}$endpoint"
done
export TEST_ETCD_ENDPOINTS="$endpoints"
export TEST_ETCD_ENDPOINT="${endpoints%%,*}"
export TEST_ETCD_MEMBERS
TEST_ETCD_MEMBERS="$(IFS=,; printf '%s' "${members[*]}")"
ready=false
for _ in {1..60}; do
  if docker exec "${members[0]}" /usr/local/bin/etcdctl endpoint health >/dev/null 2>&1; then ready=true;break;fi
  sleep 0.25
done
if [[ "$ready" != true ]]; then printf 'test etcd did not become ready\n' >&2;exit 1;fi
cd "$repo_root"
printf 'Dedicated etcd fixture: 3 members, each 1 CPU / 512MiB; %s\n' "$mode"
if [[ "$mode" == safety ]]; then
  output="$(mktemp "${TMPDIR:-/tmp}/sandbox-coordination-safety.XXXXXX")"
  trap 'rm -f "$output"; cleanup' EXIT
  go test -race ./internal/storage/state/etcd ./internal/sandbox \
    -run 'TestSharedData|TestActiveChanges|TestActiveChangeHub' -count=1 -v 2>&1 | tee "$output"
  # The crash helper is deliberately skipped in the parent process, then run
  # as a real subprocess. Every other selected test must actually execute.
  if awk '/--- SKIP:/ && !/TestSharedDataEtcdCrashProcessChild/ {bad=1} END {exit !bad}' "$output"; then
    printf 'unexpected skipped acceptance test\n' >&2;exit 1
  fi
else
  go test ./internal/sandbox -run '^$' -bench '^BenchmarkSharedDataCoordination$' \
    -benchtime=10000x -count=1 -timeout=30m
fi
