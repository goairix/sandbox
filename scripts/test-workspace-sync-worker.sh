#!/usr/bin/env bash
# Owned CGO0 Linux kernel proof; the pinned image must already be local.
set -euo pipefail
image='gcr.io/etcd-development/etcd:v3.6.15@sha256:5ed4e32061aa061f970d6500629213e8579ef17571dcf9789074966acb9afad1'
digest='gcr.io/etcd-development/etcd@sha256:5ed4e32061aa061f970d6500629213e8579ef17571dcf9789074966acb9afad1'
project="sandbox-workspace-sync-worker-$$-$(date +%s)"
fixture='workspace-sync-worker'
repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
go_tool='/Users/dysodeng/go/go1.25.6/bin/go'
if [[ $# != 1 || -z "$1" ]]; then
  echo 'usage: test-workspace-sync-worker.sh EVIDENCE_DIRECTORY' >&2
  exit 2
fi
mkdir -p "$1"
evidence=$(cd "$1" && pwd)
containers=()
attach=''
mode='initial'

join_attach() {
  local pid=$1 grace status=0
  kill -TERM "$pid" 2>/dev/null || true
  grace=$((SECONDS+2))
  while kill -0 "$pid" 2>/dev/null && (( SECONDS < grace )); do sleep 0.1; done
  if kill -0 "$pid" 2>/dev/null; then kill -KILL "$pid" 2>/dev/null || true; fi
  grace=$((SECONDS+2))
  while kill -0 "$pid" 2>/dev/null && (( SECONDS < grace )); do sleep 0.1; done
  if kill -0 "$pid" 2>/dev/null; then
    echo "attach PID=$pid joined=false" >>"$evidence/cleanup.log"
    return 1
  fi
  wait "$pid" || status=$?
  echo "attach PID=$pid exit=$status joined=true" >>"$evidence/cleanup.log"
}

cleanup() {
  local original=$? failed=0 name labels terminal waited wait_exit remaining kind
  trap - EXIT INT TERM
  for name in ${containers[@]+"${containers[@]}"}; do
    if labels=$(docker inspect --format '{{index .Config.Labels "sandbox.test.project"}} {{index .Config.Labels "sandbox.test.fixture"}}' "$name" 2>/dev/null); then
      if [[ "$labels" != "$project $fixture" ]]; then
        echo "refused unowned $name" >>"$evidence/cleanup.log"
        failed=1
        continue
      fi
      terminal=$(docker inspect --format '{{.State.Status}} {{.State.Running}}' "$name") || { failed=1; continue; }
      if [[ "$terminal" == *' true' ]]; then
        docker kill "$name" >>"$evidence/cleanup.log" 2>&1 || failed=1
        terminal=$(docker inspect --format '{{.State.Status}} {{.State.Running}}' "$name") || { failed=1; continue; }
      fi
      if [[ "$terminal" == 'exited false' ]]; then
        wait_exit=0
        waited=$(docker wait "$name") || wait_exit=$?
        echo "cleanup $name terminal=$terminal waitCommandExit=$wait_exit processWait=$waited" >>"$evidence/cleanup.log"
        [[ "$wait_exit" == 0 && "$waited" =~ ^[0-9]+$ ]] || failed=1
      elif [[ "$terminal" != 'created false' ]]; then failed=1; fi
      docker rm "$name" >>"$evidence/cleanup.log" 2>&1 || failed=1
    else failed=1; fi
  done
  if [[ -n "$attach" ]]; then join_attach "$attach" || failed=1; attach=''; fi
  for kind in container volume network; do
    case "$kind" in
      container) remaining=$(docker ps -aq --filter "label=sandbox.test.project=$project") || { failed=1; remaining='query failed'; };;
      volume) remaining=$(docker volume ls -q --filter "label=sandbox.test.project=$project") || { failed=1; remaining='query failed'; };;
      network) remaining=$(docker network ls -q --filter "label=sandbox.test.project=$project") || { failed=1; remaining='query failed'; };;
    esac
    echo "$kind owned inventory: $remaining" >>"$evidence/cleanup.log"
    [[ -z "$remaining" ]] || failed=1
  done
  if (( original != 0 || failed != 0 )); then
    echo "workspace worker fixture failed; evidence $evidence" >&2
    exit 1
  fi
  echo "workspace worker native verified; exact owned inventories empty; evidence $evidence"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

platform=$(docker image inspect --format '{{.Os}}/{{.Architecture}}' "$image")
case "$platform" in linux/arm64) arch=arm64;; linux/amd64) arch=amd64;; *) echo "unsupported local platform $platform" >&2; exit 1;; esac
docker image inspect --format '{{.Id}}' "$image" >"$evidence/image-id.txt"
docker image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "$image" >"$evidence/digests.txt"
grep -Fxq "$digest" "$evidence/digests.txt" || { echo 'pinned RepoDigest mismatch' >&2; exit 1; }
printf 'project=%s fixture=%s platform=%s CGO_ENABLED=0\n' "$project" "$fixture" "$platform" >"$evidence/fixture.txt"
(
  cd "$repo"
  env CGO_ENABLED=0 GOOS=linux GOARCH="$arch" "$go_tool" test -c -o "$evidence/workspace-sync-worker.test" ./internal/runtime/launcher
)
chmod 755 "$evidence/workspace-sync-worker.test"
shasum -a 256 "$evidence/workspace-sync-worker.test" >"$evidence/binary-sha256.txt"
for mode in workspace-worker-positive workspace-worker-reject-root workspace-worker-reject-gid workspace-worker-reject-nnp workspace-worker-reject-cap; do
  name="$project-$mode"
  user='65532:65532'
  [[ "$mode" != workspace-worker-reject-root ]] || user='0:65532'
  [[ "$mode" != workspace-worker-reject-gid ]] || user='65532:0'
  caps=(--cap-drop ALL)
  [[ "$mode" != workspace-worker-reject-cap ]] || caps+=(--cap-add CHOWN)
  security=()
  [[ "$mode" == workspace-worker-reject-nnp ]] || security+=(--security-opt no-new-privileges:true)
  docker create --pull=never --name "$name" --label "sandbox.test.project=$project" --label "sandbox.test.fixture=$fixture" \
    --network none --user "$user" --read-only --memory 64m --pids-limit 128 --cpus 0.5 \
    ${caps[@]+"${caps[@]}"} ${security[@]+"${security[@]}"} \
    --mount "type=bind,src=$evidence/workspace-sync-worker.test,dst=/workspace-sync-worker.test,readonly" \
    --env "SANDBOX_LAUNCHER_TEST_MODE=$mode" --env GOMAXPROCS=2 \
    --entrypoint /workspace-sync-worker.test "$image" -test.run '^TestWorkspaceSyncWorker' -test.v -test.count=1 -test.timeout=25s >"$evidence/$mode-container.txt"
  containers+=("$name")
  docker inspect --format '{{.Id}} user={{.Config.User}} capAdd={{.HostConfig.CapAdd}} capDrop={{.HostConfig.CapDrop}} security={{.HostConfig.SecurityOpt}} readonly={{.HostConfig.ReadonlyRootfs}} network={{.HostConfig.NetworkMode}} memory={{.HostConfig.Memory}} pids={{.HostConfig.PidsLimit}} cpus={{.HostConfig.NanoCpus}}' "$name" >"$evidence/$mode-config.txt"
  docker start -a "$name" >"$evidence/$mode.log" 2>&1 &
  attach=$!
  deadline=$((SECONDS+35))
  while kill -0 "$attach" 2>/dev/null; do
    if (( SECONDS >= deadline )); then
      echo "attach timeout for $mode" >&2
      docker kill "$name" >>"$evidence/$mode.log" 2>&1 || true
      join_attach "$attach" || true
      attach=''
      exit 1
    fi
    sleep 0.2
  done
  attach_exit=0
  wait "$attach" || attach_exit=$?
  attach=''
  terminal=$(docker inspect --format '{{.State.Status}} {{.State.Running}} {{.State.OOMKilled}} {{.State.ExitCode}}' "$name")
  if [[ "$attach_exit" != 0 || "$terminal" != 'exited false false 0' ]]; then
    printf 'mode=%s attachExit=%s joined=true terminal=%s wait=not-run\n' "$mode" "$attach_exit" "$terminal" | tee "$evidence/$mode-result.txt" >&2
    exit 1
  fi
  wait_exit=0
  waited=$(docker wait "$name") || wait_exit=$?
  printf 'mode=%s attachExit=%s joined=true terminal=%s waitCommandExit=%s processWait=%s\n' "$mode" "$attach_exit" "$terminal" "$wait_exit" "$waited" | tee "$evidence/$mode-result.txt"
  [[ "$wait_exit" == 0 && "$waited" == 0 ]] || exit 1
  grep -Fxq PASS "$evidence/$mode.log"
  if [[ "$mode" == workspace-worker-positive ]]; then
    grep -Fq 'workspace sync worker verified: original actual kernel and child identity' "$evidence/$mode.log"
  else
    grep -Fq "workspace sync worker verified: $mode rejected actual kernel; initialization consumed" "$evidence/$mode.log"
  fi
  counts=$(awk '/^[[:space:]]*--- PASS:/ {p++} /^[[:space:]]*--- FAIL:/ {f++} /^[[:space:]]*--- SKIP:/ {s++} END {printf "PASS=%d FAIL=%d SKIP=%d",p,f,s}' "$evidence/$mode.log")
  echo "$counts" >"$evidence/$mode-counts.txt"
  [[ "$counts" == *' FAIL=0 SKIP=0' ]] || exit 1
  if grep -Eq 'WARNING|WARN|--- SKIP:' "$evidence/$mode.log"; then exit 1; fi
done
