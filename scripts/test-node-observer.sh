#!/usr/bin/env bash
set -euo pipefail

# Native precursor only: no pull, installation, host PID/cgroup namespace, or
# host system mount. Each invocation owns exactly one labeled container.
repo=$(cd "$(dirname "$0")/.." && pwd)
own="$repo/.superpowers/sdd/2026-10-15-node-observer-physical-termination"
go_bin=${GO_BIN:-/Users/dysodeng/go/go1.25.6/bin/go}
epoch=${NODE_OBSERVER_EPOCH:?set a unique owned epoch}
[[ "$epoch" =~ ^[a-z0-9-]{1,40}$ ]]
image=sha256:684aa844d79d5b300db6586a25503607838055db7e7f480a50abb7c7ee7b4e32
etcd="$own/etcd-native-arm64"
[[ -x "$etcd" && -d "$own" ]]
[[ $(docker image inspect --format '{{.Id}} {{.Architecture}}' "$image") == "$image arm64" ]]
[[ $(shasum -a 256 "$etcd" | awk '{print $1}') == 117cc09981c0f1eb378e7520be6969756c2d6a762f376944e53f7bc9800c0c59 ]]
cd "$repo"
inputs="$own/native-$epoch-inputs.txt"
env GOOS=linux GOARCH=arm64 CGO_ENABLED=0 "$go_bin" list -deps -test -compiled -f '{{.Dir}}|{{join .CompiledGoFiles "|"}}' ./internal/runtime/controlrunner ./internal/nodeobserver | python3 -c 'import sys,os; [print(os.path.join(p[0],f)) for line in sys.stdin for p in [line.rstrip().split("|")] for f in p[1:] if f]' | sort -u > "$inputs"
xargs shasum -a 256 < "$inputs" > "$own/native-$epoch-before.sha256"
env GOOS=linux GOARCH=arm64 CGO_ENABLED=0 "$go_bin" test -c ./internal/runtime/controlrunner -o "$own/native-$epoch.test"
env GOOS=linux GOARCH=arm64 CGO_ENABLED=0 "$go_bin" test -c ./internal/nodeobserver -o "$own/native-$epoch-unit.test"
env GOOS=linux GOARCH=arm64 CGO_ENABLED=0 "$go_bin" list -deps -test -compiled -f '{{.Dir}}|{{join .CompiledGoFiles "|"}}' ./internal/runtime/controlrunner ./internal/nodeobserver | python3 -c 'import sys,os; [print(os.path.join(p[0],f)) for line in sys.stdin for p in [line.rstrip().split("|")] for f in p[1:] if f]' | sort -u > "$own/native-$epoch-post-inputs.txt"
cmp "$inputs" "$own/native-$epoch-post-inputs.txt"
xargs shasum -a 256 < "$inputs" > "$own/native-$epoch-after.sha256"
cmp "$own/native-$epoch-before.sha256" "$own/native-$epoch-after.sha256"
shasum -a 256 "$own/native-$epoch.test" "$own/native-$epoch-unit.test" > "$own/native-$epoch-binary.sha256"
printf 'NATIVE_BINARY_VERIFIED %s\n' "$epoch"
run="node-observer-$epoch"
[[ -z $(docker ps -aq --filter "label=sandbox.run=$run") ]]
id=$(docker create --name "$run" --label sandbox.task=node-observer-physical --label "sandbox.run=$run" --network none --cgroupns private --read-only --tmpfs /tmp:rw,nosuid,nodev,size=268435456 --pids-limit 256 --memory 512m --cpus 2 --cap-drop ALL --cap-add SYS_ADMIN --cap-add SYS_PTRACE --cap-add KILL --cap-add SETUID --cap-add SETGID --cap-add SETPCAP --security-opt no-new-privileges=true --user 0:0 --mount "type=bind,src=$own/native-$epoch.test,dst=/fixture/observer.test,readonly" --mount "type=bind,src=$own/native-$epoch-unit.test,dst=/fixture/unit.test,readonly" --mount "type=bind,src=$etcd,dst=/fixture/etcd,readonly" --env SANDBOX_NODE_OBSERVER_NATIVE=1 --env GOMAXPROCS=2 --entrypoint /bin/sh "$image" -ec '/fixture/unit.test -test.run "^TestNodeObserver(EnrollmentBoundary|OptionsRejectMissingAuthority)$" -test.v; exec /fixture/observer.test -test.run "^TestNodeObserverPhysicalNative(Unknown)?$" -test.v -test.timeout=65s')
cleanup() {
  if [[ -n ${id:-} ]]; then
    if [[ $(docker inspect --format '{{.State.Running}}' "$id") == true ]]; then docker kill "$id"; fi
    docker wait "$id"
    docker rm "$id"
    [[ -z $(docker ps -aq --filter "label=sandbox.run=$run") ]]
  fi
}
trap cleanup EXIT
[[ $(docker inspect --format '{{.HostConfig.CgroupnsMode}} {{.HostConfig.PidMode}} {{.HostConfig.NetworkMode}} {{index .Config.Labels "sandbox.run"}}' "$id") == "private  none $run" ]]
set +e
docker start -a "$id" > "$own/native-$epoch.log" 2>&1 &
attach_pid=$!
wait "$attach_pid"
attach_status=$?
container_status=$(docker wait "$id")
set -e
printf 'actual attach Wait=%s container Wait=%s\n' "$attach_status" "$container_status"
docker inspect --format '{{.Id}} {{.State.Running}} {{.State.ExitCode}} {{index .Config.Labels "sandbox.run"}}' "$id"
docker rm "$id"
id=
[[ -z $(docker ps -aq --filter "label=sandbox.run=$run") ]]
if [[ "$attach_status" != 0 || "$container_status" != 0 ]]; then exit 1; fi
[[ $(rg -c '^--- PASS: TestNodeObserverPhysicalNative ' "$own/native-$epoch.log") == 1 ]]
[[ $(rg -c '^--- PASS: TestNodeObserverPhysicalNativeUnknown ' "$own/native-$epoch.log") == 1 ]]
! rg -q '^--- SKIP:' "$own/native-$epoch.log"
