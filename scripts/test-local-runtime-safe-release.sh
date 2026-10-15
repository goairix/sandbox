#!/usr/bin/env bash
set -euo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
own="$repo/.superpowers/sdd/2026-10-16-local-runtime-safe-release"
go_bin=${GO_BIN:-/Users/dysodeng/go/go1.25.6/bin/go}
epoch=${LOCAL_RELEASE_EPOCH:?set a unique owned epoch}
mode=${1:-run}
selector=${LOCAL_RELEASE_SELECTOR:-'^TestLocalRuntimeReleaseNative$'}
case "$selector" in '^TestLocalRuntimeReleaseNative$'|'^TestLocalRuntimeReleaseNativeFaults$'|'^TestLocalRuntimeReleaseNativeACKs$'|'^TestLocalRuntimeReleaseNativeEffects$'|'^TestLocalRuntimeReleaseNativeClose$'|'^TestLocalRuntimeReleaseNativeBoundaries$'|'^TestLocalRuntimeReleaseNativeRecoveryACKs$') ;; *) exit 2;; esac
[[ "$epoch" =~ ^[a-z0-9-]{1,40}$ ]]
[[ "$mode" == build || "$mode" == run || "$mode" == host-red || "$mode" == host-green ]]
# The exact pre-existing approved image; never pull or install.
image=sha256:684aa844d79d5b300db6586a25503607838055db7e7f480a50abb7c7ee7b4e32
etcd="$own/etcd-linux-arm64"
[[ -x "$etcd" ]]
[[ $(shasum -a 256 "$etcd" | awk '{print $1}') == 117cc09981c0f1eb378e7520be6969756c2d6a762f376944e53f7bc9800c0c59 ]]
cd "$repo"
inputs() {
  local artifact=$1 target=$2 kind=$3 platform=$4
  if [[ "$kind" == test ]]; then
    env GOOS="$platform" GOARCH=arm64 CGO_ENABLED=0 "$go_bin" list -deps -test -compiled -json "$target"
  else
    env GOOS="$platform" GOARCH=arm64 CGO_ENABLED=0 "$go_bin" list -deps -compiled -json "$target"
  fi > "$artifact.json"
  python3 -c 'import sys,json,os
s=open(sys.argv[1]).read(); d=json.JSONDecoder(); paths=set(); fields="GoFiles CgoFiles CompiledGoFiles TestGoFiles XTestGoFiles SFiles CFiles CXXFiles MFiles HFiles FFiles SwigFiles SwigCXXFiles SysoFiles EmbedFiles TestEmbedFiles XTestEmbedFiles".split()
while s.strip():
 o,n=d.raw_decode(s.lstrip()); s=s.lstrip()[n:]
 for field in fields:
  for f in o.get(field,[]): paths.add(f if os.path.isabs(f) else os.path.join(o["Dir"],f))
for p in sorted(paths): print(p)' "$artifact.json" > "$artifact"
}
build() {
  local name=$1 target=$2 kind=$3
  local base="$own/native-$epoch-$name"
  inputs "$base-inputs.txt" "$target" "$kind" linux
  xargs shasum -a 256 < "$base-inputs.txt" > "$base-before.sha256"
  if [[ "$kind" == test ]]; then
    env GOOS=linux GOARCH=arm64 CGO_ENABLED=0 "$go_bin" test -c "$target" -o "$base"
  else
    env GOOS=linux GOARCH=arm64 CGO_ENABLED=0 "$go_bin" build -o "$base" "$target"
  fi
  inputs "$base-post-inputs.txt" "$target" "$kind" linux
  cmp "$base-inputs.txt" "$base-post-inputs.txt"
  xargs shasum -a 256 < "$base-post-inputs.txt" > "$base-after.sha256"
  cmp "$base-before.sha256" "$base-after.sha256"
  tar -czPf "$base-source.tar.gz" -T "$base-inputs.txt"
  shasum -a 256 "$base" "$base-source.tar.gz" > "$base-binary.sha256"
  "$go_bin" version -m "$base" > "$base-buildinfo.txt"
}
if [[ "$mode" == host-red || "$mode" == host-green ]]; then
  selector=${LOCAL_RELEASE_HOST_SELECTOR:-'^TestLocalRuntimeReleaseOpaqueBinding$'}
  case "$selector" in '^TestLocalRuntimeReleaseOpaqueBinding$'|'^TestLocalRuntimeReleaseDomainRecords$') ;; *) exit 2;; esac
  base="$own/host-$epoch"
  inputs "$base-inputs.txt" ./internal/storage/state/etcd test darwin
  xargs shasum -a 256 < "$base-inputs.txt" > "$base-before.sha256"
  set +e
  env CGO_ENABLED=0 "$go_bin" test ./internal/storage/state/etcd -run "$selector" -count=1
  status=$?
  set -e
  inputs "$base-post-inputs.txt" ./internal/storage/state/etcd test darwin
  cmp "$base-inputs.txt" "$base-post-inputs.txt"
  xargs shasum -a 256 < "$base-post-inputs.txt" > "$base-after.sha256"
  cmp "$base-before.sha256" "$base-after.sha256"
  exit "$status"
fi
if [[ "$mode" == build ]]; then
  build runner.test ./internal/runtime/controlrunner test
  build monitor ./cmd/sandbox-launcher product
  shasum -a 256 "$repo/scripts/test-local-runtime-safe-release.sh" > "$own/native-$epoch-script.sha256"
  printf 'LOCAL_RELEASE_BUILD_VERIFIED %s\n' "$epoch"
  exit 0
fi
shasum -a 256 -c "$own/native-$epoch-script.sha256"
for name in runner.test monitor; do
  shasum -a 256 -c "$own/native-$epoch-$name-binary.sha256"
done
[[ $(docker image inspect --format '{{.Id}} {{.Architecture}}' "$image") == "$image arm64" ]]
run="local-runtime-release-$epoch"
[[ -z $(docker ps -aq --filter label=sandbox.task=local-runtime-safe-release) ]]
id=$(docker create --name "$run" --label sandbox.task=local-runtime-safe-release --label "sandbox.run=$run" --network none --cgroupns private --read-only --tmpfs /tmp:rw,nosuid,nodev,size=268435456 --tmpfs /workspace:rw,nosuid,nodev,size=1048576,uid=1000,gid=1000,mode=0700 --pids-limit 256 --memory 512m --cpus 2 --cap-drop ALL --cap-add SYS_ADMIN --cap-add SYS_PTRACE --cap-add KILL --cap-add SETUID --cap-add SETGID --cap-add SETPCAP --security-opt no-new-privileges=true --user 0:0 --mount "type=bind,src=$own/native-$epoch-runner.test,dst=/fixture/runner.test,readonly" --mount "type=bind,src=$own/native-$epoch-monitor,dst=/fixture/monitor,readonly" --mount "type=bind,src=$etcd,dst=/fixture/etcd,readonly" --env SANDBOX_LOCAL_RELEASE_NATIVE=1 --env GOMAXPROCS=2 --entrypoint /fixture/runner.test "$image" -test.run "$selector" -test.v -test.timeout=150s)
started=false
attach_pid=
attach_waited=false
container_waited=false
attach_status=unknown
container_status=unknown
audit_container() {
  docker inspect --format '{"id":{{json .Id}},"run":{{json (index .Config.Labels "sandbox.run")}},"state":{"running":{{.State.Running}},"exit":{{.State.ExitCode}},"oom":{{.State.OOMKilled}},"started":{{json .State.StartedAt}},"finished":{{json .State.FinishedAt}}},"isolation":{"cgroupns":{{json .HostConfig.CgroupnsMode}},"pidmode":{{json .HostConfig.PidMode}},"network":{{json .HostConfig.NetworkMode}},"readonly":{{.HostConfig.ReadonlyRootfs}},"memory":{{.HostConfig.Memory}},"nano_cpus":{{.HostConfig.NanoCpus}},"pids":{{json .HostConfig.PidsLimit}},"cap_add":{{json .HostConfig.CapAdd}},"cap_drop":{{json .HostConfig.CapDrop}},"security":{{json .HostConfig.SecurityOpt}}},"mounts":[{{range $i,$m := .Mounts}}{{if $i}},{{end}}{"type":{{json $m.Type}},"source":{{json $m.Source}},"destination":{{json $m.Destination}},"rw":{{$m.RW}}}{{end}}]}' "$id"
}
refresh_started() {
  local at
  at=$(docker inspect --format '{{.State.StartedAt}}' "$id") || return 1
  if [[ "$at" != 0001-01-01T00:00:00Z ]]; then started=true; fi
}
cleanup() {
  local original=$?
  set +e
  if [[ -n ${id:-} ]]; then
    [[ $(docker inspect --format '{{index .Config.Labels "sandbox.run"}}' "$id") == "$run" ]] || exit 1
    if [[ $(docker inspect --format '{{.State.Running}}' "$id") == true ]]; then docker kill "$id"; fi
    if [[ -n "$attach_pid" && "$attach_waited" == false ]]; then
      kill -TERM "$attach_pid" 2>/dev/null || true
      wait "$attach_pid"
      attach_status=$?
      attach_waited=true
    fi
    refresh_started || original=1
    if [[ $(docker inspect --format '{{.State.Running}}' "$id") == true ]]; then docker kill "$id" || original=1; fi
    if [[ "$started" == true && "$container_waited" == false ]]; then
      container_waited=true
      container_status=$(docker wait "$id") || original=1
    fi
    audit_container > "$own/native-$epoch-container-final.json" || original=1
    printf 'cleanup original attach Wait=%s container Wait=%s started=%s\n' "$attach_status" "$container_status" "$started"
    docker rm "$id" || original=1
    [[ -z $(docker ps -aq --filter "label=sandbox.run=$run") ]] || exit 1
  fi
  exit "$original"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
[[ $(docker inspect --format '{{.HostConfig.CgroupnsMode}} {{.HostConfig.PidMode}} {{.HostConfig.NetworkMode}} {{index .Config.Labels "sandbox.run"}}' "$id") == "private  none $run" ]]
audit_container > "$own/native-$epoch-container.json"
set +e
docker start -a "$id" > "$own/native-$epoch.log" 2>&1 &
attach_pid=$!
wait "$attach_pid"
attach_status=$?
attach_waited=true
set -e
refresh_started
if [[ "$started" == true ]]; then
  container_waited=true
  container_status=$(docker wait "$id")
fi
printf 'actual attach Wait=%s container Wait=%s\n' "$attach_status" "$container_status"
audit_container > "$own/native-$epoch-container-final.json"
docker rm "$id"
id=
[[ -z $(docker ps -aq --filter "label=sandbox.run=$run") ]]
if [[ "$attach_status" != 0 || "$container_status" != 0 ]]; then exit 1; fi
if [[ "$selector" == '^TestLocalRuntimeReleaseNative$' ]]; then
  [[ $(rg -c '^--- PASS: TestLocalRuntimeReleaseNative ' "$own/native-$epoch.log") == 1 ]]
  [[ $(rg -c '^--- PASS: TestLocalRuntimeReleaseNativeWorker ' "$own/native-$epoch.log") == 1 ]]
elif [[ "$selector" == '^TestLocalRuntimeReleaseNativeFaults$' ]]; then
  [[ $(rg -c '^--- PASS: TestLocalRuntimeReleaseNativeFaults ' "$own/native-$epoch.log") == 1 ]]
  [[ $(rg -c '^--- PASS: TestLocalRuntimeReleaseNativeWorker ' "$own/native-$epoch.log") == 6 ]]
elif [[ "$selector" == '^TestLocalRuntimeReleaseNativeACKs$' ]]; then
  [[ $(rg -c '^--- PASS: TestLocalRuntimeReleaseNativeACKs ' "$own/native-$epoch.log") == 1 ]]
  [[ $(rg -c '^--- PASS: TestLocalRuntimeReleaseNativeWorker ' "$own/native-$epoch.log") == 3 ]]
elif [[ "$selector" == '^TestLocalRuntimeReleaseNativeEffects$' ]]; then
  [[ $(rg -c '^--- PASS: TestLocalRuntimeReleaseNativeEffects ' "$own/native-$epoch.log") == 1 ]]
  [[ $(rg -c '^--- PASS: TestLocalRuntimeReleaseNativeWorker ' "$own/native-$epoch.log") == 7 ]]
elif [[ "$selector" == '^TestLocalRuntimeReleaseNativeClose$' ]]; then
  [[ $(rg -c '^--- PASS: TestLocalRuntimeReleaseNativeClose ' "$own/native-$epoch.log") == 1 ]]
  [[ $(rg -c '^--- PASS: TestLocalRuntimeReleaseNativeWorker ' "$own/native-$epoch.log") == 1 ]]
elif [[ "$selector" == '^TestLocalRuntimeReleaseNativeBoundaries$' ]]; then
  [[ $(rg -c '^--- PASS: TestLocalRuntimeReleaseNativeBoundaries ' "$own/native-$epoch.log") == 1 ]]
  [[ $(rg -c '^--- PASS: TestLocalRuntimeReleaseNativeWorker ' "$own/native-$epoch.log") == 7 ]]
else
  [[ $(rg -c '^--- PASS: TestLocalRuntimeReleaseNativeRecoveryACKs ' "$own/native-$epoch.log") == 1 ]]
  [[ $(rg -c '^--- PASS: TestLocalRuntimeReleaseNativeWorker ' "$own/native-$epoch.log") == 2 ]]
fi
! rg -q '^--- SKIP:' "$own/native-$epoch.log"
printf 'LOCAL_RELEASE_NATIVE_FINISHED %s exact-owned-inventory-empty\n' "$epoch"
