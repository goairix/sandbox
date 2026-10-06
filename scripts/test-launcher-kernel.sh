#!/usr/bin/env bash
# Controller-owned native acceptance. Uses only the already-local pinned image.
set -euo pipefail
image='gcr.io/etcd-development/etcd:v3.6.15@sha256:5ed4e32061aa061f970d6500629213e8579ef17571dcf9789074966acb9afad1'
digest='gcr.io/etcd-development/etcd@sha256:5ed4e32061aa061f970d6500629213e8579ef17571dcf9789074966acb9afad1'
project="sandbox-launcher-kernel-test-$$-$(date +%s)"
fixture='launcher-kernel-boundary'
repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
if [[ $# != 1 || -z "$1" ]]; then
  echo 'usage: test-launcher-kernel.sh EVIDENCE_DIRECTORY' >&2
  exit 2
fi
evidence=$1
mkdir -p "$evidence"
evidence=$(cd "$evidence" && pwd)
containers=()
volumes=()
cleanup() {
  local original=$? failed=0 name labels remaining kind
  trap - EXIT INT TERM
  for name in ${containers[@]+"${containers[@]}"}; do
    if labels=$(docker inspect --format '{{index .Config.Labels "sandbox.test.project"}} {{index .Config.Labels "sandbox.test.fixture"}}' "$name" 2>/dev/null); then
      if [[ "$labels" == "$project $fixture" ]]; then
        if docker rm -f "$name" >>"$evidence/cleanup.log" 2>&1; then
          echo "container $name rm exit=0" >>"$evidence/cleanup.log"
        else failed=1; echo "container $name rm failed" >>"$evidence/cleanup.log"; fi
      else failed=1; echo "refused unowned container $name" >>"$evidence/cleanup.log"; fi
    else failed=1; echo "container inspect failed $name" >>"$evidence/cleanup.log"; fi
  done
  for name in ${volumes[@]+"${volumes[@]}"}; do
    if labels=$(docker volume inspect --format '{{index .Labels "sandbox.test.project"}} {{index .Labels "sandbox.test.fixture"}}' "$name" 2>/dev/null); then
      if [[ "$labels" == "$project $fixture" ]]; then
        if docker volume rm "$name" >>"$evidence/cleanup.log" 2>&1; then
          echo "volume $name rm exit=0" >>"$evidence/cleanup.log"
        else failed=1; echo "volume $name rm failed" >>"$evidence/cleanup.log"; fi
      else failed=1; echo "refused unowned volume $name" >>"$evidence/cleanup.log"; fi
    else failed=1; echo "volume inspect failed $name" >>"$evidence/cleanup.log"; fi
  done
  for kind in container volume network; do
    case "$kind" in
      container) if remaining=$(docker ps -aq --filter "label=sandbox.test.project=$project"); then :; else failed=1; remaining='inventory error'; fi;;
      volume) if remaining=$(docker volume ls -q --filter "label=sandbox.test.project=$project"); then :; else failed=1; remaining='inventory error'; fi;;
      network) if remaining=$(docker network ls -q --filter "label=sandbox.test.project=$project"); then :; else failed=1; remaining='inventory error'; fi;;
    esac
    printf '%s inventory result (empty means successful query): %s\n' "$kind" "$remaining" >>"$evidence/cleanup.log"
    [[ -z "$remaining" ]] || failed=1
  done
  if (( original != 0 || failed != 0 )); then
    echo "kernel fixture failed; evidence $evidence" >&2
    exit 1
  fi
  echo "kernel boundary verified: canonical native suite; owned inventories empty; evidence $evidence"
}
# Only wait after observing that this exact local attach client has exited.
# A client surviving TERM gets a bounded grace period, then KILL and reaping.
terminate_attach() {
  local pid=$1 grace forced=false attach_exit=0
  kill -TERM "$pid" 2>/dev/null || true
  grace=$((SECONDS+2))
  while kill -0 "$pid" 2>/dev/null && (( SECONDS < grace )); do sleep 0.1; done
  if kill -0 "$pid" 2>/dev/null; then
    forced=true
    kill -KILL "$pid" 2>/dev/null || true
    grace=$((SECONDS+2))
    while kill -0 "$pid" 2>/dev/null && (( SECONDS < grace )); do sleep 0.1; done
  fi
  if kill -0 "$pid" 2>/dev/null; then
    printf 'attachPID=%s forced=%s joined=false; client did not exit\n' "$pid" "$forced" | tee "$evidence/$mode-termination.txt" >&2
    return 1
  fi
  wait "$pid" || attach_exit=$?
  printf 'attachPID=%s attachExit=%s forced=%s joined=true\n' "$pid" "$attach_exit" "$forced" | tee "$evidence/$mode-termination.txt" >&2
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
# inspect is required even if Go could cross-compile for the host architecture.
docker image inspect "$image" >"$evidence/image.json"
platform=$(docker image inspect --format '{{.Os}}/{{.Architecture}}' "$image")
case "$platform" in linux/arm64) arch=arm64;; linux/amd64) arch=amd64;; *) echo "unsupported local image platform $platform" >&2; exit 1;; esac
printf '%s\n' "$platform" >"$evidence/platform.txt"
docker image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "$image" >"$evidence/digests.txt"
if ! grep -Fxq "$digest" "$evidence/digests.txt"; then echo 'pinned local RepoDigest mismatch' >&2; exit 1; fi
(
 cd "$repo"
 env CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go test -c -o "$evidence/launcher-kernel.test" ./internal/runtime/launcher
 git rev-parse HEAD >"$evidence/source-commit.txt"
 git diff -- internal/runtime/launcher scripts/test-launcher-kernel.sh >"$evidence/source.diff"
 shasum -a 256 internal/runtime/launcher/*.go scripts/test-launcher-kernel.sh >"$evidence/source-sha256.txt"
)
shasum -a 256 "$evidence/launcher-kernel.test" >"$evidence/binary-sha256.txt"
for mode in positive reject-nnp reject-missing-cap reject-extra-cap reject-role reject-thread reject-securebits reject-setter reject-partial; do
 name="$project-$mode"
 volume="$name-tmp"
 volumes+=("$volume")
 docker volume create --label "sandbox.test.project=$project" --label "sandbox.test.fixture=$fixture" "$volume" >"$evidence/$mode-volume.txt"
 caps=(--cap-drop ALL --cap-add KILL --cap-add SETGID --cap-add SETUID)
 [[ "$mode" == reject-missing-cap ]] || caps+=(--cap-add SETPCAP)
 [[ "$mode" != reject-extra-cap ]] || caps+=(--cap-add CHOWN)
 security=()
 [[ "$mode" == reject-nnp ]] || security+=(--security-opt no-new-privileges:true)
 args=()
 [[ "$mode" != positive ]] || args=(-test.v -test.count=1 -test.timeout=45s)
 containers+=("$name")
 docker create --pull=never --name "$name" --label "sandbox.test.project=$project" --label "sandbox.test.fixture=$fixture" \
   --network none --user 0:0 --read-only --memory 64m --pids-limit 128 --cpus 0.5 \
   ${caps[@]+"${caps[@]}"} ${security[@]+"${security[@]}"} \
   --mount "type=bind,src=$evidence/launcher-kernel.test,dst=/launcher-kernel.test,readonly" \
   --mount "type=volume,src=$volume,dst=/tmp" \
   --env "SANDBOX_LAUNCHER_TEST_MODE=$mode" --env GOMAXPROCS=2 \
   --entrypoint /launcher-kernel.test "$image" ${args[@]+"${args[@]}"} >"$evidence/$mode-container.txt"
 docker inspect "$name" >"$evidence/$mode-created.json"
 docker start -a "$name" >"$evidence/$mode.log" 2>&1 &
 attach=$!
 deadline=$((SECONDS+60))
 while kill -0 "$attach" 2>/dev/null; do
   if (( SECONDS >= deadline )); then
     echo "attach timeout for $mode" >&2
     kill_exit=0
     docker kill "$name" >>"$evidence/$mode.log" 2>&1 || kill_exit=$?
     printf 'container kill exit=%s\n' "$kill_exit" >>"$evidence/$mode.log"
     terminate_attach "$attach" || true
     exit 1
   fi
   sleep 0.2
 done
 attach_exit=0
 wait "$attach" || attach_exit=$?
 docker inspect "$name" >"$evidence/$mode-exited.json"
 terminal=$(docker inspect --format '{{.State.Status}} {{.State.Running}} {{.State.OOMKilled}} {{.State.ExitCode}}' "$name")
 expected=0
 [[ "$mode" != reject-securebits ]] || expected=2
 # An attachment can fail while its container keeps running. Do not enter
 # docker wait unless actual terminal state and attachment outcome agree.
 if [[ "$attach_exit" != "$expected" || "$terminal" != "exited false false $expected" ]]; then
   printf 'mode=%s attachExit=%s terminal=%s waitCommandExit=not-run processWait=not-run\n' "$mode" "$attach_exit" "$terminal" | tee "$evidence/$mode-result.txt" >&2
   exit 1
 fi
 wait_exit=0
 waited=$(docker wait "$name") || wait_exit=$?
 printf 'mode=%s attachExit=%s terminal=%s waitCommandExit=%s processWait=%s\n' "$mode" "$attach_exit" "$terminal" "$wait_exit" "$waited" | tee "$evidence/$mode-result.txt"
 [[ "$attach_exit" == "$expected" && "$terminal" == "exited false false $expected" && "$wait_exit" == 0 && "$waited" == "$expected" ]] || exit 1
 if [[ "$mode" == reject-securebits ]]; then
   grep -Fq 'AllThreadsSyscall6 results differ between threads' "$evidence/$mode.log"
   if grep -Fq 'kernel boundary verified' "$evidence/$mode.log"; then exit 1; fi
 elif [[ "$mode" == positive ]]; then
   grep -Fq 'kernel boundary verified: user isolation' "$evidence/$mode.log"
   grep -Fq 'kernel boundary verified: worker cleanup' "$evidence/$mode.log"
   grep -Fq 'monitor confinement verified: inherited policy and user exec' "$evidence/$mode.log"
   grep -Fq 'monitor confinement verified: confinement-setter rejected without boundary' "$evidence/$mode.log"
   grep -Fxq PASS "$evidence/$mode.log"
   counts=$(awk '/^[[:space:]]*--- PASS:/ {p++} /^[[:space:]]*--- FAIL:/ {f++} /^[[:space:]]*--- SKIP:/ {s++} END {printf "PASS=%d FAIL=%d SKIP=%d",p,f,s}' "$evidence/$mode.log")
   echo "$counts" | tee "$evidence/positive-counts.txt"
   [[ "$counts" == *' FAIL=0 SKIP=0' ]] || exit 1
 else
   grep -Fq "kernel boundary verified: $mode rejected" "$evidence/$mode.log"
 fi
done
