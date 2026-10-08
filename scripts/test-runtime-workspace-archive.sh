#!/usr/bin/env bash
# Task1 admission-only physical fixture; binary must be frozen and precompiled.
set -euo pipefail
image='gcr.io/etcd-development/etcd:v3.6.15@sha256:5ed4e32061aa061f970d6500629213e8579ef17571dcf9789074966acb9afad1'
digest='gcr.io/etcd-development/etcd@sha256:5ed4e32061aa061f970d6500629213e8579ef17571dcf9789074966acb9afad1'
project="sandbox-runtime-workspace-archive-$$-$(date +%s)"
fixture=runtime-workspace-archive
repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
[[ $# == 3 && -n "$1" && "$2" == admission && -f "$3" ]] || { echo 'usage: test-runtime-workspace-archive.sh EVIDENCE_DIRECTORY admission PRECOMPILED_BINARY' >&2; exit 2; }
mkdir -p "$1"
evidence=$(cd "$1" && pwd)
containers=()
volumes=()
cleanup() {
  local original=$? failed=0 name labels remaining kind
  trap - EXIT INT TERM
  for name in ${containers[@]+"${containers[@]}"}; do
    if labels=$(docker inspect --format '{{index .Config.Labels "sandbox.test.project"}} {{index .Config.Labels "sandbox.test.fixture"}}' "$name" 2>/dev/null) && [[ "$labels" == "$project $fixture" ]]; then
      docker rm -f "$name" >>"$evidence/cleanup.log" 2>&1 || failed=1
    else failed=1; echo "refused or unavailable container $name" >>"$evidence/cleanup.log"; fi
  done
  for name in ${volumes[@]+"${volumes[@]}"}; do
    if labels=$(docker volume inspect --format '{{index .Labels "sandbox.test.project"}} {{index .Labels "sandbox.test.fixture"}}' "$name" 2>/dev/null) && [[ "$labels" == "$project $fixture" ]]; then
      docker volume rm "$name" >>"$evidence/cleanup.log" 2>&1 || failed=1
    else failed=1; echo "refused or unavailable volume $name" >>"$evidence/cleanup.log"; fi
  done
  for kind in container volume network; do
    case "$kind" in
      container) remaining=$(docker ps -aq --filter "label=sandbox.test.project=$project") || failed=1;;
      volume) remaining=$(docker volume ls -q --filter "label=sandbox.test.project=$project") || failed=1;;
      network) remaining=$(docker network ls -q --filter "label=sandbox.test.project=$project") || failed=1;;
    esac
    printf '%s inventory (empty means query succeeded): %s\n' "$kind" "$remaining" >>"$evidence/cleanup.log"
    [[ -z "$remaining" ]] || failed=1
  done
  (( original == 0 && failed == 0 )) || { echo "archive fixture failed; evidence $evidence" >&2; exit 1; }
  echo "archive fixture passed; owned inventories empty; evidence $evidence"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
docker image inspect --format '{"id":{{json .Id}},"digests":{{json .RepoDigests}},"os":{{json .Os}},"architecture":{{json .Architecture}}}' "$image" >"$evidence/image.json"
platform=$(docker image inspect --format '{{.Os}}/{{.Architecture}}' "$image")
case "$platform" in linux/arm64) arch=arm64;; linux/amd64) arch=amd64;; *) echo "unsupported platform $platform" >&2; exit 1;; esac
printf '%s\n' "$platform" >"$evidence/platform.txt"
docker image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "$image" >"$evidence/digests.txt"
grep -Fxq "$digest" "$evidence/digests.txt" || { echo 'local pinned digest mismatch' >&2; exit 1; }
binary=$(cd "$(dirname "$3")" && pwd)/$(basename "$3")
shasum -a 256 "$binary" >"$evidence/binary-sha256.txt"
selected="^TestTaskArchiveAdmissionNative$"
expected=0
case_name=admission
name="$project-$case_name"
docker create --pull=never --name "$name" --label "sandbox.test.project=$project" --label "sandbox.test.fixture=$fixture" \
  --network none --user 0:0 --read-only --memory 128m --pids-limit 128 --cpus 0.5 \
  --cap-drop ALL --cap-add KILL --cap-add SETUID --cap-add SETGID --cap-add SETPCAP --security-opt no-new-privileges:true \
  --mount "type=bind,src=$binary,dst=/workspace-archive.test,readonly" \
  --tmpfs /journal:rw,nosuid,nodev,noexec,size=32m,mode=0700 \
  --env SANDBOX_LAUNCHER_TEST_MODE=positive --env SANDBOX_TASK_ARCHIVE_NATIVE=1 --env TMPDIR=/journal --env GOMAXPROCS=2 --entrypoint /workspace-archive.test "$image" \
  -test.v -test.count=1 -test.timeout=45s -test.run "$selected" >"$evidence/container.txt"
containers+=("$name")
docker inspect --format '{"id":{{json .Id}},"image":{{json .Image}},"user":{{json .Config.User}},"labels":{{json .Config.Labels}},"host":{{json .HostConfig}},"mounts":{{json .Mounts}}}' "$name" >"$evidence/created.json"
docker start -a "$name" >"$evidence/native.log" 2>&1 &
attach=$!
deadline=$((SECONDS+60))
while kill -0 "$attach" 2>/dev/null; do
  if (( SECONDS >= deadline )); then
    docker kill "$name" >>"$evidence/native.log" 2>&1 || true
    kill -TERM "$attach" 2>/dev/null || true
    grace=$((SECONDS+2))
    while kill -0 "$attach" 2>/dev/null && (( SECONDS < grace )); do sleep 0.1; done
    forced=false
    if kill -0 "$attach" 2>/dev/null; then
      forced=true
      kill -KILL "$attach" 2>/dev/null || true
      grace=$((SECONDS+2))
      while kill -0 "$attach" 2>/dev/null && (( SECONDS < grace )); do sleep 0.1; done
    fi
    if kill -0 "$attach" 2>/dev/null; then
      printf 'attachPID=%s forced=%s joined=false\n' "$attach" "$forced" | tee "$evidence/termination.txt" >&2
      exit 1
    fi
    wait "$attach" || true
    printf 'attachPID=%s forced=%s joined=true\n' "$attach" "$forced" | tee "$evidence/termination.txt" >&2
    echo 'attach timeout' >&2
    exit 1
  fi
  sleep 0.2
done
attach_exit=0
wait "$attach" || attach_exit=$?
docker inspect --format '{"id":{{json .Id}},"state":{{json .State}}}' "$name" >"$evidence/exited.json"
terminal=$(docker inspect --format '{{.State.Status}} {{.State.Running}} {{.State.Restarting}} {{.State.OOMKilled}} {{.State.ExitCode}}' "$name")
printf 'attachExit=%s terminal=%s\n' "$attach_exit" "$terminal" | tee "$evidence/result.txt"
# Never wait on a still-running container after an attach failure. The owned
# cleanup kills/removes it; only an observed terminal process can be waited.
if [[ "$terminal" != 'exited false false '* ]]; then
  echo 'waitCommandExit=not-run processWait=not-terminal' >>"$evidence/result.txt"
  exit 1
fi
waited=$(docker wait "$name")
printf 'waitCommandExit=0 processWait=%s\n' "$waited" >>"$evidence/result.txt"
if [[ "$attach_exit" != "$expected" || "$terminal" != "exited false false false $expected" ]]; then
  exit 1
fi
[[ "$waited" == "$expected" ]] || exit 1
grep -Fxq PASS "$evidence/native.log"
counts=$(awk '/^[[:space:]]*--- PASS:/ {p++} /^[[:space:]]*--- FAIL:/ {f++} /^[[:space:]]*--- SKIP:/ {s++} END {printf "PASS=%d FAIL=%d SKIP=%d",p,f,s}' "$evidence/native.log")
echo "$counts" | tee "$evidence/counts.txt"
[[ "$counts" == *' FAIL=0 SKIP=0' ]] || exit 1
