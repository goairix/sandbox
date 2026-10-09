#!/usr/bin/env bash
# Explicit owned Linux CGO0 proof, using only an already-local pinned image.
set -euo pipefail
image='gcr.io/etcd-development/etcd:v3.6.15@sha256:5ed4e32061aa061f970d6500629213e8579ef17571dcf9789074966acb9afad1'
digest='gcr.io/etcd-development/etcd@sha256:5ed4e32061aa061f970d6500629213e8579ef17571dcf9789074966acb9afad1'
project="sandbox-workspace-sync-local-$$-$(date +%s)"
fixture='workspace-sync-local'
repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
go_tool='/Users/dysodeng/go/go1.25.6/bin/go'
if [[ $# != 1 || -z "$1" ]]; then echo 'usage: test-workspace-sync-local.sh EVIDENCE_DIRECTORY' >&2; exit 2; fi
mkdir -p "$1"
evidence=$(cd "$1" && pwd)
name="$project-native"
container=''
attach=''
join_failed=0

join_attach() {
  local pid=$1 grace status=0
  kill -TERM "$pid" 2>/dev/null || true
  grace=$((SECONDS+2))
  while kill -0 "$pid" 2>/dev/null && (( SECONDS < grace )); do sleep 0.1; done
  if kill -0 "$pid" 2>/dev/null; then kill -KILL "$pid" 2>/dev/null || true; fi
  grace=$((SECONDS+2))
  while kill -0 "$pid" 2>/dev/null && (( SECONDS < grace )); do sleep 0.1; done
  if kill -0 "$pid" 2>/dev/null; then
    echo "attach PID=$pid project=$project fixture=$fixture joined=false retained=true" >>"$evidence/cleanup.log"
    return 1
  fi
  wait "$pid" || status=$?
  echo "attach PID=$pid exit=$status joined=true" >>"$evidence/cleanup.log"
}

cleanup() {
  local original=$? failed=$join_failed labels terminal waited wait_exit kind remaining
  trap - EXIT INT TERM
  if [[ -n "$container" ]]; then
    if labels=$(docker inspect --format '{{index .Config.Labels "sandbox.test.project"}} {{index .Config.Labels "sandbox.test.fixture"}}' "$container" 2>/dev/null); then
      if [[ "$labels" != "$project $fixture" ]]; then
        echo "refused unowned $container" >>"$evidence/cleanup.log"
        failed=1
      else
        terminal=$(docker inspect --format '{{.State.Status}} {{.State.Running}}' "$container") || { failed=1; terminal='unknown'; }
        if [[ "$terminal" == *' true' ]]; then
          docker kill "$container" >>"$evidence/cleanup.log" 2>&1 || failed=1
          terminal=$(docker inspect --format '{{.State.Status}} {{.State.Running}}' "$container") || { failed=1; terminal='unknown'; }
        fi
        if [[ "$terminal" == 'exited false' ]]; then
          wait_exit=0
          waited=$(docker wait "$container") || wait_exit=$?
          echo "container=$container terminal=$terminal waitCommandExit=$wait_exit processWait=$waited" >>"$evidence/cleanup.log"
          if [[ "$wait_exit" == 0 && "$waited" =~ ^[0-9]+$ ]]; then
            for i in 1 2 3; do
              awk -v member="local$i" '$0 == "BEGIN RAW ETCD CHILD " member {copy=1; next} $0 == "END RAW ETCD CHILD " member {copy=0; next} copy {print}' "$evidence/native.log" >"$evidence/etcd-child-$i.log"
              [[ -s "$evidence/etcd-child-$i.log" ]] || failed=1
            done
            docker rm "$container" >>"$evidence/cleanup.log" 2>&1 || failed=1
          else failed=1; fi
        elif [[ "$terminal" == 'created false' ]]; then
          docker rm "$container" >>"$evidence/cleanup.log" 2>&1 || failed=1
        else
          echo "container=$container terminal=$terminal retained=true" >>"$evidence/cleanup.log"
          failed=1
        fi
      fi
    else failed=1; fi
  fi
  if [[ -n "$attach" ]]; then
    if join_attach "$attach"; then attach=''; else failed=1; join_failed=1; fi
  fi
  for kind in container volume network; do
    case "$kind" in
      container) remaining=$(docker ps -aq --filter "label=sandbox.test.project=$project" --filter "label=sandbox.test.fixture=$fixture") || { failed=1; remaining='query failed'; };;
      volume) remaining=$(docker volume ls -q --filter "label=sandbox.test.project=$project" --filter "label=sandbox.test.fixture=$fixture") || { failed=1; remaining='query failed'; };;
      network) remaining=$(docker network ls -q --filter "label=sandbox.test.project=$project" --filter "label=sandbox.test.fixture=$fixture") || { failed=1; remaining='query failed'; };;
    esac
    echo "$kind exact owned inventory: $remaining" >>"$evidence/cleanup.log"
    [[ -z "$remaining" ]] || failed=1
  done
  if (( original != 0 || failed != 0 )); then echo "local fixture failed; retainedAttachPID=${attach:-none}; evidence=$evidence" >&2; exit 1; fi
  echo "local native gate verified; three original servers joined; exact owned inventories empty; evidence=$evidence"
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
source_files=(
  internal/storage/state/etcd/task_workspace_sync_local_types.go
  internal/storage/state/etcd/task_workspace_sync_local_linux.go
  internal/storage/state/etcd/task_workspace_sync_local_unsupported.go
  internal/storage/state/etcd/task_workspace_sync_local_guard.go
  internal/storage/state/etcd/task_workspace_sync_local_test.go
  internal/storage/state/etcd/task_workspace_sync_local_native_linux_test.go
  internal/storage/state/etcd/task_workspace_sync_intent_types.go
  internal/storage/state/etcd/task_workspace_sync_intent_records.go
  internal/storage/state/etcd/task_workspace_sync_intent_admission.go
  internal/storage/state/etcd/task_workspace_sync_source_types.go
  internal/storage/state/etcd/task_close_delivery_fixture_test.go
  scripts/test-workspace-sync-local.sh
)
(
  cd "$repo"
  shasum -a 256 "${source_files[@]}" >"$evidence/source-sha256.txt"
  env CGO_ENABLED=0 GOOS=linux GOARCH="$arch" "$go_tool" test -c -o "$evidence/workspace-sync-local.test" ./internal/storage/state/etcd
  shasum -a 256 -c "$evidence/source-sha256.txt" >"$evidence/source-build-match.txt"
) >"$evidence/build.log" 2>&1
chmod 755 "$evidence/workspace-sync-local.test"
shasum -a 256 "$evidence/workspace-sync-local.test" >"$evidence/binary-sha256.txt"
"$go_tool" version -m "$evidence/workspace-sync-local.test" >"$evidence/binary-build-info.txt"
docker create --pull=never --name "$name" --label "sandbox.test.project=$project" --label "sandbox.test.fixture=$fixture" \
  --network none --user 65532:65532 --read-only --memory 512m --pids-limit 256 --cpus 2 \
  --cap-drop ALL --security-opt no-new-privileges:true --tmpfs /work:rw,nosuid,nodev,noexec,size=512m,mode=1777 \
  --mount "type=bind,src=$evidence/workspace-sync-local.test,dst=/workspace-sync-local.test,readonly" \
  --env SANDBOX_WORKSPACE_SYNC_LOCAL_NATIVE=1 --env GOMAXPROCS=2 \
  --entrypoint /workspace-sync-local.test "$image" \
  -test.run '^TestTaskWorkspaceSync(LocalNative|IntentNativeAtomicRegistration|IntentNativeUnknownCommitted|IntentNativeSameTxnCurrentFences|IntentNativePaired)' \
  -test.v -test.count=1 -test.timeout=240s >"$evidence/container-id.txt"
container="$name"
docker inspect --format '{{.Id}} user={{.Config.User}} capAdd={{.HostConfig.CapAdd}} capDrop={{.HostConfig.CapDrop}} security={{.HostConfig.SecurityOpt}} readonly={{.HostConfig.ReadonlyRootfs}} network={{.HostConfig.NetworkMode}} memory={{.HostConfig.Memory}} pids={{.HostConfig.PidsLimit}} cpus={{.HostConfig.NanoCpus}} tmpfs={{.HostConfig.Tmpfs}}' "$container" >"$evidence/container-config.txt"
docker start -a "$container" >"$evidence/native.log" 2>&1 &
attach=$!
deadline=$((SECONDS+270))
while kill -0 "$attach" 2>/dev/null; do
  if (( SECONDS >= deadline )); then
    echo 'native attach timeout' >&2
    docker kill "$container" >>"$evidence/native.log" 2>&1 || true
    if join_attach "$attach"; then attach=''; else join_failed=1; fi
    exit 1
  fi
  sleep 0.2
done
attach_exit=0
wait "$attach" || attach_exit=$?
attach=''
terminal=$(docker inspect --format '{{.State.Status}} {{.State.Running}} {{.State.OOMKilled}} {{.State.ExitCode}}' "$container")
printf 'attachExit=%s joined=true terminal=%s\n' "$attach_exit" "$terminal" >"$evidence/result.txt"
[[ "$attach_exit" == 0 && "$terminal" == 'exited false false 0' ]] || exit 1
grep -Fxq PASS "$evidence/native.log"
[[ $(grep -c 'native etcd child .*joined=true' "$evidence/native.log") == 3 ]] || exit 1
counts=$(awk '/^[[:space:]]*--- PASS:/ {p++} /^[[:space:]]*--- FAIL:/ {f++} /^[[:space:]]*--- SKIP:/ {s++} END {printf "PASS=%d FAIL=%d SKIP=%d",p,f,s}' "$evidence/native.log")
echo "$counts" >"$evidence/counts.txt"
[[ "$counts" == *' FAIL=0 SKIP=0' ]] || exit 1
