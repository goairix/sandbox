# 原生 etcd Operation 准入 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox syntax for tracking.

**Goal:** 为已发布 sandbox 提供短期、原 Lease 绑定的 data/mutation 准入、续期、取消与精确未知结果仲裁，普通空闲 sandbox 不增加 Lease 或 goroutine。

**Architecture:** 永久 owner/control/placement/fence/runtime index 保留；只有正在准入或执行的 operation 才使用独立 Lease，其 guard、token、receipt 和可选 mutation lock 全部附同一原 Lease。操作能力是私有 origin 与单调期限绑定对象，public reference 只供只读归因和仲裁；本批不发 runtime 指令。实际执行签发、永久 effect intent、target gate/terminal、exclusive/destroy、业务 TTL 更新与 snapshot mutation 属后续组合事务，不用本批取消或 token 缺失冒充排空。

**Tech Stack:** Go，已固定的 etcd client/server，现有 constructor authority/controlled clock、bounded identity-fenced Txn 与真实 owned 三成员集成测试。

**Spec:** `docs/superpowers/specs/2026-10-06-etcd-state-management-design.md`，Operation gate 和 Lease、事务未知结果、执行 fencing 章节。用户已批准一至五阶段连续执行，本计划是阶段二的一个独立单元，沿用当前功能分支及逐单元提交；不增加确认流程。

## Global Constraints

- operation 专用原生 Lease 默认 30 秒，约 10 秒 KeepAlive；不为每个 operation 建 TCP 连接。本批固定请求 30 秒，以显式 Renew 调用续原 Lease；共享续期调度在后续实际调用层接入，不增加后台 goroutine/ticker。
- 控制记录永久 Lease0；短期 operation attempt guard 与 receipt 都附原 operation Lease。失租、unknown renew 或单调 deadline 越界后能力不可复活，不新 Grant，不同 ID 重建，不由 public reference 构造 capability。
- 业务到期采用 constructor 受控 UTC，Δ=1 秒，`now+Δ>=expires_at` 拒绝新准入；未知时钟或误差预算超限拒绝，不接 caller Now，不以本机墙钟兜底。已入场操作可以跨业务到期排空，续期不会修改业务 expires_at。
- 线性读取与 mutation 比较永久 identity/restore；namespace root<=512 bytes、key<=1024 bytes、每 Txn 总预算<=64、保守序列化字节预算<=256KiB。新 ephemeral guard/token<=4096 bytes、receipt<=2048 bytes；strict UTF8/unknown/duplicate/null/missing/trailing JSON，UUID/hash/positive revision/Lease 与同 Lease envelope；所有切片输入和输出复制。
- 原有永久 Stage / CreationClaim / Publish 协议、64预算和 errors.Is 语义不改。operation receipt 是不同类型和 key family，禁止调用 ResolveStage 或用永久 receipt 解码器处理 operation receipt。
- 固定 point 读取，不按存量N scan，不写每 sandbox 热操作计数器；元数据 Lease 数量按活跃操作 C 增长，不随空闲 sandbox N 增长。
- 每验证自审通过的功能单元立即独立提交；独立审查发现修复另提交。保留既有 Sentinel 计划修改，不读取 .env/凭证，不切生产、不调用 runtime/mount、不 push/merge。

## Review Focus

1. admission reply 丢失只能恢复 committed/aborted/expired 证据，不能凭元数据恢复原 capability 或发送第二条指令。
2. guard/token 同值重建、旧 boot/restore/control revision 与迟到完整 Txn 必须被原比较拒绝；missing 只说明能力失效，不说明外部效果失败。
3. mutation lock 与 token 同 Lease、同 admission Txn，失租不留下永久死锁，也不意味着已有 data 任务排空。
4. delayed KeepAlive 回复不能延长本地期限；unknown renew 后所有后续动作拒绝，即使服务端暂时仍有 token。
5. Cancel 失联或与 Renew 竞争只能终止本地能力，不能撤销其他 Lease、回收 owner 或写虚假外部终态。

## Task 1：ephemeral schema 与 coherent 准入上下文

**Files:** Create `internal/storage/state/etcd/operation_records.go`、`operation_read.go`、对应 records/read tests。不把 ephemeral type 塞入 Lease0 domain codec。

**Interfaces:**

```go
type OperationKind string // OperationData="data", OperationMutation="mutation"
type OperationOutcome string // unknown, committed, aborted, expired
// expired 表示短期 guard/receipt 已消失、旧 Txn 无法提交，不是 target 终态。
type OperationReference struct {
    Namespace,RestoreEpoch,RequestID,SandboxID,OperationID,Digest string
    Partition uint8; Kind OperationKind; LeaseID int64
}
type OperationRecord struct {
    Version uint32; Reference OperationReference
    WorkspaceHash,IntentID string; Generation,DataGateEpoch,ControlRevision int64
    Runtime RuntimeReference; Snapshot SnapshotReference; ExpiresAt time.Time
}
type operationReceipt struct { Version uint32; Reference OperationReference; Outcome OperationOutcome }
func (*Backend) loadOperationControl(context.Context,string)(*operationControlBundle,error)
func (Namespace) operationKeys(OperationReference)(token,guard,receipt,mutation string,err error)
```

Public typed fields全部 snake_case JSON；OperationID为内部 canonical non-nil UUID，RequestID为合法 segment<=128。guard 与 token 保存同一个完整 OperationRecord（guard 首次 CRev 与 admission token CRev 可以不同），receipt 仅 committed/aborted。Reference LeaseID=0 只允许 Begin 返回 Grant 未知的预分配诊断，不能参与 operationKeys/Resolve；任何合法 wire、已知 Grant 的 ref 与 capabilities 都要求 LeaseID>0。Digest 是 Task2 原 descriptor 的 SHA256，不以 LeaseID=0 当可恢复凭证。

keys=`p/xx/sandboxes/<sandboxID>/operations/<operationID>`，guard/receipt=`p/xx/operation-attempts/<operationID>/guard|receipt`，mutation=`p/xx/sandboxes/<sandboxID>/mutation`。operation guard 不与 Stage attempts 混用。解码器要求 KV.Lease==Reference.LeaseID>0、CRev=ModRev>0（ephemeral immutable）；receipt与token committed时必须同首次revision。receipt malformed 返回 ErrCorruptReceipt，其他 malformed 返回 ErrCorruptRecord；destination失败时不污染旧对象。

loadOperationControl先固定point发现 placement，再发现 control 中的 runtime UID；最终在一个 readDomain Txn 读取 placement/control/owner/fence/runtime index五条。bundle私有保存 Control、Partition、按该顺序的 Keys/KVs；mutable owner/control无需 CRev=ModRev，但 Lease0 与有效revision必须成立，UID index要求 immutable。所有归属、restore、sandbox/intent/workspace/generation、exact Runtime ID/UID/BootID、MountAttempt、hashPartition相等，owner/fence generation一致；全五个值都参与后续 ModRevision+Lease0 比较。control PhaseActive 才可准入，其他phase使用新 sentinel ErrOperationAdmissionClosed；缺失control/placement同样closed，部分链条或不一致为corrupt。发现期间变化可返回conflict而不混合两个快照；无自动重试/无全量scan。

- [ ] `TestOperationRecordStrictCodec` 行为 RED→GREEN：leased valid roundtrip、literal lease0/别Lease、null/duplicate/missing/unknown/trailing、nil UUID、oversize和 destination/copy边界拒绝；同 UUID operation key不受 caller路径拼接影响。
- [ ] `TestOperationControlCoherentBundle` 真实已签名 Publish fixture RED→GREEN：plain/fuse active、其余phase拒绝、五条链条逐项missing/tamper/restore/boot mismatch，以及 discovery中替换control/placement不能拿旧UID组合授权。
- [ ] `go test ./internal/storage/state/etcd -run '^TestOperation(Record|Control)' -count=1` 实际 endpoints；定向race/vet/gofmt/diff，自审即小提交，spec→quality独立review。

## Task 2：一笔原 Lease 的短期入场

**Files:** Create `operation_admission.go`、`operation_admission_test.go`、`operation_admission_fault_test.go`。消费 Task1 types/bundle，不修改原 Stage。

**Interfaces:**

```go
type OperationCapability struct // private origin/parent context, copied record/fences, original Lease/guard revision, mutex/deadline/lost
func (*OperationCapability) Reference() OperationReference
func (*OperationCapability) Control() SandboxControlRecord // clone Runtime pointer
// 不提供从 Reference/Record 重构能力的构造函数。
type BeginOperationInput struct { SandboxID,RequestID string; Kind OperationKind }
type BeginOperationResult struct {
    Outcome OperationOutcome; Reference OperationReference
    Capability *OperationCapability; GuardCleanupError error
}
func (*Backend) BeginOperation(context.Context,BeginOperationInput)(BeginOperationResult,error)
```

只支持data/mutation；exclusive必须由后续关闭准入+target gate协议提供，本批拒绝该值。创建内部OperationID，读取 coherent bundle、验证 configured clock/expiry。两种准入都比较mutation不存在；mutation同时建立leased mutation lock，阻止新的data/mutation入场，已有data仍需后续target排空，不能认为它们已经结束。不会为data改control或增加counter。

Digest=SHA256(`"sandbox-operation-admission:v1\x00"`+确定性typed JSON descriptor)，descriptor包含 Version1、Namespace/restore、RequestID/SandboxID/internalOperationID/Kind、全部 OperationRecord context（不含 LeaseID/Digest），以及五条 bundle 原 Key/ModRevision/Value 按Task1固定顺序。复制与保守operation/byte/key/record预检均在任何 Grant 前。placeholder LeaseID只用于大小上界预检；实际guard/token/receipt只使用已知原Lease。保存预分配ref，若Grant未知、没有已知LeaseID，则返回 unknown/nil capability且不发送guard Txn。

Grant30s使用发送前单调sent+server TTL确定deadline；检查响应cluster/ID/TTL、ctx/deadline。guard Txn比较base4、guard/receipt/token不存在，写单leased guard。unknown guard-init绝不发送admit；已知lease独立bounded回收，错误另存GuardCleanupError。成功guard CRev固定不可复建。然后再ctx/live/controlled clock/expiry检查。

admit Txn比较base4、五条记录ModRevision/Lease0十条、guard Value/Lease/CRev三条、receipt/token/mutation三absence，Then leased token+committed receipt（mutation另leasedlock）。4+10+3+3=20 comparisons，2/3writes和最多5个失败point读远低64；预检计算最坏分支。失败分支固定点读取identity/restore/receipt/guard/token，严验响应header/key/count/lease。只在确定committed且原guard/token/receipt仍live、本地deadline与ctx未过期才返回Capability。reply未知保持ref/unknown/nil cap；失败不能构造cap。已知不能交付的入场或CAS冲突独立回收原Lease，known historical committed证据与cleanuperror分开；本批未开始任何target操作。能力保留原入场parent context；后续Renew必须检查它仍未取消，不能用新background context复活已经取消或到期的原请求。成功后由调用层持有原cap，不能自动回收；关闭Backend不回收别人的Lease。

- [ ] `TestBeginOperationAtomicAdmission` stub真实 RED→GREEN：data token+receipt同revision、mutation再lock同revision、全部Lease相同；原五条永久值/revision不改；两个data均成功且Lease不同；mutation竞争一次，持lock新data拒绝；局部终止一项不影响另一项。
- [ ] `TestBeginOperationPreGrantValidation`：非法kind/context/ID、未配clock、clockunknown/expired/budget全部beforeGrant；输出对象修改不污染私有能力/比较。
- [ ] `TestBeginOperationOriginalFences`：Grant之后换control到destroying、owner/index/placement/fence重写/同值重建、restore变化阻止原admit；guard同值重建不能沿用CreateRevision。
- [ ] `TestBeginOperationUnknownReplies`：真实丢Grant/guard/admit回复不得返回cap；已知Lease回收失败单独字段，exactref保留；延迟guard或完整admit不误判外部abort。后者待Task3 resolver证明。
- [ ] 真实全package、定向race/vet/gofmt/diff、自审立即功能小提交，spec→qualityreview。

## Task 3：短期仲裁、原 Lease 续期及显式取消

**Files:** Create `operation_lifecycle.go`、`operation_resolve.go`、各自 tests。消费 Task2 opaque capability，复用 bounded clock/identity和Lease client，不增watch/timer。

**Interfaces:**

```go
func (*Backend) ResolveOperation(context.Context,OperationReference)(OperationOutcome,error)
func (*Backend) RenewOperation(context.Context,*OperationCapability) error
func (*Backend) CancelOperationCapability(context.Context,*OperationCapability) error
```

Resolve只接受已知Lease正数ref，绝不Grant或返回cap。一次coherent point读guard/token/receipt/mutation；若guard/token/receipt三个自身key全部不存在返回expired（因UUID不复建旧admit永不可提交，不代表外部效果终态）；别operation的合法mutation lock不改变这一结果。自身malformed/半套为corrupt/unknown。原guard存在而无receipt/token时，解析精确record，比较base、guard Value/Lease/CRev、receipt/token不存在，Then aborted receipt附同一原Lease。admit与abort争相同receiptabsence，最多一者赢；guard丢失则不建abortmarker，不新Lease。已有committed必须有exact token且同revision、同Lease，mutation模式lock也匹配；已有aborted要求token不存在、lock不是此operation的占用（他人的合法lock可以存在），不以历史receipt恢复能力。全读/Txn响应严格验证key/cardinality/header，未知结果unknown；不得将missing解释为target失败。

Renew mutex序列化同一cap，origin、原parent context与未lost/deadline检查，原context已取消时标记lost；读取/比较original guard+token Value/Lease/CRev、committedreceipt、五条original control上下文fences（mutation能力额外比较自己lock的Value/Lease/CRev，已入场data不因后来合法mutation lock出现而拒绝续期），一次no-write identity/restore Txn成功后只KeepAliveOnce原Lease。续期失败/unknown/原fences变更/回复晚于旧deadline使cap irrevocably lost；成功deadline从发送前monotone+serverTTL计算，并在收到时验证旧deadline仍未过期，绝不从响应到达时加TTL。不重读新control替换旧cap，不修改原证据或业务期限，不新Grant。业务expires_at已过但control没变的已入场cap允许在自身timeout/Lease内排空；本批Renew不把业务到期当外部已终止，也不签发新指令。

Cancel先在同一mutex标记lost，再bounded Revoke自己的原Lease（LeaseNotFound幂等成功）；上下文取消仍不可恢复cap，不复建、不逐key删除、无需改owner/control。可重试原Revoke，不能撤销另cap。此方法撤销准入能力，仅供未dispatch回收或显式取消，不叫End、不报告外部已终态；真正成功End还需后续可信target terminal证据与durable effect协议。token缺失后destroy仍须target closed gate+已接受任务实际排空。operationref是元数据归因，不可用于旧Stage或Renew/Cancel。

- [ ] `TestResolveOperationLateCompleteTransaction` 真实延迟整个admit，resolver先abort后恢复原Txn，token/lock不出现；相反真实commitreply丢失只恢复committed证据；Lease revoke/自然过期返回expired且原Tx永不能晚到重建；旧ref/restore/malformed/leasedreceipt/重建guard不能误授权。
- [ ] `TestRenewOperationOriginalLease`：多次renew原ID无Grant、sent时刻保守截止；真实失租、unknown reply、pause过deadline与同值重建guard/token不可复活；控制TTL变更、gate/boot/restore变化拒绝；原parent context取消后新background ctx不得恢复；已入场跨业务expires不修改expires。
- [ ] `TestCancelOperationCapability`：并发renew/cancel/race、重复取消、不存在Lease、cancel RPC未知，lost不可复活、仅原Lease受影响；明确不把metadata消失当target终止。
- [ ] `TestOperationActiveCostIndependentOfIdleRecords`：真实固定数量active operation、额外大量合成永久idle metadata不增Grant/KeepAlive/point数量；只作结构性常数路径验证，不声称真实大N性能/SLO。
- [ ] 定向race、自审即独立提交、spec→quality；controller fresh owned fault fixture与全仓test/vet/build，再whole-branchreview并保存容量/验证报告，不宣称Redis已移除。

## 自审与后续缺口

五项Review Focus分别由Task2unknown/fences与Task3resolver/renew/cancel测试覆盖；producer/consumer接口只跨私有bundle/cap，不混用旧permanentStage。取消与安全End明确不同；实际operation签发、dispatch effect journal及expiry token、launcher执行屏障、exclusive/destroy/task、pool slots、Docker/Upload、GC与容量backpressure仍须后续单元完成后才可接生产。根证书初始publication有效期不会被本批当成TTL扩展后持续管理授权；持续target授权需另定rotation/recertification契约。
