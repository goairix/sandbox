#!/usr/bin/env bash
# Shared by the cold lifecycle collectors. Wrappers own all
# selectors, build targets and literal docker-create security/mount arguments.

native_input_paths() {
  local artifact=$1
  python3 -c 'import sys,json,os
s=open(sys.argv[1]).read(); d=json.JSONDecoder(); paths=set(); fields="GoFiles CgoFiles CompiledGoFiles TestGoFiles XTestGoFiles SFiles CFiles CXXFiles MFiles HFiles FFiles SwigFiles SwigCXXFiles SysoFiles EmbedFiles TestEmbedFiles XTestEmbedFiles".split()
while s.strip():
 o,n=d.raw_decode(s.lstrip()); s=s.lstrip()[n:]
 for field in fields:
  for f in o.get(field,[]): paths.add(f if os.path.isabs(f) else os.path.join(o["Dir"],f))
for p in sorted(paths): print(p)' "$artifact.json" > "$artifact"
}
inputs() {
  local artifact=$1 input_target=$2 kind=$3
  if [[ "$kind" == product ]]; then
    env GOOS=linux GOARCH=arm64 CGO_ENABLED=0 "$go_bin" list -deps -compiled -json "$input_target" > "$artifact.json"
  else
    env GOOS=linux GOARCH=arm64 CGO_ENABLED=0 "$go_bin" list -deps -test -compiled -json "$input_target" > "$artifact.json"
  fi
  native_input_paths "$artifact"
}
native_verify_archive() {
  python3 - "$1" "$2" <<'PY'
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
}
native_build() {
  local name=$1 build_target=$2 kind=$3 base="$own/native-$epoch-$1"
  [[ ! -e "$base-inputs.txt.json" && ! -e "$base" ]] || exit 1
  inputs "$base-inputs.txt" "$build_target" "$kind"
  xargs shasum -a 256 < "$base-inputs.txt" > "$base-before.sha256"
  set +e
  if [[ "$kind" == test ]]; then
    env GOOS=linux GOARCH=arm64 CGO_ENABLED=0 "$go_bin" test -c "$build_target" -o "$base" > "$base-build.log" 2>&1
  else
    env GOOS=linux GOARCH=arm64 CGO_ENABLED=0 "$go_bin" build -o "$base" "$build_target" > "$base-build.log" 2>&1
  fi
  local status=$?
  set -e
  inputs "$base-post-inputs.txt" "$build_target" "$kind"
  cmp "$base-inputs.txt" "$base-post-inputs.txt"
  xargs shasum -a 256 < "$base-post-inputs.txt" > "$base-after.sha256"
  cmp "$base-before.sha256" "$base-after.sha256"
  [[ "$status" == 0 ]] || return "$status"
  COPYFILE_DISABLE=1 tar -czPf "$base-source.tar.gz" -T "$base-inputs.txt"
  native_verify_archive "$base-source.tar.gz" "$base-before.sha256"
  shasum -a 256 "$base" "$base-source.tar.gz" > "$base-binary.sha256"
  "$go_bin" version -m "$base" > "$base-buildinfo.txt"
}
native_archive_scripts() {
  shasum -a 256 "$collector" "$helper" > "$own/native-$epoch-script-post.sha256"
  cmp "$own/native-$epoch-script.sha256" "$own/native-$epoch-script-post.sha256"
  COPYFILE_DISABLE=1 tar -czPf "$own/native-$epoch-script.tar.gz" "$collector" "$helper"
  native_verify_archive "$own/native-$epoch-script.tar.gz" "$own/native-$epoch-script.sha256"
  shasum -a 256 -c "$own/native-$epoch-script.sha256"
  shasum -a 256 "$own/native-$epoch-script.tar.gz" > "$own/native-$epoch-script-archive.sha256"
}

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

native_join_container() {
  started=false
  attach_pid=
  attach_waited=false
  container_waited=false
  attach_status=unknown
  container_status=unknown
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
}
