#!/usr/bin/env bash
set -euo pipefail
repo=$(cd "$(dirname "$0")/.." && pwd)
own="$repo/.superpowers/sdd/2026-10-16-etcd-cold-create-service"
go_bin=/Users/dysodeng/go/go1.25.6/bin/go
epoch=${COLD_CREATE_EPOCH:?set a unique owned epoch}
mode=${1:-build}
fixture=${COLD_CREATE_FIXTURE:-inventory}
case "$fixture" in inventory) native_name=TestColdPreparationNativeFixture;; attempt) native_name=TestColdCreateAttemptNativeFixture;; attempt-close) native_name=TestColdCreateAttemptCloseNativeFixture;; settlement) native_name=TestColdPreparationSettlementNativeFixture;; *) exit 2;; esac
[[ "$epoch" =~ ^[a-z0-9-]{1,40}$ ]] || exit 1
[[ "$mode" == build || "$mode" == run ]] || exit 1
cd "$repo"
collector="$repo/scripts/test-cold-create-native.sh"
helper="$repo/scripts/cold-create-native-common.sh"
if [[ "$mode" == build ]]; then
  [[ ! -e "$own/native-$epoch-script.sha256" ]] || exit 1
  shasum -a 256 "$collector" "$helper" > "$own/native-$epoch-script.sha256"
else
  # Verify the wrapper and every sourced dependency before executing the helper.
  shasum -a 256 -c "$own/native-$epoch-script.sha256"
fi
source "$helper"
if [[ "$mode" == build ]]; then
  native_build runner.test ./internal/runtime/controlrunner test
  native_build etcd.test ./internal/storage/state/etcd test
  native_archive_scripts
  printf '%s\n' "$fixture" > "$own/native-$epoch-fixture.txt"
  shasum -a 256 "$own/native-$epoch-fixture.txt" >> "$own/native-$epoch-script-archive.sha256"
  printf 'COLD_CREATE_BUILD_VERIFIED %s\n' "$epoch"
  exit 0
fi
if [[ "$mode" == run ]]; then
  [[ $(cat "$own/native-$epoch-fixture.txt") == "$fixture" ]] || exit 1
  native_selector="^$native_name\$"
  image=sha256:684aa844d79d5b300db6586a25503607838055db7e7f480a50abb7c7ee7b4e32
  etcd="$own/etcd-linux-arm64"
  [[ $(shasum -a 256 "$etcd" | awk '{print $1}') == 117cc09981c0f1eb378e7520be6969756c2d6a762f376944e53f7bc9800c0c59 ]] || exit 1
  native_verify_archive "$own/native-$epoch-script.tar.gz" "$own/native-$epoch-script.sha256"
  shasum -a 256 -c "$own/native-$epoch-script-archive.sha256"
  for name in runner.test etcd.test; do
    shasum -a 256 -c "$own/native-$epoch-$name-binary.sha256"
  done
  [[ $(docker image inspect --format '{{.Id}} {{.Architecture}}' "$image") == "$image arm64" ]] || exit 1
  run="cold-create-$epoch"
  [[ ! -e "$own/native-$epoch.log" ]] || exit 1
  [[ -z $(docker ps -aq --filter label=sandbox.task=cold-create) ]] || exit 1
  id=$(docker create --name "$run" --label sandbox.task=cold-create --label "sandbox.run=$run" --network none --cgroupns private --read-only --tmpfs /tmp:rw,nosuid,nodev,size=268435456 --pids-limit 256 --memory 512m --cpus 2 --cap-drop ALL --security-opt no-new-privileges=true --user 0:0 --mount "type=bind,src=$own/native-$epoch-runner.test,dst=/fixture/runner.test,readonly" --mount "type=bind,src=$own/native-$epoch-etcd.test,dst=/fixture/etcd-tests,readonly" --mount "type=bind,src=$etcd,dst=/fixture/etcd,readonly" --env SANDBOX_COLD_CREATE_NATIVE=1 --env GOMAXPROCS=2 --entrypoint /fixture/runner.test "$image" -test.run "$native_selector" -test.v -test.timeout=100s)
  native_join_container
  [[ $(rg -c "^--- PASS: $native_name " "$own/native-$epoch.log") == 1 ]] || exit 1
  ! rg -q '^--- SKIP:' "$own/native-$epoch.log" || exit 1
  printf 'COLD_CREATE_NATIVE_FINISHED %s exact-owned-inventory-empty\n' "$epoch"
  exit 0
fi
