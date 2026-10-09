# Workspace Archive Recovery Storage Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox syntax for tracking.

**Goal:** 为原始workspace archive保存有额度的持久文件和完整归属，并在冷启动只读诊断重开。

**Architecture:** Linux opt-in固定槽store，pending/received/bound原子manifest；保留现有原始authority链。文件持久性先于首次local intent，冷读取不构造执行权限。

**Tech Stack:** Go 1.25.6、x/sys/unix、现有三成员etcd 3.6.15与Linux arm64 worker；无依赖变更。

**Spec:** `docs/superpowers/specs/2026-10-09-workspace-archive-recovery-storage-design.md`

## Global Constraints

- Recovery与legacy spool互斥，要求recycling budget；Linux支持，nil/legacy不改变；所有recovery相关options先复制再验证。
- Jobs=1..64、固定三文件/槽；manifest M=65536、TAR A=JobBytes−2M≥1536、每槽index/metadata内存预留I+4M。额度在IO前预扣，Close不unlink/不退还持久job/disk。
- codec固定wire/EscapeHTML(false)/bounded IO；最大合法schema逐字段上界和实际字节测试，禁止任意Marshal/ReadFile后检查。
- 无新etcd KV、50pins/64nodes/256KiB事务/64KiB record及checkpoint/GC不变；不造冷source/witness/claim或退休许可。
- store.mu/nativeMu不跨IO/join；metadata gate所有等待/IO均持原生命周期登记，Close实际等待。
- 源码、测试、文档、报告、格式修改只能apply_patch；禁止cat/heredoc/Python写这些文件、gofmt -w。允许工具生成原始日志/JSON/二进制/brief/diff。
- 禁止读取/修改/stage无关Sentinel文档、读取旧CLOSED OWN、重跑已验收单元或复用旧作者/审查员。禁止main/config/deploy/push/依赖升级。
- 精确路径小步提交：持OPEN OWN/git-commit.lock fcntl LOCK_EX，覆盖branch/expectedHEAD/空index/add/exactstaged/diffcheck/commit/空index。

## Review Focus

1. metadata编码扩张、并发staging和cold扫描是否在既定额度内。
2. 收到完整TAR与实际Session.Close、received seal、first intent之间的先后关系。
3. metadata IO发生在原receive ownerDone之后的登记、Close等待和cancel竞态。
4. `.next`/orphan/tamper不自动提升或退款；root flock不冒充writer fence。
5. nil/legacy继续可用；新profile cold诊断无可执行capability，目标binding不可跨文件替换。

---

### Task 1: Fixed persistent store and original receive/local binding

**Files:**
- Modify: `internal/storage/state/etcd/client.go`, `task_archive_delivery_options.go`, `task_archive_receive_types.go`, `task_archive_spool.go`, `task_archive_delivery.go`, `task_workspace_sync_intent_admission.go`。
- Create: `internal/storage/state/etcd/task_archive_recovery_{types,codec,store,linux,unsupported}.go`；types职责状态/诊断，codec职责bounded canonical wire，store职责quota/state/lifecycle，linux职责descriptor filesystem，unsupported拒绝profile。
- Test: new `internal/storage/state/etcd/task_archive_recovery_{codec,store,native_linux}_test.go`；可修改 `scripts/test-workspace-sync-local.sh` 只加入精确本单元selector，不重跑旧suite。

**Interfaces:**
- Consumes现有TaskClaim/archive draft/received/spool/source/localTarget原始链、`taskArchiveIndexBytes`、`taskWorkspaceSyncLocalAttribution.Validate`与checkpoint TargetDigest生成规范。
- Produces `TaskWorkspaceArchiveRecoveryOptions`（嵌入原spool options），私有 `taskArchiveRecoverySeed` / `taskArchiveRecoveryManifest` / `taskArchiveRecoveryPlanWire` / `taskArchiveRecoveryBinding` / fixed store/slots。
- `(*taskArchiveStore).reserveRecovery(ctx context.Context, seed taskArchiveRecoverySeed) (*taskArchiveSpool, error)`；新store constructor可私有接ownedoptions。
- `(*taskArchiveSpool).sealOriginalReceived(ctx context.Context, original *taskReceivedArchive) error`；`(*taskArchiveSpool).sealOriginalBinding(ctx context.Context, source *PreparedTaskWorkspaceSyncSource, body *taskWorkspaceSyncBody) error`。
- `coldSnapshot(slot uint32)`返回私有owned诊断copy；不得返回可读TAR/authority。具体私有diagnostic类型由作者固定并报告。

- [ ] **Step 1: RED与最终wire/额度表。** 添加有意义的旧行为失败测试（Close删除/非空root拒绝或first intent没有bound manifest），原始输出保留。实现前报告实际schema每字段上界≤53121/M65536及I+4M各同时存活对象；如不能满足立即提出具体修订，不能静默扩大。添加codec合法最大及拒绝、overflow/64full、真实FS/child锁测试。
- [ ] **Step 2: Store、codec、options。** 按spec实现固定槽、strict有界读写、Linux root/descriptor身份、pending初始化两次durable、charged quarantine冷扫描、retain Close/metadata串行登记。nillegacy保持。先跑新store/codec focused host race与Linux交叉编译，验证后提交 `feat(etcd): retain bounded workspace archives for cold diagnostics`，不等整单元完再提交。
- [ ] **Step 3: 原始接线。** recovery reserve前seed；receive成功实际Session.Close后received seal；registered source/body/target下first recycling reserve前bound及postguard；以后冻结binding。额外metadata登记覆盖delivery-layer seal。禁止在锁内做IO。focused测试验证后提交 `feat(etcd): persist original workspace attribution before local effects`。
- [ ] **Step 4: 故障与生命周期验证。** 真实owned subprocess在pending前后、artifact创建/身份、TAR fsync后、received rename/root fsync、bound前后和first reserve前窗口kill，每条路径实际Wait；冷重开只诊断charged，不自动选择next。实际Linux新profilearchive→两文件local、不同target拒绝、seal失败零reserve/目标effects、实际metadata/postguard Close等待。hostrace只跑新tests及必要compatibility，native只跑新selector；保留失败与后续结果。需要测试专用hook只能私有fixture、不提供产品authority入口。
- [ ] **Step 5: 最终核验与小提交。** 运行新unit完整hostrace、native新selector、affectedpackage vet/Linuxcross、gofmt -l、bash -n/diffcheck。每次raw JSON/实际fixture完整stdout stderr、本单元repo source快照SHA/testbinary compiler字段/实际child Wait和精确label0容器清理日志保留到OPEN OWN。已通过且源码未变不重跑。测试/selector单独连贯提交，不推送。
- [ ] **Step 6: 自查与报告。** 写OWN task-1-report.md（apply_patch）列commits、files、schema/staticbuffers quota表、真实RED/GREEN、各源epoch/uniqueleaf数和SKIP/FAIL、来源绑定/child Wait/清理、diagnostic而非recovery及tmpfs非断电边界。静态artifact SHA清单只含本单元稳定证据，排除自身/活动ledger/review；不要复制旧单元证据。Root另派一名fresh reviewer，同代码不重跑作者tests。

本plan是总迁移一个功能单元；完成后继续下一恢复单元，不触发整个branch完成、push/deploy或重复全分支review。
