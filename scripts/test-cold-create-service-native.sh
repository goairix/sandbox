#!/usr/bin/env bash
set -euo pipefail
repo=$(cd "$(dirname "$0")/.." && pwd)
own="$repo/.superpowers/sdd/2026-10-16-etcd-cold-create-service"
go_bin=/Users/dysodeng/go/go1.25.6/bin/go
epoch=${COLD_SERVICE_EPOCH:?set a unique owned epoch}
mode=${1:?build or run}
[[ "$epoch" =~ ^[a-z0-9-]{1,40}$ ]] || exit 1
[[ "$mode" == build || "$mode" == run ]] || exit 1
cd "$repo"
collector="$repo/scripts/test-cold-create-service-native.sh"
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
  native_build launcher ./cmd/sandbox-launcher product
  native_archive_scripts
  printf 'COLD_SERVICE_BUILD_VERIFIED %s\n' "$epoch"
  exit 0
fi
if [[ "$mode" == run ]]; then
  native_selector='^TestColdCreateServiceNative$'
  image=sha256:684aa844d79d5b300db6586a25503607838055db7e7f480a50abb7c7ee7b4e32
  etcd="$own/etcd-linux-arm64"
  [[ $(shasum -a 256 "$etcd" | awk '{print $1}') == 117cc09981c0f1eb378e7520be6969756c2d6a762f376944e53f7bc9800c0c59 ]] || exit 1
  native_verify_archive "$own/native-$epoch-script.tar.gz" "$own/native-$epoch-script.sha256"
  shasum -a 256 -c "$own/native-$epoch-script-archive.sha256"
  for name in runner.test launcher; do
    shasum -a 256 -c "$own/native-$epoch-$name-binary.sha256"
  done
  [[ $(docker image inspect --format '{{.Id}} {{.Architecture}}' "$image") == "$image arm64" ]] || exit 1
  run="cold-create-service-$epoch"
  [[ ! -e "$own/native-$epoch.log" ]] || exit 1
  [[ -z $(docker ps -aq --filter label=sandbox.task=cold-create-service) ]] || exit 1
  id=$(docker create --name "$run" --label sandbox.task=cold-create-service --label "sandbox.run=$run" --network none --cgroupns private --read-only --tmpfs /tmp:rw,nosuid,nodev,size=268435456 --tmpfs /workspace:rw,nosuid,nodev,size=1048576,uid=0,gid=0,mode=0777 --pids-limit 256 --memory 512m --cpus 2 --cap-drop ALL --cap-add SYS_ADMIN --cap-add SYS_PTRACE --cap-add SYS_CHROOT --cap-add KILL --cap-add SETUID --cap-add SETGID --cap-add SETPCAP --security-opt no-new-privileges=true --user 0:0 --mount "type=bind,src=$own/native-$epoch-runner.test,dst=/fixture/runner.test,readonly" --mount "type=bind,src=$own/native-$epoch-launcher,dst=/usr/local/bin/sandbox-launcher,readonly" --mount "type=bind,src=$etcd,dst=/fixture/etcd,readonly" --env SANDBOX_COLD_CREATE_SERVICE_NATIVE=1 --env GOMAXPROCS=2 --entrypoint /fixture/runner.test "$image" -test.run "$native_selector" -test.v -test.timeout=100s)
  native_join_container
  [[ $(rg -c '^--- PASS: TestColdCreateServiceNative ' "$own/native-$epoch.log") == 1 ]] || exit 1
  [[ $(rg -c 'cold service original formal launcher Cmd.Wait=exit0' "$own/native-$epoch.log") == 2 ]] || exit 1
  ! rg -q '^--- SKIP:' "$own/native-$epoch.log" || exit 1
  printf 'COLD_SERVICE_NATIVE_FINISHED %s exact-owned-inventory-empty\n' "$epoch"
  exit 0
fi
