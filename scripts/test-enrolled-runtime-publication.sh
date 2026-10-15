#!/usr/bin/env bash
set -euo pipefail

# Only this task's new finite cases. No pull/install, host namespaces, or old
# observer/local-sync matrix; every process belongs to one exact labeled fixture.
repo=$(cd "$(dirname "$0")/.." && pwd)
own="$repo/.superpowers/sdd/2026-10-15-enrolled-runtime-publication"
go_bin=${GO_BIN:-/Users/dysodeng/go/go1.25.6/bin/go}
epoch=${ENROLLED_PUBLICATION_EPOCH:?set a unique owned epoch}
mode=${1:-run}
selector=${ENROLLED_PUBLICATION_SELECTOR:-'^TestEnrolledSupervisorNative$'}
[[ "$epoch" =~ ^[a-z0-9-]{1,40}$ ]]
[[ "$mode" == build || "$mode" == run ]]
case "$selector" in '^TestEnrolledSupervisorNative$'|'^TestEnrolledSupervisorNativeJournal$'|'^TestEnrolledSupervisorNativeRefusal$'|'^TestEnrolledPublicationNativeFixture$') ;; *) exit 2;; esac
image=sha256:684aa844d79d5b300db6586a25503607838055db7e7f480a50abb7c7ee7b4e32
etcd="$own/etcd-native-arm64"
[[ -x "$etcd" ]]
[[ $(shasum -a 256 "$etcd" | awk '{print $1}') == 117cc09981c0f1eb378e7520be6969756c2d6a762f376944e53f7bc9800c0c59 ]]
cd "$repo"
inputs() {
  local artifact=$1 target=$2 kind=$3
  if [[ "$kind" == test ]]; then
    env GOOS=linux GOARCH=arm64 CGO_ENABLED=0 "$go_bin" list -deps -test -compiled -f '{{.Dir}}|{{join .CompiledGoFiles "|"}}|{{join .SFiles "|"}}|{{join .EmbedFiles "|"}}' "$target"
  else
    env GOOS=linux GOARCH=arm64 CGO_ENABLED=0 "$go_bin" list -deps -compiled -f '{{.Dir}}|{{join .CompiledGoFiles "|"}}|{{join .SFiles "|"}}|{{join .EmbedFiles "|"}}' "$target"
  fi | python3 -c 'import sys,os; [print(os.path.join(p[0],f)) for line in sys.stdin for p in [line.rstrip().split("|")] for f in p[1:] if f]' | sort -u > "$artifact"
}
build() {
  local name=$1 target=$2 kind=$3
  local base="$own/native-$epoch-$name"
  inputs "$base-inputs.txt" "$target" "$kind"
  xargs shasum -a 256 < "$base-inputs.txt" > "$base-before.sha256"
  if [[ "$kind" == test ]]; then
    env GOOS=linux GOARCH=arm64 CGO_ENABLED=0 "$go_bin" test -c "$target" -o "$base"
  else
    env GOOS=linux GOARCH=arm64 CGO_ENABLED=0 "$go_bin" build -o "$base" "$target"
  fi
  inputs "$base-post-inputs.txt" "$target" "$kind"
  cmp "$base-inputs.txt" "$base-post-inputs.txt"
  xargs shasum -a 256 < "$base-inputs.txt" > "$base-after.sha256"
  cmp "$base-before.sha256" "$base-after.sha256"
  tar -czPf "$base-source.tar.gz" -T "$base-inputs.txt"
  shasum -a 256 "$base" "$base-source.tar.gz" > "$base-binary.sha256"
  "$go_bin" version -m "$base" > "$base-buildinfo.txt"
}
if [[ "$mode" == build ]]; then
  build runner.test ./internal/runtime/controlrunner test
  build etcd.test ./internal/storage/state/etcd test
  build monitor ./cmd/sandbox-launcher product
  shasum -a 256 "$repo/scripts/test-enrolled-runtime-publication.sh" > "$own/native-$epoch-script.sha256"
  printf 'ENROLLED_NATIVE_BUILD_VERIFIED %s\n' "$epoch"
  exit 0
fi
shasum -a 256 -c "$own/native-$epoch-script.sha256"
for name in runner.test etcd.test monitor; do
  shasum -a 256 -c "$own/native-$epoch-$name-binary.sha256"
done
[[ $(docker image inspect --format '{{.Id}} {{.Architecture}}' "$image") == "$image arm64" ]]
run="enrolled-publication-$epoch"
[[ -z $(docker ps -aq --filter "label=sandbox.run=$run") ]]
id=$(docker create --name "$run" --label sandbox.task=enrolled-runtime-publication --label "sandbox.run=$run" --network none --cgroupns private --read-only --tmpfs /tmp:rw,nosuid,nodev,size=268435456 --pids-limit 256 --memory 512m --cpus 2 --cap-drop ALL --cap-add SYS_ADMIN --cap-add SYS_PTRACE --cap-add KILL --cap-add SETUID --cap-add SETGID --cap-add SETPCAP --security-opt no-new-privileges=true --user 0:0 --mount "type=bind,src=$own/native-$epoch-runner.test,dst=/fixture/runner.test,readonly" --mount "type=bind,src=$own/native-$epoch-etcd.test,dst=/fixture/etcd-tests,readonly" --mount "type=bind,src=$own/native-$epoch-monitor,dst=/fixture/monitor,readonly" --mount "type=bind,src=$etcd,dst=/fixture/etcd,readonly" --env SANDBOX_ENROLLED_PUBLICATION_NATIVE=1 --env GOMAXPROCS=2 --entrypoint /fixture/runner.test "$image" -test.run "$selector" -test.v -test.timeout=110s)
cleanup() {
  local original=$?
  if [[ -n ${id:-} ]]; then
    [[ $(docker inspect --format '{{index .Config.Labels "sandbox.run"}}' "$id") == "$run" ]] || exit 1
    if [[ $(docker inspect --format '{{.State.Running}}' "$id") == true ]]; then docker kill "$id"; fi
    docker wait "$id"
    docker rm "$id"
    [[ -z $(docker ps -aq --filter "label=sandbox.run=$run") ]] || exit 1
  fi
  exit "$original"
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
if [[ "$selector" == '^TestEnrolledSupervisorNativeRefusal$' ]]; then
  [[ $(rg -c '^--- PASS: TestEnrolledSupervisorNativeRefusal ' "$own/native-$epoch.log") == 1 ]]
elif [[ "$selector" == '^TestEnrolledSupervisorNativeJournal$' ]]; then
  [[ $(rg -c '^--- PASS: TestEnrolledSupervisorNativeJournal ' "$own/native-$epoch.log") == 1 ]]
elif [[ "$selector" == '^TestEnrolledSupervisorNative$' ]]; then
  [[ $(rg -c '^--- PASS: TestEnrolledSupervisorNative ' "$own/native-$epoch.log") == 1 ]]
else
  [[ $(rg -c '^--- PASS: TestEnrolledPublicationNativeFixture ' "$own/native-$epoch.log") == 1 ]]
fi
if [[ "$selector" == '^TestEnrolledSupervisorNative$' || "$selector" == '^TestEnrolledPublicationNativeFixture$' ]]; then
  [[ $(rg -c '^--- PASS: TestEnrolledPublicationNative ' "$own/native-$epoch.log") == 1 ]]
fi
! rg -q '^--- SKIP:' "$own/native-$epoch.log"
printf 'ENROLLED_NATIVE_FINISHED %s exact-owned-inventory-empty\n' "$epoch"
