# Detached plain cleanup 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. 每 Task 一个新实现者和一个唯一完整 Task reviewer。Root 负责 Go/Git/Docker/gofmt 与提交。

**Goal:** 实现 HTTP detached business 创建产物的实际自动销毁消费者。

**Architecture:** 从明确 detached/plain 来源生成独立 no-effect settlement，新增严格
终止 profile，复用原 DueWorker/Run/join/owner/quota/P 管线，最后接部署与 HTTP。

**Tech Stack:** Go1.25.6、etcd3.6.14、原 controlprotocol/launcher/observer。

**Spec:** `docs/superpowers/specs/2026-10-18-etcd-detached-cleanup-design.md`；继承已批准主方案。

## Global Constraints

256分区/4 Relist/20MiB/s Range/100 claim-s/32永久任务；64 native节点、128KiB请求、
256KiB回复、64MiB/10000keys worker预算。Unknown不重发、不提前释放；所有实际
native调用和原物理 goroutine 完成 join。源码 apply_patch；AGENTS注释/分段/包职责；
共用主仓库缓存。Sentinel及无关go.mod保持原状，不切默认入口、不push或部署。

## Review Focus

- snapshot v1、none计划、缺 archive 都不证明无远端 effect。
- 任意配置 digest 不证明 plain contract；必须从 pinned release image 计算。
- 旧 local v1 字节、比较及信任边界不变；新版禁止混合 profile。
- 丢 ACK/取消不产生新 attempt 或遗失原 Run，原请求 Query/receipt 必须可恢复。
- 重建 quota 身份不读取已删 ownership；大历史只使用原有限 reader/预算。

## Task 1：明确 detached/plain 来源与配置

**Files:** 新 `internal/storage/state/etcd/task_cleanup_detached_profile.go`、
`task_cleanup_detached_options.go` 及相应测试；修改 `client.go` 和
`domain_detached_identity.go`。若需复用签名 fixture，只允许窄构造参数提取，旧行为不变。

**Interfaces:** Options.DetachedCleanup *DetachedCleanupOptions；
DetachedCleanupOptions.PinnedPlainImage string；Backend 私有不可变配置。
`detachedCleanupSource(task TaskRecord, snapshot SandboxSnapshotRecord,
intent CreationIntentRecord, publication *RuntimeEnrolledPublicationEntry)
(WorkspaceIdentity,error)` 为私有来源分类，普通返回值不授予 effect 权限。
实现者须在测试前用≤20行冻结必要历史 verifier 调用及精确参数绑定。

- [ ] 真实元数据与错误归属/配置的测试先行；Root确认真正 RED 后实现。
- [ ] 构造前校验 release image/contract，复制配置并保持原 identity 公式。
- [ ] 来源精确绑定、签名历史验证或明确复用已有认证入口，拒绝 local/FUSE/错绑定。
- [ ] 定向 host/race/Linux vet；唯一 Task 审查；连贯提交。不得开放 HTTP/consumer。

## Task 2：严格 version2 detached 终止 wire

**Files:** `internal/runtime/controlprotocol/task_runtime_termination_authority.go`、
新的 `task_runtime_termination_detached_test.go`；必要新私有 validator 文件。

**Interfaces:** spec 中的 DetachedRuntimeEffectBindings 与 intent.DetachedEffects；
原 Encode/DecodeTaskRuntimeTerminationIntent 签名不变，ticket v1不变。

- [ ] 新 v2 正向、缺 proof/混合 profile、旧 v1精确字节与新完整 digest ticket 测试先行。
- [ ] Root真正 RED后实现严格variant，保持旧 canonical v1。
- [ ] 全 controlprotocol host、相关race、Linux检查、唯一Task审查及小步提交。

Task1与Task2可在独立已有工作树并行；没有共享源码文件。两者共同的profile常量
以协议spec为准，来源分类不产生协议 settlement 或 signature authority。

## 后续接续门槛（不能跳过）

Task3 为原 dual-drain 后的持久 no-effect settlement 与原 Claim/Run历史认证，
Task4 为新终止/owner-release authority 与实际 actor/配额/终态 reader 接线，
Task5 为显式部署/HTTP及新真实 Linux组合。每项开始前冻结具体原生读写/RPC/预算、
恢复引用和精确接口到本计划，再测试先行、实现、验收、小步提交；不能用当前
Task1/2的单元通过把这些门槛记为完成。旧local与已验收quota底层只测必要回归，
不重放旧完整矩阵或重复创建已完成任务。
