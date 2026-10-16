#!/usr/bin/env bash
set -euo pipefail
repo=$(cd "$(dirname "$0")/.." && pwd)
own="$repo/.superpowers/sdd/2026-10-16-etcd-cold-runtime-quota-return"
go_bin=/Users/dysodeng/go/go1.25.6/bin/go
epoch=${COLD_QUOTA_EPOCH:?set a unique owned epoch}
mode=${1:?build or run}
fixture=${COLD_QUOTA_FIXTURE:-metadata}
[[ "$epoch" =~ ^[a-z0-9-]{1,40}$ ]] || exit 1
[[ "$mode" == build || "$mode" == run ]] || exit 1
case "$fixture" in metadata) native_name=TestColdRuntimeQuotaReturnNativeFixture;; fullchain) native_name=TestColdRuntimeQuotaReturnNativeFullchain;; *) exit 2;; esac
cd "$repo"
collector="$repo/scripts/test-cold-quota-return-native.sh"
helper="$repo/scripts/cold-create-native-common.sh"
if [[ "$mode" == build ]]; then
  [[ ! -e "$own/native-$epoch-script.sha256" ]] || exit 1
  shasum -a 256 "$collector" "$helper" > "$own/native-$epoch-script.sha256"
else
  shasum -a 256 -c "$own/native-$epoch-script.sha256"
fi
shasum -a 256 -c "$own/native-$epoch-script.sha256"
source "$helper"
if [[ "$mode" == run ]]; then
  native_verify_archive "$own/native-$epoch-script.tar.gz" "$own/native-$epoch-script.sha256"
fi
verify_elf() {
  python3 -c 'import sys,struct
b=open(sys.argv[1],"rb").read(20)
assert b[:6]==b"\x7fELF\x02\x01" and struct.unpack("<H",b[18:20])[0]==183' "$1"
  "$go_bin" version -m "$1" > "$2"
  rg -q ': go1\.25\.6$' "$2"
  rg -q 'GOOS=linux' "$2"
  rg -q 'GOARCH=arm64' "$2"
}
names=(runner.test)
if [[ "$fixture" == metadata ]]; then names+=(etcd.test); else names+=(monitor); fi
if [[ "$mode" == build ]]; then
  native_build runner.test ./internal/runtime/controlrunner test
  if [[ "$fixture" == metadata ]]; then
    native_build etcd.test ./internal/storage/state/etcd test
  else
    native_build monitor ./cmd/sandbox-launcher product
  fi
  for name in "${names[@]}"; do
    verify_elf "$own/native-$epoch-$name" "$own/native-$epoch-$name-elf-buildinfo.txt"
  done
  native_archive_scripts
  printf '%s\n' "$fixture" > "$own/native-$epoch-fixture.txt"
  shasum -a 256 "$own/native-$epoch-fixture.txt" >> "$own/native-$epoch-script-archive.sha256"
  printf 'COLD_QUOTA_BUILD_VERIFIED %s\n' "$epoch"
  exit 0
fi
[[ $(< "$own/native-$epoch-fixture.txt") == "$fixture" ]] || exit 1
shasum -a 256 -c "$own/native-$epoch-script-archive.sha256"
for name in "${names[@]}"; do
  shasum -a 256 -c "$own/native-$epoch-$name-binary.sha256"
  native_verify_archive "$own/native-$epoch-$name-source.tar.gz" "$own/native-$epoch-$name-before.sha256"
  verify_elf "$own/native-$epoch-$name" "$own/native-$epoch-$name-run-elf-buildinfo.txt"
done
image=sha256:684aa844d79d5b300db6586a25503607838055db7e7f480a50abb7c7ee7b4e32
etcd="$own/etcd-linux-arm64"
[[ $(shasum -a 256 "$etcd" | awk '{print $1}') == 117cc09981c0f1eb378e7520be6969756c2d6a762f376944e53f7bc9800c0c59 ]] || exit 1
[[ $(docker image inspect --format '{{.Id}} {{.Architecture}}' "$image") == "$image arm64" ]] || exit 1
run="cold-quota-return-$epoch"
[[ ! -e "$own/native-$epoch.log" ]] || exit 1
[[ -z $(docker ps -aq --filter label=sandbox.task=cold-quota-return) ]] || exit 1
if [[ "$fixture" == metadata ]]; then
  id=$(docker create --name "$run" --label sandbox.task=cold-quota-return --label "sandbox.run=$run" --network none --cgroupns private --read-only --tmpfs /tmp:rw,nosuid,nodev,size=268435456 --pids-limit 256 --memory 512m --cpus 2 --cap-drop ALL --security-opt no-new-privileges=true --user 0:0 --mount "type=bind,src=$own/native-$epoch-runner.test,dst=/fixture/runner.test,readonly" --mount "type=bind,src=$own/native-$epoch-etcd.test,dst=/fixture/etcd-tests,readonly" --mount "type=bind,src=$etcd,dst=/fixture/etcd,readonly" --env SANDBOX_COLD_QUOTA_NATIVE=1 --env SANDBOX_COLD_CREATE_NATIVE=1 --env GOMAXPROCS=2 --entrypoint /fixture/runner.test "$image" -test.run "^$native_name\$" -test.v -test.timeout=100s)
else
  id=$(docker create --name "$run" --label sandbox.task=cold-quota-return --label "sandbox.run=$run" --network none --cgroupns private --read-only --tmpfs /tmp:rw,nosuid,nodev,size=268435456 --tmpfs /workspace:rw,nosuid,nodev,size=1048576,uid=1000,gid=1000,mode=0700 --pids-limit 256 --memory 512m --cpus 2 --cap-drop ALL --cap-add SYS_ADMIN --cap-add SYS_PTRACE --cap-add KILL --cap-add SETUID --cap-add SETGID --cap-add SETPCAP --security-opt no-new-privileges=true --user 0:0 --mount "type=bind,src=$own/native-$epoch-runner.test,dst=/fixture/runner.test,readonly" --mount "type=bind,src=$own/native-$epoch-monitor,dst=/fixture/monitor,readonly" --mount "type=bind,src=$etcd,dst=/fixture/etcd,readonly" --env SANDBOX_COLD_QUOTA_NATIVE=1 --env SANDBOX_LOCAL_RELEASE_NATIVE=1 --env GOMAXPROCS=2 --entrypoint /fixture/runner.test "$image" -test.run "^$native_name\$" -test.v -test.timeout=150s)
fi
native_join_container
[[ $(rg -c "^--- PASS: $native_name " "$own/native-$epoch.log") == 1 ]] || exit 1
! rg -q '^--- SKIP:' "$own/native-$epoch.log" || exit 1
printf 'COLD_QUOTA_NATIVE_FINISHED %s exact-owned-inventory-empty\n' "$epoch"
