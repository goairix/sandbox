# etcd 领域记录与 workspace 原子申请实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 交付永久领域记录与首次workspace申请组合事务，保留可恢复创建intent，按功能边界独立提交。

**Architecture:** 复用已提交的namespace、identity/restore guard与持久stage仲裁。领域代码固定计算key、验证关联及永久性；首次申请原子写request、owner、fence、intent、publishing control、immutable snapshot和ID placement，不接旧Store或生产Manager。发布及清理随后加入live intent/task claim、runtime gate和外部结算证据，本批不提供无能力的Bind/Delete接口。

**Tech Stack:** Go1.25、client/v3 3.6.14、三成员etcd3.6.15真实fixture、testify与race。

## 全局约束

- 当前分支`codex/etcd-state-management`；基线`b4c6232`。保留原有Sentinel计划修改，不暂存。
- owner/fence/control/snapshot/request/intent/placement全为永久key；不以Lease删除权威状态。generation为正int64，溢出拒绝且不写。
- stable workspace hash包含provider、SHA256(storage identity)、bucket、canonical prefix；采用现有length-framed SHA256算法，partition取workspace digest首byte。规范化prefix复用`storage.BuildWorkspacePrefix`，拒绝非canonical输入，不能静默归并不同用户输入。
- workspace storage identity必须对应operator Identity.StorageID；跨storage申请拒绝。原始storage identity和幂等key不进key或记录。
- 每记录schema1。字段ID/restore/request/snapshot version为合法ASCII segment≤128；hash/digest是lowercase64hex。runtime字段opaque UTF8非空无control≤128。控制/owner/intent/request编码≤4KiB（常见值目标<1KiB）；fence≤1KiB；snapshot整体≤64KiB。
- Task1 private codec严格unknown/trailing JSON、结构字段和Lease=0，不能独立提供授权；Task2所有领域读取验证namespace/完整key与value关联、restore binding；错误分类`ErrInvalidRecord`（输入）和`ErrCorruptRecord`（已存状态）。不接受破坏schema的silent fallback。
- 正常读仅point keys，default线性Txn比较base identity/restore；与记录同一Txn验证元数据，不依赖Watch授权，不扫描全量N。
- 本批不实现runtime创建、claim续接、active发布、owner释放、GC、operation gate或后端切换。中间状态不能用于生产分配器。目标是主方案阶段一的下一独立交付。

## Task 1：领域模型、身份与固定key

**Files:** Create `internal/storage/state/etcd/domain_identity.go`、`domain_records.go`、`domain_keys.go`及对应`*_test.go`；新增领域错误在`domain_records.go`。不改旧state接口。

**Produces:**

```go
func NewWorkspaceIdentity(provider, storageIdentity, bucket, prefix string) (WorkspaceIdentity, error)
func (w WorkspaceIdentity) Hash() string
func (w WorkspaceIdentity) Partition() uint8
// WorkspaceIdentity private fields include storage hash; only constructor can create valid identity.

type RuntimeReference struct { ID, UID, BootID string }
type SnapshotReference struct { Version, Digest string }
type SandboxPhase string // publishing, active, workspace_exclusive, destroying, cleanup_pending

type WorkspaceOwnerRecord struct {
    Version uint32; WorkspaceHash, SandboxID, IntentID, RestoreEpoch string
    Generation int64; Runtime *RuntimeReference; MountAttempt uint8
}
type WorkspaceFenceRecord struct {
    Version uint32; WorkspaceHash, RestoreEpoch string; Generation int64
}
type SandboxControlRecord struct {
    Version uint32; SandboxID, WorkspaceHash, IntentID, RestoreEpoch string
    Generation, DataGateEpoch int64; Phase SandboxPhase
    Snapshot SnapshotReference; Runtime *RuntimeReference; MountAttempt uint8
    ExpiresAt time.Time
}
type SandboxSnapshotRecord struct {
    Version uint32; SandboxID string; Snapshot SnapshotReference; Payload json.RawMessage
}
type SandboxPlacementRecord struct {
    Version uint32; SandboxID, WorkspaceHash, IntentID, RestoreEpoch string
    Partition uint8; Generation int64 // partition must equal workspace digest first byte
}
type CreationIntentRecord struct {
    Version uint32; IntentID, SandboxID, WorkspaceHash, RequestHash, ConfigurationDigest, RestoreEpoch string
    Generation int64; Phase string // pending or published
}
type CreationRequestRecord struct {
    Version uint32; RequestID, RequestHash, ConfigurationDigest, IntentID, SandboxID, WorkspaceHash, RestoreEpoch string
    Generation int64; Phase string // pending or completed
}
func (record <each type>) Validate() error
func encodeDomainRecord(record interface{ Validate() error }) (string,error)
func decodeDomainRecord(kv *mvccpb.KeyValue, record interface{ Validate() error }) error
func snapshotDigest(payload json.RawMessage) (string,error)
func requestKeyHash(principal, idempotencyKey string) (string,error)
func (n Namespace) workspaceKeys(w WorkspaceIdentity) (owner,fence string,err error)
func (n Namespace) sandboxKeys(partition uint8,sandboxID,snapshotVersion string) (control,snapshot string,err error)
func (n Namespace) intentKey(partition uint8,intentID string) (string,error)
func (n Namespace) requestKey(hash string) (string,error)
func (n Namespace) placementKey(sandboxID string) (string,error)
```

Use explicit JSON tags snake_case. Optional runtime uses omitempty; publishing requires nil runtime/mount0; active/workspace_exclusive control require exact runtime and mount≤1; destroying/cleanup_pending permit nil runtime+mount0 or valid exact runtime+mount≤1. Nil runtime never proves absence or safe cleanup: durable intent remains for attribution and no release API exists in this batch. owner allows provisional nil/mount0 or bound runtime/mount≤1. Intent/request only two named phases; completed/published cannot automatically imply target side effects were fenced.

- [x] 写身份collision/路径与storage差异测试、记录不合法schema/generation/runtime/snapshot/hash/size/leased/unknown JSON测试，先观察RED。

```go
w, err := NewWorkspaceIdentity("s3", "storage-a", "bucket", "team/project/")
require.NoError(t, err)
w2, err := NewWorkspaceIdentity("s3", "storage-b", "bucket", "team/project/")
require.NoError(t, err)
require.NotEqual(t, w.Hash(), w2.Hash())
_, err = NewWorkspaceIdentity("s3", "storage-a", "bucket", "team/../project/")
require.ErrorIs(t, err, ErrInvalidRecord)
```

- [x] 实现上述值类型、validation/strict bounded codec及固定key。不保存raw storage或idempotency key；snapshot digest必须覆盖json.Compact后的payload字节（保留数字及object key顺序，不经float转换），mutation会复制caller数据；digest算法不将JSON数字转float。
- [x] `go test -race ./internal/storage/state/etcd -run '^TestDomain' -count=1` GREEN；spec review→quality review，修复必要问题并复测。
- [x] 独立提交模型/身份/key，不等领域事务或全Redis替代完成。

## Task 2：线性领域读取与首次申请

**Files:** Create `domain_read.go`、`workspace_acquire.go`、`workspace_acquire_test.go`。只使用Task1准确类型/keys与已提交Backend API。

**Consumes:** `Backend.BeginStage(ctx, partition, requestID, stageID, Mutation, ttl)`、`CommitStage`、`ResolveStage`、`ReleaseStage`、`baseComparisons`、Task1 records/keys。

**Produces:**

```go
type AcquireIntentInput struct {
    Workspace WorkspaceIdentity; Principal, IdempotencyKey string
    RequestID, IntentID, SandboxID, SnapshotVersion string
    Payload json.RawMessage; Now time.Time; TTL time.Duration
}
type AcquireDisposition string // created, replay, occupied
// caller-generated IDs must be validated and allocated before invocation.
// digest includes workspace hash, compact snapshot payload digest and requested TTL, not Now/allocated IDs.
type AcquireIntentResult struct {
    Disposition AcquireDisposition; Outcome Outcome; Reference StageReference
    Request CreationRequestRecord; Owner *WorkspaceOwnerRecord
    GuardCleanupError error // best-effort original Lease cleanup, does not change known outcome
}
func (b *Backend) AcquireIntent(ctx context.Context,input AcquireIntentInput) (AcquireIntentResult,error)
func (b *Backend) LoadRequest(ctx context.Context,principal,idempotencyKey string) (*CreationRequestRecord,error)
func (b *Backend) LoadOwner(ctx context.Context,w WorkspaceIdentity) (*WorkspaceOwnerRecord,error)
func (b *Backend) LoadPlacement(ctx context.Context,sandboxID string) (*SandboxPlacementRecord,error)
func (b *Backend) LoadControl(ctx context.Context,partition uint8,sandboxID string) (*SandboxControlRecord,error)
func (b *Backend) LoadSnapshot(ctx context.Context,partition uint8,sandboxID string,ref SnapshotReference) (*SandboxSnapshotRecord,error)
```

Input TTL positive≤365days, Now nonzero and UTC-normalized representable year1..9999, ExpiresAt=Now+TTL. digest encoding exact framed hash(workspaceHash,payloadDigest,TTL nanoseconds). `ErrIdempotencyConflict` means same principal/key different digest even when workspace partitions differ; `ErrGenerationOverflow` for fence.MaxInt64. Invalid supplied IDs/data fail before anyStage/Grant. Backend's bound storage identity must match workspace storage hash. Snapshot payload is immutable validated UTF8 JSON; compact before digest and storage, wire encoding must preserve compact payload bytes/digest consistently (Encoder.SetEscapeHTML(false), include<>&/U+2028/largeinteger roundtrip); reject if encoded record exceeds64KiB beforeGrant.

Read helper `readDomain(ctx, ops...clientv3.Op)` executes If(base).Then(point reads).Else(identity/restore), validates header cluster and permanent exact metadata in same result. Validate permanent records, version, all hash/ID relations and restore bindings. The private generic codec validates only envelope/value; read methods must separately check returnedKV.Key exactly equals requestedkey and context linkage. Control workspace digest firstbyte must equal requestedpartition; snapshot sandboxID/version/digest must equal requestedref; request hash must equal derivedkey; owner/fence workspace hash must equal expected workspace; placement ID must equal queriedID. Include validJSON wrongkey/value regression tests; privatecodec cannot independently authorize work. Missing returns nil; malformed/leased/wrong key record returnsErrCorruptRecord; restore mismatch returnsErrIdentityMismatch. `LoadOwner` reads owner+fence in sameTxn, requires matching generation if owner exists; fence without owner is valid retained generation. `LoadControl` does not prove actualruntimehealth or authorize execution.

Acquire reads request/owner/fence plus proposed control/snapshot/intent/placement in one bounded linearTxn. Same-key request is decoded first: differentdigest409-equivalenterror, samedigest returns replay of storedpending/completed record without mutation or changingTTL/config; request fields/key/restore must bevalid. If owner exists after valid fence linkage return occupied with existing owner, no overwrite and no newrequest. Corrupt/leased existing records failclosed rather than occupancy success. If owner absent and nonzero fence valid, nextgeneration=fence+1; missingfence starts1. Existing proposed control/snapshot/intent/placement absentconditions reject collision, including same sandboxID acrossdifferentworkspace partitions. Permanent indexes/sandbox/id placement bindsID toworkspacehash/partition/generation/intent/restore and is written in sameTxn; futurecleanup conditionally removes it. LoadPlacement enables ID-only API routing. Absence compare isCreateRevision0, existingfence compareModRevision plus rawvalue/Lease0. Every newlywrittenkey checksabsence and every existingread used for authorization checks revision/permanentlease.

Encode seven permanent records, prepareMutation bounds before anyGrant. BeginStage(requestID,"acquire",acquireStageTTL=30*time.Second) then CommitStage. The guard Lease TTL is independent of business TTL (≤365days), never pass businessTTL toBeginStage. Return exactReference on commitunknown/CASconflict, never call absence proof or erase intent/owner. Defer bounded best-effort Release originalStage, store failure separately inGuardCleanupError. Abort resolver remains separately explicit API: caller persists/refetches request/intent and resolves exact stage; do not abort wholependingrequest. No automaticruntime work or claim inferred fromcommitted stage.

- [x] 写真实三成员并发与unknown测试，先观察RED。

```go
result, err := b.AcquireIntent(ctx, input)
require.NoError(t, err)
require.Equal(t, OutcomeCommitted, result.Outcome)
require.Equal(t, AcquireCreated, result.Disposition)
request, err := b.LoadRequest(ctx,input.Principal,input.IdempotencyKey)
require.NoError(t,err)
require.Equal(t,"pending",request.Phase)
// real Range verifies owner/fence/control/snapshot/intent/request/placement/receipt share ModRevision and Lease0.
```

- [x] 实现point read helper、公开read methods、bounded atomicacquire。并发同workspace16~32请求仅一个创建；其他occupied或exactstageCASconflict，owner不能被删除。跨partition相同sandboxID竞争只能一胜者；相同幂等key不同workspace和TTL返回conflict；相同digest重放保留原ID/TTL/配置且无rewrite。
- [x] 验证generation永久递增及MaxInt64不写；未知commit丢响应后resolver恢复committed且request仍pending；迟到完整Txn先abort再放行不创建任何领域record。原restore更改、leased/corrupt owner/fence/control/snapshot/request/intent，storage mismatch及写入预算失败都不能授权新申请。客户端input在提交前更改不能影响已固定mutation；48小时/365天与短业务TTL不改变固定30秒stageLease。
- [x] `bash scripts/test-etcd-state.sh -v`，真实race无skip；`go test ./...`、`go vet ./internal/storage/state/etcd`、sandbox构建、`git diff --check`。
- [x] spec review→quality review，修复并复测；保存实际结果，单独提交领域申请事务。

## 计划审查补充

独立审查指出仅有partition control不能保证全局sandboxID唯一，也不能支持ID-only lookup；当前已增加indexes/sandbox/id placement、同Txn无记录比较与写入、精确读取及跨partition同ID竞争验证。placement为权威永久记录，不能随Lease删除。

## 后续紧接的协议

下一计划交付可恢复intent claim（guard/claim精确value、Lease、CreateRevision、restore）、绑定exact Runtime UID/BootID及已确认mount/data gate的Publish组合事务，同Txn完成owner/control/runtimeindex/request及Publish receipt。cleanup先完成operation/gate与sender fence/remote settled证据，再实现ReleaseWithEvidence，不能将bool断言或owner不存在当作外部结算证据。之后推进pool/operation/scheduler/runtime和Redis移除；保持每项可验证功能独立提交。
