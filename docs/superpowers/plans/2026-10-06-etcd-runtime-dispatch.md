# etcd runtime dispatch 持久声明实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [x]`) syntax for tracking.

**Goal:** 让 pending 创建 intent 的 runtime 调用身份和输入可持久恢复，禁止未知结果时换 operation 再创建。

**Architecture:** 使用原 creation claim 的24条比较，将 immutable dispatch declaration、input 与该元数据尝试的 committed receipt 同事务提交。固定 intent family 只允许一个声明；重放读原 operation，修改调用输入冲突。先在 Stage 内生成不可复用 attempt，再用 private builder 构造带 locator 的业务 mutation，避免把 mutation digest 写进其自身而产生循环。

**Tech Stack:** 已固定 Go1.25/client3.6.14/server3.6.15，无新依赖；真实三成员 fixture 与 race。

**Spec:** `docs/superpowers/specs/2026-10-06-etcd-state-management-design.md` 创建、task 能力、未知结果与外部 fencing 章节。

## Global Constraints

- `codex/etcd-state-management`，基线 `5d6f03e`；原有 Sentinel 计划修改不动、不暂存。每个可验证单元独立提交。
- 永久声明和input不附 Lease、不可覆写；metadata committed不证明 runtime 已创建，也不授予调用外部 target 的能力。本批不调用 runtime、不接生产 wiring、不释放 owner、不做 GC。
- 后续实际调用必须经过可信 dispatcher/helper 的原 claim、业务期限、restore/epoch/gate 检查；持久声明只提供恢复查询的归因信息。unknown target结果保留原 operation，不能另发新 operation。
- 正常路径仅固定 point keys，不 scan 全量沙盒N，不为每沙盒新增常驻 goroutine/Lease。stage Lease固定30秒，与业务TTL/task Lease独立。
- ID/restore segment≤128，digest lowercase64hex；scope root≤512、key≤1024。声明编码≤4096 bytes、input完整编码≤65536 bytes；JSON保留compact字节数字/顺序/HTML/Unicode，先复制caller数据。
- 所有读取比较base identity/restore，检查header cluster、exact key、永久 Lease0、schema、正revision和上下文。不能以missing或本地lost推断外部调用失败。
- 原始 claim 的 server CAS 不被更换；失租后旧 Stage 仍由原 guard/claim/control 与 exact receipt 仲裁。本地 lost不能撤回已复制比较。

## Review Focus

1. 相同 intent 并发声明不同 target/input，不能生成两个 operation；元数据 loser unknown须裁决 exact stage。
2. 声明实际提交但丢回复，新 Backend 能点读原 operation和原receipt，不能换ID执行。
3. guard/claim重建或restore/control变更后的迟到完整事务必须失败。
4. schema合法但错 namespace/intent/sandbox/generation/snapshot/claim或 receipt locator 的记录拒绝；声明/input/receipt必须同一首次revision。
5. caller修改payload、输出entry或Stage builder返回的mutation不能污染既定digest和永久记录。

## Task 1：Stage attempt locator 与一次性 private builder

**Files:** Create `internal/storage/state/etcd/stage_attempt.go`、`stage_attempt_test.go`；Modify `stage.go`，仅提取既有BeginStage核心。

**Interfaces:**

```go
type StageAttemptLocator struct {
    Namespace string; Partition uint8
    RequestID, StageID, AttemptID, RestoreEpoch string
}
func (l StageAttemptLocator) Validate() error
func (l StageAttemptLocator) reference(digest string) StageReference // private
func (b *Backend) beginStageWithBuilder(ctx context.Context,partition uint8,requestID,stageID string,ttl time.Duration,build func(StageAttemptLocator)(Mutation,error)) (*Stage,error)
```

Locator使用显式snake_case JSON。Validate验证canonical namespace root（可从末尾scope/cell复原NewNamespace）、合法ID/restore与canonicalUUID，不保存digest。builder为private package API，attemptID由内部新UUID选定，不接受caller指定旧attempt。标准BeginStage用返回原input的闭包复用核心，其签名、预算、guard与receipt语义保持一致。输入/locator validation、builder error、prepareMutation全部在任何Grant之前；build仅调用一次、在任何RPC之前，mutation随后深拷贝和计算digest。locator.reference补入此digest，StageReference/receipt包含完整身份。

- [x] 写真实 `TestStageAttemptBuilderAtomicLocator`：mutation保存locator，receipt.ref的namespace/p/request/stage/attempt/restore匹配，digest非空且commit同revision；先stub产生真实assertion RED。
- [x] 写builder调用次数1、nil/error/invalid输出不Grant；caller修改返回mutation、重复调用各产生新attempt，resolver先abort与迟到完整Txn不写。
- [x] 实现private builder；标准Stage回归和新定向race GREEN；自审达到要求即独立提交，再做spec→quality独立审查，修复另作小提交。

## Task 2：dispatch record、固定key与可恢复线性读取

**Files:** Create `runtime_dispatch_records.go`、`runtime_dispatch_read.go` 及对应tests；Modify `domain_records.go`的closed codec新增两种类型。

**Interfaces:**

```go
type RuntimeDispatchKind string // RuntimeDispatchCreate="create", RuntimeDispatchPrepare="prepare"
type DispatchClaimReference struct { ClaimID,WorkerID string; CreateRevision,LeaseID int64 }
type RuntimeDispatchRecord struct {
    Version uint32
    IntentID,SandboxID,WorkspaceHash,RequestHash,ConfigurationDigest,RestoreEpoch string
    Generation,DataGateEpoch int64
    Snapshot SnapshotReference; ExpiresAt time.Time
    OperationID string; Kind RuntimeDispatchKind; Target,PayloadDigest string
    Claim DispatchClaimReference; Attempt StageAttemptLocator
}
type RuntimeDispatchInputRecord struct {
    Version uint32; IntentID,SandboxID,WorkspaceHash,RestoreEpoch,OperationID,PayloadDigest string
    Generation int64; Payload json.RawMessage
}
func (r RuntimeDispatchRecord) Validate() error
func (r RuntimeDispatchInputRecord) Validate() error
func (n Namespace) runtimeDispatchKeys(p uint8,intentID string) (declaration,input string,error)
type RuntimeDispatchEntry struct { Record RuntimeDispatchRecord; Payload json.RawMessage; Reference StageReference }
func (b *Backend) LoadRuntimeDispatch(ctx context.Context,w WorkspaceIdentity,intentID string) (*RuntimeDispatchEntry,error)
```

声明固定key=`intentKey/runtime-dispatch`，input=`intentKey/runtime-input`。两种record永久schema1、共用outer归属；operationID canonicalUUID，target合法opaque UTF8无control≤128；kind只create/prepare；claim ID canonicalUUID、worker segment≤128、epoch/Lease正数。声明attempt.Namespace=current namespace、partition=workspacehash首byte、restore一致、stageID固定`runtime_dispatch`、requestID以后声明时绑定原request。input digest覆盖compact payload，input与声明ID/hash/generation/restore/operation/digest必须相同；expires UTC非零有效。

Loader先线性读两个固定key，both missing返回nil而非外部失败证据，half missing拒绝。定位receipt后再同一线性Txn读declaration/input/exactreceipt（三个point keys），重新验证整个entry。三key的CreateRevision=ModRevision>0且彼此相同，receipt永久、严格JSON/UTF8/unknown/trailing、schema1、outcome committed、locator全部字段相同、digest lowercase64hex；从该实际receipt取得完整StageReference，仅可仲裁而不可提交。receipt存在但声明缺失不自动查询；声明存在却receipt缺失/aborted/错位为corrupt而不Resolve。stored restore错返回IdentityMismatch；损坏receipt返回CorruptReceipt，其余损坏CorruptRecord。若首次存在而第二次三个都消失，返回nil，供后续合法GC竞态使用；不推断target状态。

- [x] 写model/codec与真实loader RED，测试正确永久同revision三key可从新Backend恢复ref；wrong value/key/lease/epoch/digest/locator、half missing、receipt aborted或wrong ref拒绝。
- [x] 实现closed codec扩展、keys、bounded point loader；保留精度及HTML/Unicode/原JSON顺序、输入输出复制及完整wire预算测试。
- [x] 定向race/vet GREEN，自审达到要求即独立提交，再做spec→quality独立审查，修复另作小提交。

## Task 3：原 creation claim 下的原子声明

**Files:** Create `runtime_dispatch.go`、对应happy/fault/validation tests；Modify `creation_claim.go`、`creation_claim_read.go`只增加private不可变workspace identity/request/control上下文副本，不改变claim guard/Lease/比较数。

**Interfaces:**

```go
type RuntimeDispatchInput struct { Kind RuntimeDispatchKind; Target string; Payload json.RawMessage }
type RuntimeDispatchDisposition string // DispatchDeclared="declared", DispatchReplay="replay"
type RuntimeDispatchResult struct {
    Outcome Outcome; Disposition RuntimeDispatchDisposition; Reference StageReference
    Entry *RuntimeDispatchEntry; GuardCleanupError error
}
func (b *Backend) DeclareRuntimeDispatch(ctx context.Context,c *CreationClaim,input RuntimeDispatchInput) (RuntimeDispatchResult,error)
```

ClaimCreation保存原immutable workspace identity，并从已验证bundle保存private request和control值（publishing的runtime=nil），包含原requestID/hash/configdigest、snapshot与expires；不暴露该上下文的可变指针。workspace identity用于调用既有LoadRuntimeDispatch，不从裸hash重建或绕过storage authority校验。原24条比较照旧，只有活跃C保留少量副本。声明先验证ctx/origin/current local能力、kind/target、私有payload compact copy/full编码预算；读取existing entry并核对其归属与当前claim上下文。已有合法声明且kind/target/inputdigest相同则replay原operation/ref（允许来自新claim的恢复），不Grant、不改Lease、TTL或原creator；不同输入ErrIdempotencyConflict，错位或corrupt拒绝，不覆盖。replay仅返回metadata evidence，不授予旧creator能力。

不存在时内部生成新operationUUID；使用Task1 builder生成新的metadata attempt，构造两个immutable records，其归属均来自private claim上下文，snapshot/expiry不从caller覆盖。mutation包含原24cmp及declaration/input CreateRevision0，两个永久writes；总42项含14stage预留，原预算不放宽。Begin stage30s后重检ctx/local claim，CommitStage；失败/unknown返回exactReference且Entry=nil，不能伪称declared。只有knownCommitted返回declared entry；background bounded Release原stage记录cleanup错误，不能降低known outcome，也不释放creation claim或owner。

- [x] 写真实声明/重放与graph不变RED；declaration/input/receipt同revision、永久Lease0，原domain和claim不改。
- [x] 写16并发声明一个originaloperation、同输入重放保留originalID/ref/expiry，不同kind/target/payload冲突且不Grant；重新领取claim/新Backend只读原声明，拒绝另operation。
- [x] 写lost real commit reply→ResolveCommitted→新Backend Load原operation，delayed完整Txn先aborted后不写；claim失租/同值重建、restore/control变化、callerpayload mutation、invalid/wire budget beforeGrant、cleanup failure与Outcome分开。
- [x] 实现atomic声明；真实定向race、全仓/vet/build/gofmt/diff验证及自审后即独立提交；随后执行全fixture、spec→quality及本批跨模块final review，保存报告，修复另作小提交。

## 紧接的发布单元

之后实现可信target publication证据协议与Publish：证据绑定exact runtime UID/BootID、原dispatch operation和inputdigest、当前claim epoch、data gate与一次mount attempt；不能接受caller的bool完成断言。证据认证及可信helper来源成立后，owner/control/runtime index/request/intent/publication receipt组合事务才可active。证据包可先实现和验证，实际launcher/adapter接入按主设计继续；中间阶段不作为生产后端切换。
