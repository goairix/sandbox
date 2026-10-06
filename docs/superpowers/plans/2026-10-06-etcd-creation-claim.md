# etcd 创建 intent 的原始租约能力实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [x]`) syntax for tracking.

**Goal:** 为已持久化的 pending 创建 intent 提供可恢复管理权，失租的旧能力不能再提交领域事务。

**Architecture:** 每个实际执行中的创建任务使用一个独立短 Lease，claim 与不可复用 guard 同事务附着该原始 Lease。能力绑定永久申请记录及原始 revision；永久 owner/control 不随租约消失。此单独交付仅建立管理权和后续元数据事务的比较条件；runtime dispatch journal、exact runtime Publish 和 launcher gate 在下一批实现，取得 claim 本身不准许重复外部创建。

**Tech Stack:** 已固定的 Go1.25、client/v3 3.6.14、etcd3.6.15 三成员真实 fixture；不增加依赖。

**Spec:** `docs/superpowers/specs/2026-10-06-etcd-state-management-design.md` 创建、task 能力和外部 fencing 章节。

## 全局约束

- branch `codex/etcd-state-management`，承接领域模型和 AcquireIntent；既有 Sentinel 计划修改不暂存、不覆盖。
- task 能力包含不可复用 claim ID、claim CreateRevision、Lease ID、restore_epoch 和 live guard。claim 与 guard 都附原 task Lease；后续事务比较两者精确 value、LeaseValue、CreateRevision 及 owner/control revision。
- 失租后不能重建同 guard 或缓存 capability 继续提交。新 claim 使用新 UUID 和新 Lease；restore epoch 隔离快照恢复后 revision 回退。
- 永久 owner/fence/control/request/intent/placement 不附 Lease，不释放、不删除；request=pending 与 AcquireStage=committed 不证明 runtime 已创建。
- 只查固定 point keys，不创建后台 goroutine 或每沙盒常驻租约。claim 数量按实际进行中的创建任务 C 计量，不能按全部存活 N 计量。
- API 取消不删除持久 intent；unknown claim 创建不返回能力，best-effort 撤销原 Lease，不能重新附着、收养或续接该 attempt。
- Reference 是归因信息，不是可重新构造的 capability；新 Backend 只能新建 claim，不能使用旧 Backend 的能力。

## Review Focus

1. Lease 失效后 claim 同 key 被重建，旧能力必须因原 value/Lease/CreateRevision 三重比较失败。
2. claim 创建实际成功但回复丢失，不得返回能力，也不得让恢复者删除永久 owner。
3. schema 合法但关系错位的 intent/request/owner/control/placement 必须拒绝领取。
4. Renew 的 RPC 超时或假阳性不能证明管理权仍有效，所有真正 mutation 仍重验原 capability。
5. 旧 control revision 或 restore epoch 的迟到事务必须失败，不能以续 Lease 绕过领域 CAS。

## Task 1：永久 intent 读取与创建 claim

**Files:** Create `internal/storage/state/etcd/creation_claim.go`、`creation_claim_records.go`、`creation_claim_read.go` 及 `creation_claim_test.go`。已有文件只在确实需要共享有界 response 校验时作最小重构；不改生产 Manager。

**Consumes:** `readDomain(ctx, keys...string)`、严格永久 record codec、`validateWorkspaceBinding`、固定 namespace key、`BeginStage`/`CommitStage`/`ReleaseStage`、`revokeStageLease`。

**Produces:**

```go
type CreationClaim struct { /* private original Backend, Lease, value, revisions; mutex-protected deadline/lost */ }
type CreationClaimReference struct {
    ClaimID, WorkerID, IntentID, SandboxID, WorkspaceHash, RestoreEpoch string
    Partition uint8
    Generation, DataGateEpoch, CreateRevision, LeaseID int64
}
func (c *CreationClaim) Reference() CreationClaimReference
func (b *Backend) LoadCreationIntent(ctx context.Context, w WorkspaceIdentity, intentID string) (*CreationIntentRecord,error)
func (b *Backend) ClaimCreation(ctx context.Context, w WorkspaceIdentity, intentID, workerID string, ttl time.Duration) (*CreationClaim,error)
func (b *Backend) RenewCreationClaim(ctx context.Context,c *CreationClaim) error
func (b *Backend) ReleaseCreationClaim(ctx context.Context,c *CreationClaim) error
func (b *Backend) creationClaimComparisons(c *CreationClaim) ([]clientv3.Cmp,error) // private, copied comparisons
```

`LoadCreationIntent` 同一线性 Txn 比较 base metadata、读 exact intent key；校验永久 Lease0、schema、intent ID、workspace hash/partition、当前 restore。missing 返回 nil；不推断外部状态。

`ClaimCreation` ctx 非 nil，w 绑定当前 storage，intent/worker 为合法 ASCII segment≤128，TTL 正数≤24h、向上取整到服务端秒数。在 Grant 之前完成所有输入、路径、记录与预算检查。先点读 intent 定位 sandbox/request，再一个线性 Txn 同时读 intent、owner、fence、control、request、placement、现有 claim 共 7 个 key，最终领取事务再次 CAS 全部永久记录。记录必须存在且 pending intent/pending request/publishing control；intent ID、sandbox ID、workspace hash、generation、request hash、configuration digest、restore 互相一致；owner 未绑定 runtime 且 mount0，control 同样未绑定，control/placement 关系必须正确。各 KV CreateRevision>0、ModRevision≥CreateRevision、永久 Lease0。记录变化不准许使用旧值领取。

合法已发布图（published intent、completed request、control 与 owner 的 exact runtime/mount 一致）仅不再适合领取 creation claim，关系核验后返回 `ErrConflict` 且不 Grant；active、workspace_exclusive、destroying、cleanup_pending 均验证。pending/pending 同归属图进入 destroying/cleanup_pending 后也不能再领取 creation claim，返回 `ErrConflict`，交后续 cleanup 协议恢复。不误报存储损坏；字段错位、缺损或矛盾的 phase/runtime 仍返回 `ErrCorruptRecord`，加入真实回归。

固定 claim key：`p/xx/intents/intentID/claim`。每次 guard key：`p/xx/intents/intentID/guards/newUUID`。私有 schema1 claim record≤4096 bytes，明确保存上述归属字段及原 Lease ID；guard 与 claim 使用同一 immutable value，CreateRevision 相同。现有 claim 须严格 JSON/UTF8/字段/精确 key 校验，KV.Lease 非零且与 value 的 LeaseID 一致、归属和当前永久记录匹配；有效已占用返回 ErrConflict；缺损/错位/无租约不能抢占。不存在则 CAS CreateRevision0。

Grant 和 Txn 均使用独立的 bounded request context；校验 grant nonnil/header cluster/非零 ID/TTL>0，无能力时仅撤销已知本集群原 Lease。领取 Txn 比较 base、6 个永久记录的 ModRevision+raw value+Lease0、claim absence、unique guard absence，同时 put 两个 leased key。必须收到成功的本 cluster response 才返回 capability，其 epoch=该 Txn header.Revision。失败或 unknown 都返回 nil；撤销错误可 errors.Join，但不删除领域记录。失败 metadata branch 必须按 exact key、cardinality、nil-safe 校验 identity/restore。

`creationClaimComparisons` 拒绝 nil/跨 Backend/本地失效能力，返回私有固定 guard 与 claim 的 value/Lease/CreateRevision 比较，加领取时所有永久 record 的 ModRevision/value/Lease0 比较，深拷贝，caller 修改不能污染 capability。它不是外部操作授权；后续 dispatch/Publish 事务仍需要对应 durable effect intent 和 target evidence。

Grant 和每次 KeepAlive 以请求发送前本地单调 `time.Now()` 加服务端 TTL 计算保守 deadline，不以回复到达时间加 TTL；Grant 回复或领取 Txn 回复处理时已经越界，不返回能力且仅清理原 Lease。能力的 immutable 归属字段与 mutex 保护的 deadline/lost 分开；本地 deadline 越界或 Renew RPC/验证失败后标记 lost，后续同能力不可重新续租、不可构造新比较；新 claim 必须独立领取，不会复活旧能力。

本地 lost 不能追溯修改已经复制到 Stage 的 comparisons，也不能证明已经发送的事务 aborted；这些 Stage 仍由原服务器 guard/claim CAS、已确认的原 Lease revoke/expiry 与 exact receipt 仲裁决定。测试与报告必须保留这一界限，不以本地标志替代原 Lease 永久失效证明。

`RenewCreationClaim` 首先检查本地截止与 lost，以 bounded linear Txn 比较 base+全部 capability 条件，失败分类 identity mismatch 或 ErrGuardExpired；成功再 KeepAliveOnce 原 Lease，并验证 nonnil/header cluster/ID/TTL>0、发送前时间推导 deadline 未越界。并发 renew 串行且等待 mutex 的时间计入调用 context；取锁后先检查 ctx.Err。不写 claim、不刷新 generation/control revision、不重新 Grant。续租与随后 mutation 之间仍可能失租，不能省略 mutation CAS。`ReleaseCreationClaim` 标记本地能力失效并只 Revoke 原 Lease，LeaseNotFound 幂等；即使本地已 lost 也允许清理原 Lease，可在 caller 已取消时由上层用独立 background context 重试。不得删除 owner/control/intent/request 或其他人的新 Lease。

- [x] 写 `TestCreationClaimPermanentIntentLookup`、`TestCreationClaimAtomicOriginalLease`，真实三成员先观察 RED（不是仅编译失败），永久图 ModRevision/Lease0 不变，两 leased key 同 CreateRevision 与 Lease。
- [x] 写 `TestCreationClaimConcurrentOneWinner`（16 contenders）、`TestCreationClaimRenewAndRelease`、`TestCreationClaimReacquireRejectsOldCapability`；读取 claim 的 Lease 与 value，续租不改 revision，旧能力不能提交带 receipt 的真实事务，新能力可以。
- [x] 写 unknown grant/实际领取丢回复/迟到领取、restore 变更、wrong key/nil envelope、永久关系缺损、leased/corrupt domain、非法 TTL/ID/w/ctx、其他 Backend capability 的回归；input 错误与损坏记录不 Grant，unknown 不返回 capability。
- [x] 写 `TestCreationClaimDelayedRenewCannotRevive`：取得真实 KeepAlive 正响应后延迟交付，原 Lease/guard 在交付前撤销或截止越界；不会以到达时间加 TTL 继续使用同 capability，比较拒绝/真实旧事务失败。覆盖 Renew unknown 后同能力不能再次 KeepAlive，以及并发 renew/race。
- [x] 写 `TestCreationClaimControlChangeBlocksDelayedStage`、`TestCreationClaimGuardRecreateBlocksOldStage`：先建包含 private comparisons 的 Stage，再改 control 或删原 guard 并同 value/Lease 重建；旧 CommitStage 不能写测试 checkpoint，ResolveStage 只能 aborted。新 claimant epoch 大于旧 epoch。
- [x] 写 `TestCreationClaimStageBudget`：private comparisons 固定 24 条（两 leased key 共6、六永久 key 共18）；沿用 stageProtocolOperations=14，剩余业务 comparisons+writes 上限26。测试总计64允许、65拒绝且拒绝前不 Grant；不降低已有 txn 预算。
- [x] 实现上述 API；真实定向 race GREEN 与 spec/quality review，必要问题修复并复审。
- [x] fresh `bash scripts/test-etcd-state.sh -v`、`go test ./...`、`go vet ./internal/storage/state/etcd`、sandbox 构建、gofmt/diff checks，通过后保存报告并独立提交 `feat: add original-lease creation intent claims`。

## 下一交付与界限

建立 claim 后再做 durable runtime dispatch/checkpoint 和 Publish：绑定 exact runtime UID/BootID、不可重复 mount attempt、target gate 回执、runtime index、request completed 与 Publish receipt 同事务。未知外部 create/mount 先查可归因 target receipt，不重新执行。后续 due/dirty/task 索引需要与 control 变化同事务，不能因本批 point read 而宣称 scheduler 已避免全量扫描。运行后端仍由现有生产 wiring 决定；本批不开放 etcd allocator 或删除 Redis。
