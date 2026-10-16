#!/usr/bin/env bash
set -euo pipefail
repo=$(cd "$(dirname "$0")/.." && pwd)
own="$repo/.superpowers/sdd/2026-10-16-etcd-cold-create-service"
go_bin=/Users/dysodeng/go/go1.25.6/bin/go
epoch=${COLD_SERVICE_EPOCH:?set a unique owned epoch}
mode=${1:?build or run}
platform=${COLD_SERVICE_GOOS:-darwin}
[[ "$epoch" =~ ^[a-z0-9-]{1,40}$ ]] || exit 1
[[ "$mode" == build || "$mode" == run ]] || exit 1
[[ "$platform" == darwin || "$platform" == linux && "$mode" == vet ]] || exit 1
target=${COLD_SERVICE_TARGET:-./cmd/sandbox-launcher}
case "$target" in ./internal/runtime/launcherconfig|./internal/kubecontract|./internal/runtime/kubernetes|./internal/runtime/controltransport|./internal/nodeobserver|./cmd/sandbox-launcher|./internal/runtime/controlrunner) ;; *) exit 2;; esac
selector=${COLD_SERVICE_SELECTOR:-'^TestEnrolledProvision'}
cd "$repo"
inputs() {
  local artifact=$1 input_target=${2:-$target} kind=${3:-test} input_platform=${4:-$platform}
  if [[ "$mode" == race ]]; then
    env GOOS="$input_platform" GOARCH=arm64 CGO_ENABLED=1 "$go_bin" list -race -deps -test -compiled -json "$input_target" > "$artifact.json"
  elif [[ "$kind" == product ]]; then
    env GOOS="$input_platform" GOARCH=arm64 CGO_ENABLED=0 "$go_bin" list -deps -compiled -json "$input_target" > "$artifact.json"
  else
    env GOOS="$input_platform" GOARCH=arm64 CGO_ENABLED=0 "$go_bin" list -deps -test -compiled -json "$input_target" > "$artifact.json"
  fi
  python3 -c 'import sys,json,os
s=open(sys.argv[1]).read(); d=json.JSONDecoder(); paths=set(); fields="GoFiles CgoFiles CompiledGoFiles TestGoFiles XTestGoFiles SFiles CFiles CXXFiles MFiles HFiles FFiles SwigFiles SwigCXXFiles SysoFiles EmbedFiles TestEmbedFiles XTestEmbedFiles".split()
while s.strip():
 o,n=d.raw_decode(s.lstrip()); s=s.lstrip()[n:]
 for field in fields:
  for f in o.get(field,[]): paths.add(f if os.path.isabs(f) else os.path.join(o["Dir"],f))
for p in sorted(paths): print(p)' "$artifact.json" > "$artifact"
}
native_build() {
  local name=$1 build_target=$2 kind=$3 base="$own/native-$epoch-$1"
  [[ ! -e "$base-inputs.txt.json" && ! -e "$base" ]] || exit 1
  inputs "$base-inputs.txt" "$build_target" "$kind" linux
  xargs shasum -a 256 < "$base-inputs.txt" > "$base-before.sha256"
  set +e
  if [[ "$kind" == test ]]; then
    env GOOS=linux GOARCH=arm64 CGO_ENABLED=0 "$go_bin" test -c "$build_target" -o "$base" > "$base-build.log" 2>&1
  else
    env GOOS=linux GOARCH=arm64 CGO_ENABLED=0 "$go_bin" build -o "$base" "$build_target" > "$base-build.log" 2>&1
  fi
  local status=$?
  set -e
  inputs "$base-post-inputs.txt" "$build_target" "$kind" linux
  cmp "$base-inputs.txt" "$base-post-inputs.txt"
  xargs shasum -a 256 < "$base-post-inputs.txt" > "$base-after.sha256"
  cmp "$base-before.sha256" "$base-after.sha256"
  [[ "$status" == 0 ]] || return "$status"
  COPYFILE_DISABLE=1 tar -czPf "$base-source.tar.gz" -T "$base-inputs.txt"
  python3 - "$base-source.tar.gz" "$base-before.sha256" <<'PY'
import hashlib,sys,tarfile
expected={}
for line in open(sys.argv[2]):
 digest,path=line.rstrip("\n").split("  ",1); expected[path]=digest
with tarfile.open(sys.argv[1]) as archive:
 regular=[m for m in archive.getmembers() if m.isfile()]
 members={m.name:m for m in regular}
 assert len(regular)==len(members)
 assert set(members)==set(expected)
 for path,digest in expected.items():
  assert hashlib.sha256(archive.extractfile(members[path]).read()).hexdigest()==digest,path
PY
  shasum -a 256 "$base" "$base-source.tar.gz" > "$base-binary.sha256"
  "$go_bin" version -m "$base" > "$base-buildinfo.txt"
}
if [[ "$mode" == build ]]; then
  native_build runner.test ./internal/runtime/controlrunner test
  native_build launcher ./cmd/sandbox-launcher product
  shasum -a 256 "$repo/scripts/test-cold-create-service-native.sh" > "$own/native-$epoch-script.sha256"
  COPYFILE_DISABLE=1 tar -czPf "$own/native-$epoch-script.tar.gz" "$repo/scripts/test-cold-create-service-native.sh"
  python3 - "$own/native-$epoch-script.tar.gz" "$own/native-$epoch-script.sha256" <<'PYSCRIPT'
import hashlib,sys,tarfile
digest,path=open(sys.argv[2]).read().rstrip("\n").split("  ",1)
with tarfile.open(sys.argv[1]) as archive:
 members=[m for m in archive.getmembers() if m.isfile()]
 assert len(members)==1 and members[0].name==path
 assert hashlib.sha256(archive.extractfile(members[0]).read()).hexdigest()==digest
PYSCRIPT
  shasum -a 256 "$own/native-$epoch-script.tar.gz" > "$own/native-$epoch-script-archive.sha256"
  printf 'COLD_SERVICE_BUILD_VERIFIED %s\n' "$epoch"
  exit 0
fi
if [[ "$mode" == run ]]; then
  native_selector='^TestColdCreateServiceNative$'
  image=sha256:684aa844d79d5b300db6586a25503607838055db7e7f480a50abb7c7ee7b4e32
  etcd="$own/etcd-linux-arm64"
  [[ $(shasum -a 256 "$etcd" | awk '{print $1}') == 117cc09981c0f1eb378e7520be6969756c2d6a762f376944e53f7bc9800c0c59 ]] || exit 1
  shasum -a 256 -c "$own/native-$epoch-script.sha256"
  shasum -a 256 -c "$own/native-$epoch-script-archive.sha256"
  for name in runner.test launcher; do
    shasum -a 256 -c "$own/native-$epoch-$name-binary.sha256"
  done
  [[ $(docker image inspect --format '{{.Id}} {{.Architecture}}' "$image") == "$image arm64" ]] || exit 1
  run="cold-create-service-$epoch"
  [[ ! -e "$own/native-$epoch.log" ]] || exit 1
  [[ -z $(docker ps -aq --filter label=sandbox.task=cold-create-service) ]] || exit 1
  id=$(docker create --name "$run" --label sandbox.task=cold-create-service --label "sandbox.run=$run" --network none --cgroupns private --read-only --tmpfs /tmp:rw,nosuid,nodev,size=268435456 --tmpfs /workspace:rw,nosuid,nodev,size=1048576,uid=0,gid=0,mode=0777 --pids-limit 256 --memory 512m --cpus 2 --cap-drop ALL --cap-add SYS_ADMIN --cap-add SYS_PTRACE --cap-add SYS_CHROOT --cap-add KILL --cap-add SETUID --cap-add SETGID --cap-add SETPCAP --security-opt no-new-privileges=true --user 0:0 --mount "type=bind,src=$own/native-$epoch-runner.test,dst=/fixture/runner.test,readonly" --mount "type=bind,src=$own/native-$epoch-launcher,dst=/usr/local/bin/sandbox-launcher,readonly" --mount "type=bind,src=$etcd,dst=/fixture/etcd,readonly" --env SANDBOX_COLD_CREATE_SERVICE_NATIVE=1 --env GOMAXPROCS=2 --entrypoint /fixture/runner.test "$image" -test.run "$native_selector" -test.v -test.timeout=100s)
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
  [[ $(docker inspect --format '{{.HostConfig.CgroupnsMode}} {{.HostConfig.PidMode}} {{.HostConfig.NetworkMode}} {{index .Config.Labels "sandbox.run"}}' "$id") == "private  none $run" ]] || exit 1
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
  [[ -z $(docker ps -aq --filter "label=sandbox.run=$run") ]] || exit 1
  [[ "$attach_status" == 0 && "$container_status" == 0 ]] || exit 1
  [[ $(rg -c '^--- PASS: TestColdCreateServiceNative ' "$own/native-$epoch.log") == 1 ]] || exit 1
  [[ $(rg -c 'cold service original formal launcher Cmd.Wait=exit0' "$own/native-$epoch.log") == 2 ]] || exit 1
  ! rg -q '^--- SKIP:' "$own/native-$epoch.log" || exit 1
  printf 'COLD_SERVICE_NATIVE_FINISHED %s exact-owned-inventory-empty\n' "$epoch"
  exit 0
fi
