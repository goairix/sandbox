# Cleanup task authority implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Persist one exact cleanup intent while atomically closing etcd admission, then issue non-revivable task Lease capabilities and conservative metadata checkpoints.

**Architecture:** Permanent immutable task/intent/link records plus leased exact claim/guard. Reuse existing native Stage guard/receipt arbitration and current five-point runtime ownership chain; no physical side effect or owner release in this unit.

**Tech Stack:** Existing Go1.25/etcd3.6 client, UUID and strict codec; no new dependencies.

**Spec:** `docs/superpowers/specs/2026-10-07-cleanup-task-authority-design.md`, a decomposition of the approved master migration.

## Global Constraints

- Branch `codex/etcd-state-management`; small verified/self-reviewed commits promptly; no push/merge/deploy. Never read/touch/stage unrelated `docs/superpowers/plans/2026-09-17-sentinel-kubernetes-129-compatibility.md`.
- Own scratch `.superpowers/sdd/2026-10-07-cleanup-task-authority/` only; completed predecessor evidence via tracked artifacts only, never sibling/deleted scratch.
- Only actually claimed tasks acquire Lease; no per-idle-sandbox client/goroutine/timer/ticker/Watch, no control/owner TTL, no new dependencies or production Manager/backend selection changes.
- Permanent records Lease0; task/intent/link immutable CreateRevision==ModRevision, mutable checkpoint ModRevision>=CreateRevision>0; current restore identity and exact original runtime/owner/control must be compared. Task3 TaskClaim/task guard use original Lease and exact value/LeaseValue/CreateRevision/ModRevision; never rebuild an old task claim or manufacture authority from diagnostic references. Task2 inherits the original Stage value/Lease/Create guard protocol, not a new guard ModRevision/local-deadline requirement.
- Existing Stage transaction budget64 operations, record64KiB and mutation256KiB remain; new task/intent4096, link2048, checkpoint4096 and claim4096-byte hard limits. Strict JSON rejects unknown/duplicate/omitted/null/trailing/nonUTF8 fields atomically.
- ttl in(0,24h], rounded up seconds; Task3 TaskClaim has conservative monotonic grant/renew send-time deadline. Existing Stage retains original native server-Lease fencing and no local monotonic deadline. Unknown/cancel/loss remains fail closed within the applicable protocol; permanent state is retained.
- Metadata checkpoint only pending/needs_reconciliation. No runtime call, closed-gate/remote settlement/End authority, owner/index/control deletion, legal reopen or age-only GC.
- Real etcd evidence uses owned isolated pinned local fixtures; worker never launches Docker or reads real .env/secrets. Root coordinates exact fixture inventory/cleanup and frozen-source consumption. Host static/compile/skip results must not be called real etcd or Linux runtime evidence.

## Review Focus

1. Same-byte task/claim/guard replacement must fail retained Create/Mod/Lease checks; Task1 strict envelope and Task3 original capability tests.
2. Destroy versus admitted or in-transit operations must preserve tokens and close only metadata; Task2 both transaction orders/concurrency/late prepared admit.
3. Lost claim or delayed checkpoint commit cannot be revived by cached Stage/reference; Task3 old prepared Stage after revoke/natural expiry/new claim tests.
4. Checkpoint strings/absent token/read timeout never authorize physical success; Task1 schema permits only two conservative states; Task2/3 permanent owner/intent preservation tests.
5. Caller Runtime pointer/metadata copies or capability copies must not mutate current authority; Task1 deep-copy reads, Task3 origin/self seal and immutable fences/race tests.

## File responsibilities

`task_records.go` strict public task/intent/link/checkpoint/reference records and private codec; `task_keys.go` fixed key families; `task_read.go` owned diagnostic reads and private coherent task bundle. `task_destroy.go` typed five-point destroy Stage. `task_claim.go` origin/self-sealed claim, renew/release; `task_claim_records.go` fixed leased record/envelope; `task_checkpoint.go` conservative typed Stage. Corresponding focused tests live beside these files. Modify existing files only at specifically necessary backwards-compatible seams; no copying a production state machine into tests.

### Task 1: Strict permanent task records and coherent diagnostic loads

**Files:** Create `internal/storage/state/etcd/task_records.go`, `task_keys.go`, `task_read.go`, `task_records_test.go`, `task_read_test.go`.

**Interfaces:** Produce `TaskReference{Namespace,RestoreEpoch,TaskID,SandboxID string; Partition uint8}`, `TaskRecord{Version uint32; Reference TaskReference; Kind,WorkspaceHash,CreationIntentID string; Generation,DataGateEpoch,ControlRevision int64; Runtime RuntimeReference; Snapshot SnapshotReference; ExpiresAt time.Time}`, `CleanupIntentRecord{Version uint32; Task TaskRecord; Attempt StageAttemptLocator}`, `TaskLinkRecord{Version uint32; Reference TaskReference}`, `TaskCheckpointState` constants `TaskCheckpointPending="pending"`, `TaskCheckpointNeedsReconciliation="needs_reconciliation"`, `TaskCheckpointRecord{Version uint32; Reference TaskReference; ClaimID,DetailDigest string; State TaskCheckpointState; Attempt StageAttemptLocator}`. Validation binds Namespace/partition/restore, canonical UUID, immutable version1/kindcleanup and exact current-runtime tuples. Cleanup intent nests exact task; 4096-byte limit includes both. Initial checkpoint has empty ClaimID/DetailDigest; otherwise both must validate. Produce fixed private key helpers and `LoadTask(ctx context.Context, ref TaskReference) (*TaskRecord,error)`, `LoadTaskCheckpoint(ctx context.Context,ref TaskReference) (*TaskCheckpointRecord,error)`; nil if exact absent, never capability. Reads identity fenced, routed keys exact, returned pointers owned; no discovery-time cache authority.

- [x] **Step1:** Add meaningful schema/envelope tests `TestTaskRecordsStrict`, `TestTaskKeys`, `TestTaskReadCopies` and `TestTaskReadIdentity`. Pin missing/duplicate/null/unknown/trailing/nonUTF8, malformed UUID/partition/namespace/identity, limits, unleased and immutable metadata, destination atomicity, caller copy independence; use actual etcd for identity-fenced read/absent/changed-restore outcomes. Before product implementation obtain a behavioral failing schema/read assertion; compile errors alone are not RED.
- [x] **Step2:** Implement the exact records/helpers/load APIs from the spec; reuse existing strict metadata decoder infrastructure rather than weakening existing codecs or adding generic privilege callbacks.
- [x] **Step3:** Run focused `go test -race ./internal/storage/state/etcd -run '^TestTask(RecordsStrict|Keys|ReadCopies|ReadIdentity)$' -count=1 -v`, scoped vet and diff-check. If integration skips without a fixture, report them as pending; propose the named selectors/source snapshot to Root for actual owned three-member run. Preserve complete argv/output/exit/env/source hashes and exact fixture scope.
- [x] **Step4:** Self-review then promptly commit each coherent tested slice; full Task1 report and original-range independent review. No Task1 claim/Destroy/checkpoint mutation API.

### Task 2: Atomic exact-control destroy intent

**Files:** Create `internal/storage/state/etcd/task_destroy.go`, `task_destroy_test.go`, `task_destroy_fault_test.go`; narrowly extend private Task1 read bundle if necessary.

**Interfaces:** Consume Task1 records/key helpers and existing five-point `loadOperationControl`, `beginStageWithBuilder`, CommitStage/ResolveStage/ReleaseStage. Produce `BeginDestroyInput{SandboxID,RequestID string; ExpectedControlRevision int64}`, `PrepareDestroy(ctx context.Context,in BeginDestroyInput,ttl time.Duration) (*Stage,TaskReference,error)`; Stage immutable and Reference diagnostic only. Exact builder persists attempt into intent/checkpoint. Initial checkpoint pending/emptyClaimID/DetailDigest, same transaction as permanent records. No return of foreign or copied authority.

- [x] **Step1:** Write `TestTaskDestroyAtomic`, `TestTaskDestroyAdmissionOrder`, `TestTaskDestroyUnknown` and `TestTaskDestroyFences`: stale expected revision; original five-point value/revision/lease/immutable recreation changes; atomic shared create revision and permanent owner retained; begin-first preserves token, destroy-first and late prepared admit refuse; competing destroy produces one link/intent; late commit-versus-abort and lost committed reply retain original attempt attribution.
- [x] **Step2:** Implement fixed five writes plus native committed Stage receipt under exact prior five-point/restore/guard/absence CAS. Preserve all original runtime/gate/expiry/snapshot fields and operations; no false current UTC expiry requirement for manual cleanup of an already expired object.
- [x] **Step3:** Run only named covering selectors on actual owned three-member etcd; host focused race/vet/diff checks, all raw/outcomes/source identities retained. Do not replay prior execution/kernel/native suites.
- [x] **Step4:** Self-review, small verified commits, full report and independent original Task2 range review.

### Task 3: Non-revivable task claim and conservative checkpoint

**Files:** Create `internal/storage/state/etcd/task_claim.go`, `task_claim_records.go`, `task_checkpoint.go`, `task_claim_test.go`, `task_claim_fault_test.go`, `task_checkpoint_test.go`; extend private coherent task bundle in `task_read.go`.

**Interfaces:** `TaskClaim` private origin/self/parentCtx/fences/claim-guard/originalLease/deadline/lost fields; `TaskClaimReference{Task TaskReference; ClaimID,WorkerID string; CreateRevision,LeaseID int64}` and copy-only `Reference()`. Produce `ClaimTask(ctx context.Context,ref TaskReference,workerID string,ttl time.Duration) (*TaskClaim,error)`, `RenewTaskClaim(ctx context.Context,c *TaskClaim) error`, `ReleaseTaskClaim(ctx context.Context,c *TaskClaim) error`, `TaskCheckpointInput{StageID string; ExpectedRevision int64; State TaskCheckpointState; DetailDigest string}`, `PrepareTaskCheckpoint(ctx context.Context,c *TaskClaim,in TaskCheckpointInput,ttl time.Duration) (*Stage,error)`. Only successful claim creation returns capability; no method adopts references/Lease.

- [x] **Step1:** Add focused actual-etcd `TestTaskClaimLifecycle`, `TestTaskClaimFences`, `TestTaskClaimUnknown`, `TestTaskCheckpointCAS`, `TestTaskCheckpointLostClaim`, `TestTaskClaimCopies`. Cover concurrent workers; original send-time conservative deadline; unknown/malformed grant/keepalive and cancellation; revoke/expiry never delete permanent data; exact claim/guard value/lease/create/mod mismatches, restore/control/owner/fence/index/task/intent/link mutation; zero/copied/foreign capability; old prebuilt checkpoint Stage after revoke/expiry cannot commit, new claim cannot revive old; conservative state schema and original Stage resolver; pointer/race immutability. Existing test-only fake RPC server may test malformed replies but cannot substitute for real Lease/CAS.
- [x] **Step2:** Implement exact coherent task bundle and original leased immutable pair; preflight complete native transaction budget before Grant. Initial task/intent/link/control destroying revision agree; later checkpoint has its own valid retained CAS revision/attempt. Renew validates complete fixed fences then keeps original Lease; loss is permanent, Release is cleanup only. Typed checkpoint builder writes only fixed conservative checkpoint and retains original task claim/guard checks in its Stage mutation.
- [x] **Step3:** Execute changed-seam actual three-member selectors, focused host race/vet/build/diff. Full report identifies every original/late/unknown outcome and precise fixture/compiler/source scope, no Linux runtime or remote settlement claim.
- [x] **Step4:** Small verified commits, independent whole Task3 range review, all named limitations individually disposed; mark source task complete only after accepted review.

## Final unit gate

- [x] Independent complete new-unit range review and exactly one collective final fix/scoped re-review if required. Permanently retain full reports/reviews/raw and ordered controller rulings with costs, individually dispose physical/production limits. Keep branch for ongoing migration, no overall completion. Runtime task-authenticated durable gate closure/dual drain/remote settlement/safe final release, then production Manager/runtime/images/pool/upload/scheduler/collector/deployment/complete Redis removal remain mandatory.
