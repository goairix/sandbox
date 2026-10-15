#!/usr/bin/env bash
# Explicit owned Linux CGO0 proof, using only an already-local pinned image.
set -euo pipefail
image='gcr.io/etcd-development/etcd:v3.6.15@sha256:5ed4e32061aa061f970d6500629213e8579ef17571dcf9789074966acb9afad1'
digest='gcr.io/etcd-development/etcd@sha256:5ed4e32061aa061f970d6500629213e8579ef17571dcf9789074966acb9afad1'
project="sandbox-workspace-sync-local-$$-$(date +%s)"
fixture='workspace-sync-local'
repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
go_tool='/Users/dysodeng/go/go1.25.6/bin/go'
if [[ $# -lt 1 || $# -gt 2 || -z "$1" ]]; then echo 'usage: test-workspace-sync-local.sh EVIDENCE_DIRECTORY [fixed local/recycling proof mode|reap-retained]' >&2; exit 2; fi
mode=${2:-normal}
test_run='^TestTaskWorkspaceSync(LocalNative|IntentNativeAtomicRegistration|IntentNativeUnknownCommitted|IntentNativeSameTxnCurrentFences|IntentNativePaired)'
case "$mode" in
  normal) ;;
	recovery-execution-cold) test_run='^TestTaskWorkspaceSyncRecoveryNativeCold$';;
	recovery-execution-authority) test_run='^TestTaskWorkspaceSyncRecoveryNativeAuthority$';;
	recovery-execution-continuation) test_run='^TestTaskWorkspaceSyncRecoveryNativeContinuation$';;
	recovery-execution-65) test_run='^TestTaskWorkspaceSyncRecoveryNativeSequential65$';;
	recovery-execution-truncate) test_run='^TestTaskWorkspaceSyncRecoveryNativeInterruptedIO$/^truncate$';;
	recovery-execution-write) test_run='^TestTaskWorkspaceSyncRecoveryNativeInterruptedIO$/^write$';;
	recovery-execution-fsync) test_run='^TestTaskWorkspaceSyncRecoveryNativeInterruptedIO$/^fsync$';;
	recovery-execution-close) test_run='^TestTaskWorkspaceSyncRecoveryNativeInterruptedIO$/^close$';;
	recovery-execution-terminal-unknown) test_run='^TestTaskWorkspaceSyncRecoveryNativeUnknown$/^terminal$';;
	recovery-execution-gc-unknown) test_run='^TestTaskWorkspaceSyncRecoveryNativeUnknown$/^gc$';;
	recovery-execution-join-write) test_run='^TestTaskWorkspaceSyncRecoveryNativeCloseJoin$/^write$';;
	recovery-execution-join-fsync) test_run='^TestTaskWorkspaceSyncRecoveryNativeCloseJoin$/^fsync$';;
	recovery-execution-join-close) test_run='^TestTaskWorkspaceSyncRecoveryNativeCloseJoin$/^close$';;
	recovery-execution-join-terminal) test_run='^TestTaskWorkspaceSyncRecoveryNativeCloseJoin$/^terminal$';;
	recovery-execution-join-gc) test_run='^TestTaskWorkspaceSyncRecoveryNativeCloseJoin$/^gc$';;
	recovery-execution-join-query) test_run='^TestTaskWorkspaceSyncRecoveryNativeCloseJoin$/^query$';;
	recovery-execution-r1-monitor) test_run='^TestTaskWorkspaceSyncRecoveryNativeReviewMonitor$';;
	recovery-execution-r1-terminal) test_run='^TestTaskWorkspaceSyncRecoveryNativeReviewTerminalPostguard$';;
	recovery-execution-history) test_run='^TestTaskWorkspaceSyncRecoveryNativeHistoricalQuery$';;
	recovery-execution-query-data) test_run='^TestTaskWorkspaceSyncRecoveryNativeQueryRejects$/^data$';;
	recovery-execution-query-double) test_run='^TestTaskWorkspaceSyncRecoveryNativeQueryRejects$/^double$';;
	recovery-execution-query-abrupt) test_run='^TestTaskWorkspaceSyncRecoveryNativeQueryRejects$/^abrupt$';;
	recovery-execution-query-close) test_run='^TestTaskWorkspaceSyncRecoveryNativeQueryRejects$/^close$';;
	recovery-execution-query-remaining) test_run='^TestTaskWorkspaceSyncRecoveryNativeQueryRejects$/^(data|double|abrupt)$';;
	recovery-execution-tamper-boot) test_run='^TestTaskWorkspaceSyncRecoveryNativeMetadataTamper$/^boot$';;
	recovery-execution-tamper-namespace) test_run='^TestTaskWorkspaceSyncRecoveryNativeMetadataTamper$/^namespace$';;
	recovery-execution-fence-claim) test_run='^TestTaskWorkspaceSyncRecoveryNativeFences$/^claim$';;
	recovery-execution-fence-archive) test_run='^TestTaskWorkspaceSyncRecoveryNativeFences$/^archive$';;
	recovery-execution-fence-history) test_run='^TestTaskWorkspaceSyncRecoveryNativeFences$/^history$';;
	recovery-execution-fence-target) test_run='^TestTaskWorkspaceSyncRecoveryNativeFences$/^target$';;
	archive-recovery-red) test_run='^TestTaskArchiveRecoveryOriginalNative$';;
	archive-recovery) test_run='^TestTaskArchiveRecovery';;
	archive-recovery-session) test_run='^TestTaskArchiveRecoverySessionNative$';;
	archive-recovery-quota) test_run='^TestTaskArchiveRecoveryColdContentQuota$';;
	archive-recovery-filesystem) test_run='^TestTaskArchiveRecoveryStoreFilesystemQuarantine$';;
	archive-recovery-crash-artifact) test_run='^TestTaskArchiveRecoveryCrashNative$/^(artifact-created|artifact-root-fsync|tar-fsync)$';;
	archive-recovery-crash-received) test_run='^TestTaskArchiveRecoveryCrashNative$/^received-';;
	archive-recovery-crash-bound) test_run='^TestTaskArchiveRecoveryCrashNative$/^bound-';;
	archive-recovery-failure) test_run='^TestTaskArchiveRecoveryFailureNative$';;
	archive-recovery-close) test_run='^TestTaskArchiveRecoveryCloseNative$';;
	archive-recovery-initialization-close) test_run='^TestTaskArchiveRecoveryInitializationCloseNative$';;
	archive-recovery-initialization-duplicate) test_run='^TestTaskArchiveRecoveryInitializationDuplicateNative$';;
	archive-recovery-initialization-r2) test_run='^TestTaskArchiveRecoveryInitialization(Close|Duplicate)Native$';;
  postguard-proof) test_run='^TestTaskWorkspaceSyncLocalNativePostguardClose$';;
  local-gate-proof) test_run='^TestTaskWorkspaceSyncLocalNative';;
  local-put-proof) test_run='^TestTaskWorkspaceSyncLocalPutNative';;
  local-put-core) test_run='^TestTaskWorkspaceSyncLocalPutNative$/^nested-and-successive-original-files$';;
  local-put-close) test_run='^TestTaskWorkspaceSyncLocalPutNative$/^io-pending-';;
  local-put-ownership) test_run='^TestTaskWorkspaceSyncLocalPutNative$/^ownership$';;
  local-put-mode) test_run='^TestTaskWorkspaceSyncLocalPutNative$/^mode-zero-preserved$';;
  local-put-error) test_run='^TestTaskWorkspaceSyncLocalPutNative$/^real-write-error-retains-partial-charge$';;
  recycling-admission) test_run='^TestTaskWorkspaceSyncRecyclingNativeAdmission$';;
  recycling-settlement) test_run='^TestTaskWorkspaceSyncRecyclingNativeSettlement$';;
  recycling-65) test_run='^TestTaskWorkspaceSyncRecyclingNativeSequential65$';;
  checkpoint-core) test_run='^TestTaskWorkspaceSyncRecyclingNative(Settlement|Sequential65)$';;
  checkpoint-faults) test_run='^TestTaskWorkspaceSyncRecyclingNativeCheckpoint';;
  checkpoint-unknown) test_run='^TestTaskWorkspaceSyncRecyclingNative(CompletionUnknown|GCReusedUnknown)$/^gc-|^TestTaskWorkspaceSyncRecyclingNativeGCReusedUnknown$';;
  checkpoint-guards) test_run='^TestTaskWorkspaceSyncRecyclingNativeCompletionFences$/^gc-';;
  checkpoint-close) test_run='^TestTaskWorkspaceSyncRecyclingNativeCompletionClose$/^gc-';;
  checkpoint-protocol) test_run='^TestTaskWorkspaceSyncRecyclingNative(Checkpoint|GCReusedUnknown|CompletionUnknown|CompletionFences|CompletionClose)$/^(gc-|checkpoint-|fault-|unknown-|ack-|legacy-)';;
  checkpoint-legacy) test_run='^TestTaskWorkspaceSyncRecyclingNativeCheckpoint$/^legacy-';;
  checkpoint-close-reads) test_run='^TestTaskWorkspaceSyncRecyclingNativeCompletionClose$/^(checkpoint-rpc-|gc-observe-rpc-)';;
  checkpoint-close-mutation) test_run='^TestTaskWorkspaceSyncRecyclingNativeCompletionClose$/^gc-(rpc|postguard)-';;
  recycling-settlement-authority) test_run='^TestTaskWorkspaceSyncRecyclingNativeSettlementAuthority$';;
  recycling-cursor) test_run='^TestTaskWorkspaceSyncRecyclingNativeCursorGate$';;
  recycling-terminal-unknown) test_run='^TestTaskWorkspaceSyncRecyclingNativeCompletionUnknown$/^terminal-';;
  recycling-gc-unknown) test_run='^TestTaskWorkspaceSyncRecyclingNativeCompletionUnknown$/^gc-';;
  recycling-terminal-fence-native) test_run='^TestTaskWorkspaceSyncRecyclingNativeCompletionFences$/^terminal-(slot-generation|intent-immutable|restore)$';;
  recycling-terminal-fence-local) test_run='^TestTaskWorkspaceSyncRecyclingNativeCompletionFences$/^terminal-(claim-lost|root|deadline)$';;
  recycling-gc-fence-native) test_run='^TestTaskWorkspaceSyncRecyclingNativeCompletionFences$/^gc-(slot-generation|intent-immutable|restore)$';;
  recycling-gc-fence-local) test_run='^TestTaskWorkspaceSyncRecyclingNativeCompletionFences$/^gc-(claim-lost|root|deadline)$';;
  recycling-terminal-close-rpc) test_run='^TestTaskWorkspaceSyncRecyclingNativeCompletionClose$/^terminal-rpc-';;
  recycling-terminal-close-postguard) test_run='^TestTaskWorkspaceSyncRecyclingNativeCompletionClose$/^terminal-postguard-';;
  recycling-gc-close-rpc) test_run='^TestTaskWorkspaceSyncRecyclingNativeCompletionClose$/^gc-rpc-';;
  recycling-gc-close-postguard) test_run='^TestTaskWorkspaceSyncRecyclingNativeCompletionClose$/^gc-postguard-';;
  recycling-gc-reused) test_run='^TestTaskWorkspaceSyncRecyclingNativeGCReusedUnknown$';;
  recycling-proof) test_run='^TestTaskWorkspaceSyncRecyclingNative';;
  recycling-close-rpc) test_run='^TestTaskWorkspaceSyncRecyclingNativeCloseRPC$';;
  recycling-competition) test_run='^TestTaskWorkspaceSyncRecyclingNativeCompetition$';;
  recycling-gate-generation) test_run='^TestTaskWorkspaceSyncRecyclingNativeGateFaults$/^(slot-same-value-generation|slot-birth|intent-immutable)$';;
  recycling-gate-authority) test_run='^TestTaskWorkspaceSyncRecyclingNativeGateFaults$/^(restore|whole-prefix|claim-lost)$';;
  recycling-gate-local) test_run='^TestTaskWorkspaceSyncRecyclingNativeGateFaults$/^(root-after-ack|caller-canceled)$';;
  recycling-postguard) test_run='^TestTaskWorkspaceSyncRecyclingNativePostguardClose$';;
  recycling-unknown-absent) test_run='^TestTaskWorkspaceSyncRecyclingNativeUnknownAbsent$';;
  recycling-base) test_run='^TestTaskWorkspaceSyncRecyclingNative(Admission|CloseRPC|CandidateFaults)$';;
  recycling-outcomes) test_run='^TestTaskWorkspaceSyncRecyclingNative(Unknown|GenerationABA)$';;
  recycling-legacy) test_run='^TestTaskWorkspaceSync(IntentNativeAtomicRegistration|IntentNativeUnknownCommitted|LocalPutNative)$/^nested-and-successive-original-files$';;
  pending-proof) test_run='^TestTaskWorkspaceSyncLocalNative$/^readonly-attribution-original-gate$';;
  reap-retained) ;;
  attach-owner) ;;
  *) echo "unknown fixture mode $mode" >&2; exit 2;;
esac
mkdir -p "$1"
evidence=$(cd "$1" && pwd)
name="$project-native"
container=''
attach=''
join_failed=0

# Internal original owner runs in its own session, independently of the
# bounded controller. Only this process publishes its actual attach Wait.
if [[ "$mode" == attach-owner ]]; then
  project=$(awk -F= '$1 == "project" {print $2}' "$evidence/attach-input.txt")
  retained_fixture=$(awk -F= '$1 == "fixture" {print $2}' "$evidence/attach-input.txt")
  container=$(awk -F= '$1 == "container" {print $2}' "$evidence/attach-input.txt")
  foreign_fixture=$(awk -F= '$1 == "foreignFixture" {print $2}' "$evidence/attach-input.txt")
  [[ "$project" =~ ^sandbox-workspace-sync-local-[0-9]+-[0-9]+$ && "$retained_fixture" == "$fixture" && "$container" == "$project-native" ]] || exit 1
  labels=$(docker inspect --format '{{index .Config.Labels "sandbox.test.project"}} {{index .Config.Labels "sandbox.test.fixture"}}' "$container")
  [[ "$labels" == "$project $fixture" ]] || exit 1
  owner_session=$(python3 -c 'import os; print(os.getsid(os.getppid()))')
  printf 'project=%s fixture=%s container=%s ownerPID=%s session=%s\n' "$project" "$fixture" "$container" "$$" "$owner_session" >"$evidence/attach-owner.txt"
  docker start -a "$container" >"$evidence/native.log" 2>&1 &
  actual_attach=$!
  printf 'project=%s fixture=%s container=%s ownerPID=%s session=%s attachPID=%s\n' "$project" "$fixture" "$container" "$$" "$owner_session" "$actual_attach" >"$evidence/attach-handle.txt"
  helper_exit=0
  if [[ "$foreign_fixture" == 1 ]]; then
    helper_deadline=$((SECONDS+10))
    until [[ $(docker inspect --format '{{.State.Running}}' "$container") == true ]]; do
      (( SECONDS < helper_deadline )) || { helper_exit=1; break; }
      sleep 0.1
    done
    if [[ "$helper_exit" == 0 ]]; then
      docker exec --user 0:0 --env SANDBOX_WORKSPACE_SYNC_LOCAL_FOREIGN_INITIALIZER=1 "$container" /workspace-sync-local.test >"$evidence/foreign-helper.log" 2>&1 &
      actual_helper=$!
      printf 'project=%s fixture=%s container=%s ownerPID=%s session=%s helperPID=%s distinctUID0Fixture=true\n' "$project" "$fixture" "$container" "$$" "$owner_session" "$actual_helper" >"$evidence/foreign-helper-handle.txt"
      wait "$actual_helper" || helper_exit=$?
      printf '%s\n' "$helper_exit" >"$evidence/foreign-helper-exit.txt"
      printf 'project=%s fixture=%s container=%s ownerPID=%s session=%s helperPID=%s actualDockerExecWait=true joined=true exit=%s distinctUID0Fixture=true\n' "$project" "$fixture" "$container" "$$" "$owner_session" "$actual_helper" "$helper_exit" >"$evidence/foreign-helper-joined.txt"
    fi
  fi
  actual_status=0
  wait "$actual_attach" || actual_status=$?
  printf '%s\n' "$actual_status" >"$evidence/attach-exit.txt"
  printf 'project=%s fixture=%s container=%s ownerPID=%s session=%s attachPID=%s joined=true exit=%s\n' "$project" "$fixture" "$container" "$$" "$owner_session" "$actual_attach" "$actual_status" >"$evidence/attach-joined.tmp"
  mv "$evidence/attach-joined.tmp" "$evidence/attach-joined.txt"
  printf 'ownerPID=%s session=%s actualAttachWait=true exit=%s supervisorEnding=true\n' "$$" "$owner_session" "$actual_status" >"$evidence/attach-owner-ending.txt"
  if [[ "$helper_exit" != 0 ]]; then exit "$helper_exit"; fi
  exit "$actual_status"
fi

child_joins_complete() {
  local i count
  [[ -f "$evidence/native.log" ]] || return 1
  for i in 1 2 3; do
    count=$(grep -Ec "^native etcd child name=local$i pid=[0-9]+ joined=true exit=" "$evidence/native.log") || return 1
    [[ "$count" == 1 ]] || return 1
    [[ $(grep -Fxc "BEGIN RAW ETCD CHILD local$i" "$evidence/native.log") == 1 ]] || return 1
    [[ $(grep -Fxc "END RAW ETCD CHILD local$i" "$evidence/native.log") == 1 ]] || return 1
  done
}

attach_join_complete() {
  [[ -f "$evidence/attach-owner.txt" && -f "$evidence/attach-joined.txt" ]] && grep -Fq "project=$project fixture=$fixture container=$container ownerPID=" "$evidence/attach-owner.txt" && grep -Fq "$(<"$evidence/attach-owner.txt") attachPID=" "$evidence/attach-joined.txt" && grep -Fq ' joined=true ' "$evidence/attach-joined.txt"
}

helper_join_complete() {
  if [[ $(awk -F= '$1 == "foreignFixture" {print $2}' "$evidence/attach-input.txt") != 1 ]]; then return 0; fi
  [[ -f "$evidence/foreign-helper-handle.txt" && -f "$evidence/foreign-helper-joined.txt" ]] && grep -Fq "$(sed 's/ distinctUID0Fixture=true$//' "$evidence/foreign-helper-handle.txt") actualDockerExecWait=true joined=true exit=" "$evidence/foreign-helper-joined.txt"
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
        if [[ "$terminal" == 'exited false' ]] && child_joins_complete && attach_join_complete && helper_join_complete; then
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
          echo "container=$container terminal=$terminal childJoins=incomplete-or-attach-pending attachOwnerPID=${attach:-recorded} retained=true" >>"$evidence/cleanup.log"
          printf 'project=%s\nfixture=%s\ncontainer=%s\nattachOwnerPID=%s\nevidence=%s\n' "$project" "$fixture" "$container" "${attach:-recorded}" "$evidence" >"$evidence/retained-handle.txt"
          failed=1
        fi
      fi
    else failed=1; fi
  fi
  if [[ -n "$attach" ]]; then
    if child_joins_complete && attach_join_complete && helper_join_complete; then
      echo "attach owner PID=$attach observed actual attach Wait marker; supervisor lifecycle separately recorded" >>"$evidence/cleanup.log"
      attach=''
    else
      echo "attach owner PID=$attach project=$project fixture=$fixture retained=true; original supervisor continues actual attach Wait" >>"$evidence/cleanup.log"
      failed=1
    fi
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
  if [[ "$mode" == reap-retained ]]; then
    echo "retained original ownership joined; exact owned inventories empty; evidence=$evidence"
  else
    echo "local native gate verified; three original servers joined; exact owned inventories empty; evidence=$evidence"
  fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

if [[ "$mode" == reap-retained ]]; then
  project=$(awk -F= '$1 == "project" {print $2}' "$evidence/retained-handle.txt")
  retained_fixture=$(awk -F= '$1 == "fixture" {print $2}' "$evidence/retained-handle.txt")
  container=$(awk -F= '$1 == "container" {print $2}' "$evidence/retained-handle.txt")
  [[ "$project" =~ ^sandbox-workspace-sync-local-[0-9]+-[0-9]+$ && "$retained_fixture" == "$fixture" && "$container" == "$project-native" ]] || exit 1
  child_joins_complete && attach_join_complete && helper_join_complete || exit 1
  # EXIT cleanup freshly checks exact labels, Docker terminal/wait, and all
  # observed original child/attach joins before removing this retained handle.
  exit 0
fi

platform=$(docker image inspect --format '{{.Os}}/{{.Architecture}}' "$image")
case "$platform" in linux/arm64) arch=arm64;; linux/amd64) arch=amd64;; *) echo "unsupported local platform $platform" >&2; exit 1;; esac
docker image inspect --format '{{.Id}}' "$image" >"$evidence/image-id.txt"
docker image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "$image" >"$evidence/digests.txt"
grep -Fxq "$digest" "$evidence/digests.txt" || { echo 'pinned RepoDigest mismatch' >&2; exit 1; }
printf 'project=%s fixture=%s platform=%s CGO_ENABLED=0\n' "$project" "$fixture" "$platform" >"$evidence/fixture.txt"
docker ps -aq --filter "label=sandbox.test.project=$project" --filter "label=sandbox.test.fixture=$fixture" >"$evidence/pre-containers.txt"
docker volume ls -q --filter "label=sandbox.test.project=$project" --filter "label=sandbox.test.fixture=$fixture" >"$evidence/pre-volumes.txt"
docker network ls -q --filter "label=sandbox.test.project=$project" --filter "label=sandbox.test.fixture=$fixture" >"$evidence/pre-networks.txt"
[[ ! -s "$evidence/pre-containers.txt" && ! -s "$evidence/pre-volumes.txt" && ! -s "$evidence/pre-networks.txt" ]] || { echo 'exact project pre-inventory is not empty' >&2; exit 1; }
source_files=(
  internal/storage/state/etcd/task_records.go
  internal/storage/state/etcd/task_claim_records.go
  internal/storage/state/etcd/task_checkpoint.go
  internal/storage/state/etcd/task_read.go
  internal/storage/state/etcd/task_workspace_sync_checkpoint_records.go
  internal/storage/state/etcd/task_workspace_sync_checkpoint.go
  internal/storage/state/etcd/task_workspace_sync_checkpoint_test.go
  internal/storage/state/etcd/task_workspace_sync_recycling_settlement.go
  internal/storage/state/etcd/task_workspace_sync_recycling_gc.go
  internal/storage/state/etcd/task_workspace_sync_recycling_settlement_test.go
  internal/storage/state/etcd/task_workspace_sync_source_cursor.go
  internal/storage/state/etcd/task_workspace_sync_recycling_admission.go
  internal/storage/state/etcd/task_workspace_sync_recycling_read.go
  internal/storage/state/etcd/task_workspace_sync_recycling_guard.go
  internal/storage/state/etcd/task_workspace_sync_recycling_test.go
  internal/storage/state/etcd/task_workspace_sync_recycling_native_linux_test.go
  internal/storage/state/etcd/task_workspace_sync_recycling_types.go
  internal/storage/state/etcd/task_workspace_sync_recycling_records.go
  internal/storage/state/etcd/task_workspace_sync_recycling_budget.go
  internal/storage/state/etcd/task_workspace_sync_local_types.go
  internal/storage/state/etcd/task_workspace_sync_local_put_linux.go
  internal/storage/state/etcd/task_workspace_sync_local_put_unsupported.go
  internal/storage/state/etcd/task_workspace_sync_source_body.go
  internal/storage/state/etcd/task_archive_delivery_native_test.go
  internal/storage/state/etcd/task_workspace_sync_local_linux.go
  internal/storage/state/etcd/task_workspace_sync_local_unsupported.go
  internal/storage/state/etcd/task_workspace_sync_local_guard.go
  internal/storage/state/etcd/task_workspace_sync_local_test.go
  internal/storage/state/etcd/task_workspace_sync_local_native_linux_test.go
  internal/storage/state/etcd/task_workspace_sync_intent_types.go
  internal/storage/state/etcd/task_workspace_sync_intent_records.go
  internal/storage/state/etcd/task_workspace_sync_intent_admission.go
  internal/storage/state/etcd/task_workspace_sync_intent_read.go
  internal/storage/state/etcd/task_workspace_sync_source_types.go
  internal/storage/state/etcd/task_close_delivery_fixture_test.go
  scripts/test-workspace-sync-local.sh
)
(
  cd "$repo"
  shasum -a 256 "${source_files[@]}" >"$evidence/source-sha256.txt"
  git rev-parse HEAD >"$evidence/working-head.txt"
  git branch --show-current >"$evidence/working-branch.txt"
  git status --short -- "${source_files[@]}" >"$evidence/working-source-status.txt"
  git diff -- "${source_files[@]}" >"$evidence/working-source.diff"
  env CGO_ENABLED=0 GOOS=linux GOARCH="$arch" "$go_tool" list -json ./internal/storage/state/etcd >"$evidence/compiler-package.json"
  python3 -c 'import json,sys; p=json.load(open(sys.argv[1])); print("\n".join(p["Dir"]+"/"+f for key in ["GoFiles","CgoFiles","TestGoFiles","XTestGoFiles"] for f in p.get(key,[])))' "$evidence/compiler-package.json" >"$evidence/compiler-selected-inputs.txt"
  xargs shasum -a 256 <"$evidence/compiler-selected-inputs.txt" >"$evidence/compiler-selected-sha256.txt"
  env CGO_ENABLED=0 GOOS=linux GOARCH="$arch" "$go_tool" test -c -o "$evidence/workspace-sync-local.test" ./internal/storage/state/etcd
  shasum -a 256 -c "$evidence/source-sha256.txt" >"$evidence/source-build-match.txt"
  shasum -a 256 -c "$evidence/compiler-selected-sha256.txt" >"$evidence/compiler-build-match.txt"
) >"$evidence/build.log" 2>&1
chmod 755 "$evidence/workspace-sync-local.test"
shasum -a 256 "$evidence/workspace-sync-local.test" >"$evidence/binary-sha256.txt"
"$go_tool" version -m "$evidence/workspace-sync-local.test" >"$evidence/binary-build-info.txt"
pending_proof=0
[[ "$mode" != pending-proof ]] || pending_proof=1
foreign_fixture=0
[[ "$mode" != local-put-proof ]] || foreign_fixture=1
[[ "$mode" != local-put-ownership ]] || foreign_fixture=1
docker create --pull=never --name "$name" --label "sandbox.test.project=$project" --label "sandbox.test.fixture=$fixture" \
  --network none --user 65532:65532 --read-only --memory 512m --pids-limit 256 --cpus 2 \
  --cap-drop ALL --security-opt no-new-privileges:true --tmpfs /work:rw,nosuid,nodev,noexec,size=512m,mode=1777 \
  --mount "type=bind,src=$evidence/workspace-sync-local.test,dst=/workspace-sync-local.test,readonly" \
  --env SANDBOX_WORKSPACE_SYNC_LOCAL_NATIVE=1 --env "SANDBOX_WORKSPACE_SYNC_LOCAL_PENDING_PROOF=$pending_proof" --env GOMAXPROCS=2 \
  --env "SANDBOX_WORKSPACE_SYNC_LOCAL_FOREIGN_FIXTURE=$foreign_fixture" \
  --entrypoint /workspace-sync-local.test "$image" \
  -test.run "$test_run" \
  -test.v -test.count=1 -test.timeout=240s >"$evidence/container-id.txt"
container="$name"
docker inspect --format '{{.Id}} user={{.Config.User}} capAdd={{.HostConfig.CapAdd}} capDrop={{.HostConfig.CapDrop}} security={{.HostConfig.SecurityOpt}} readonly={{.HostConfig.ReadonlyRootfs}} network={{.HostConfig.NetworkMode}} memory={{.HostConfig.Memory}} pids={{.HostConfig.PidsLimit}} cpus={{.HostConfig.NanoCpus}} tmpfs={{.HostConfig.Tmpfs}}' "$container" >"$evidence/container-config.txt"
# The launcher does not own attach; the dedicated session owner above does.
printf 'project=%s\nfixture=%s\ncontainer=%s\nforeignFixture=%s\n' "$project" "$fixture" "$container" "$foreign_fixture" >"$evidence/attach-input.txt"
attach=$(python3 -c 'import subprocess,sys; log=open(sys.argv[3],"ab",buffering=0); p=subprocess.Popen(["bash",sys.argv[1],sys.argv[2],"attach-owner"],start_new_session=True,stdin=subprocess.DEVNULL,stdout=log,stderr=subprocess.STDOUT); print(p.pid)' "$repo/scripts/test-workspace-sync-local.sh" "$evidence" "$evidence/attach-owner.log")
printf 'ownerPID=%s session=%s launch=start_new_session\n' "$attach" "$attach" >"$evidence/attach-launch.txt"
deadline=$((SECONDS+270))
[[ "$mode" != pending-proof ]] || deadline=$((SECONDS+18))
while ! attach_join_complete; do
  kill -0 "$attach" 2>/dev/null || { echo 'original attach owner exited without a Wait marker' >&2; exit 1; }
  if (( SECONDS >= deadline )); then
    echo 'native attach timeout' >&2
    echo "controller deadline; original attachOwnerPID=$attach container=$container project=$project fixture=$fixture retained=true" >>"$evidence/cleanup.log"
    exit 1
  fi
  sleep 0.2
done
attach_exit=$(<"$evidence/attach-exit.txt")
if [[ "$foreign_fixture" == 1 ]]; then
  helper_join_complete || exit 1
  [[ $(<"$evidence/foreign-helper-exit.txt") == 0 ]] || exit 1
fi
[[ "$attach_exit" =~ ^[0-9]+$ ]] || exit 1
terminal=$(docker inspect --format '{{.State.Status}} {{.State.Running}} {{.State.OOMKilled}} {{.State.ExitCode}}' "$container")
printf 'attachExit=%s joined=true terminal=%s\n' "$attach_exit" "$terminal" >"$evidence/result.txt"
[[ "$attach_exit" == 0 && "$terminal" == 'exited false false 0' ]] || exit 1
grep -Fxq PASS "$evidence/native.log"
child_joins_complete && attach_join_complete || exit 1
counts=$(awk '/^[[:space:]]*--- PASS:/ {p++} /^[[:space:]]*--- FAIL:/ {f++} /^[[:space:]]*--- SKIP:/ {s++} END {printf "PASS=%d FAIL=%d SKIP=%d",p,f,s}' "$evidence/native.log")
echo "$counts" >"$evidence/counts.txt"
[[ "$counts" == *' FAIL=0 SKIP=0' ]] || exit 1
