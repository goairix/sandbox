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

- [x] 真实元数据与错误归属/配置的测试先行；Root确认真正 RED 后实现。
- [x] 构造前校验 release image/contract，复制配置并保持原 identity 公式。
- [x] 来源精确绑定、签名历史验证或明确复用已有认证入口，拒绝 local/FUSE/错绑定。
- [x] 定向 host/race/Linux vet；唯一 Task 审查通过；提交 `361003b`。未开放 HTTP/consumer。

## Task 2：严格 version2 detached 终止 wire

**Files:** `internal/runtime/controlprotocol/task_runtime_termination_authority.go`、
新的 `task_runtime_termination_detached_test.go`；必要新私有 validator 文件。

**Interfaces:** spec 中的 DetachedRuntimeEffectBindings 与 intent.DetachedEffects；
原 Encode/DecodeTaskRuntimeTerminationIntent 签名不变，ticket v1不变。

- [x] 新 v2 正向、缺 proof/混合 profile、旧 v1精确字节与新完整 digest ticket 测试先行。
- [x] Root真正 RED后实现严格variant，保持旧 canonical v1。
- [x] 全 controlprotocol host、相关race、Linux检查、唯一Task审查通过；提交 `9697527`。

Task1与Task2可在独立已有工作树并行；没有共享源码文件。两者共同的profile常量
以协议spec为准，来源分类不产生协议 settlement 或 signature authority。

## 后续接续门槛（不能跳过）

### Task 3 冻结合同：持久 no-effect 与原 Run 恢复

唯一共享纯wire类型及Encode/Decode/BindingDigest放在controlprotocol，供etcd与
独立observer复用；私有etcd adapter保留native及原job权限。原E以有界精确bytes/
digest保存，不复制全E schema、不预建导出matcher。历史WriterClaim、原none Claim
与后续termination Claim可不同；present永久fact不会因后续合法Run失效，absence
恢复则必须证明原真实Joined fence。共享codec本身不构成权限。

独立私有 settlement 键为 `p/<partition>/tasks/<TaskID>/detached-noeffect`，
lease0、create-once、严格 canonical codec。初始记录上限32KiB，必须用合法最大
输入实际编码确认；不能为通过预算而截断合法来源。记录保留原 Task/birth、
WriterClaim、Run/Worker、incarnation、AttemptID/BindingDigest、plain contract、
quiescence/原 none 及 source fences、原 E canonical+stamp/S stamp。
普通来源 DTO 和合法 codec 均不授予 native 权限。

原 job/Claim/Kernel/registry 指针、E/S 出生及 Run 认证后，使用真实 signed enrolled
reader、原 dual-drain revalidation 和 native source 点读。原 `none` reservation
不得为新 Claim 覆写或重新 Reserve；旧 final-sync Claim 与当前 WriterClaim 分开
保留。只有最终来源分类与全部 native 历史都通过才能提交。

新 Pending purpose=`detached-noeffect` 复用现有 AttemptID/Digest 标量，必须有
ClaimID/LeaseID，StageID/RequestID 为空；保持原结构可比较和旧 canonical。
先固定原 attempt/binding，经原 progress 得到持久 ACK，再固定 E.mod/wire。
BindingDigest 排除 Pending/E.mod 以避免循环，但最终 settlement digest 覆盖完整
原 E。draft 自绑定 Backend/job/Claim，复制不能获得提交权限。

写事务原 taskEmptyComparisons53 + E/S/finalSync/snapshot/intent/publication
六个真实 positiveMod pins + settlement absence =60比较；Then Put1、Else三点，
总64原生节点。不能复制旧 reservation 的65节点预读。source点读22节点，原
draft仲裁10节点；所有实际 protobuf 请求/回复仍须验证128KiB/256KiB上限。
writeAttempted 在 native 发送前不可逆，Unknown只能读仲裁原 draft、不重发；
absent 单独仍 Unknown，exact committed 在失权后仅返回诊断加 error。

cold recovery 只读认证的真实 E/S 与 Run.Joined=true、E.mod 严格晚于旧原 E，
才证明原 join CAS 已 fence delayed Put。无 settlement 时也不能靠 lease消失、
none或 caller Joined 值恢复。新私有 job-bound preflight 保存完整历史；原 actor
owner 消费后才能单次 CAS 新 Run、新 Claim、重新 dual-drain 和新 attempt。
现有 local archive wrapper 保留所有 profile/inventory 检查；若提取公共 CAS，
只提取严格 one-candidate E/S 操作，不扩大调用者权限。未接入实际 detached
inventory/actor 的私有恢复 gate 不计端到端恢复完成，接线仍属Task4。

文件按 records、writer、native reader、recovery 分职责；窄改现有 Pending/job
状态及必要共用 CAS。先测试真实signed来源、copied authority拒绝、Claim/Run/S
ABA、64节点/最大字节、Unknown/丢ACK/原实际join fence与cold恢复，再实现。
首次运行前必须再冻结 recovery 的实际比较和 native 字节投影，不能把估算当验收。

### Task 4A 并行边界：实际 termination 与 observer

Task3 的 writer/read 连贯单元已以 `58f38fd` 接受并提交：真实三成员六顶层及四个
子场景、独立有界五点组件、Linux构建/vet与限定审查通过。仅该组件单元关闭，
本节之前的冷新Run门禁、内存和真实signed最大输入门槛仍开放；见对应验证报告。

独立复用D工作树，与B的Task3并行；唯一实现者
`detached_runtime_native_implementation`，合同在其OWN/CONTRACT.md。
拥有controlprotocol原termination key helper、etcd `task_runtime_termination*`
及nodeobserver相关实现/测试；不改Task3 shared settlement文件、due worker、
owner-release、quota、terminal、部署及HTTP。后续4B/4C接后者，不得因此漏项。

保留原KeysFor严格v1包装，新显式DetachedTaskRuntimeTerminationKeysFor只派生
固定v2键；Snapshot必须先读严格nativeTask后推导，不能caller指定。完整variant
比较包括Version/DetachedEffects，不能只比较common Authority。新私有
loadDetachedRuntimeAuthority(ctx,j,c)核原registry job/Claim/Kernel/Run、真实
永久settlement及signed source/q/none，独立保存真实q context/current certificate，
不伪造archive DTO。共用原provider/termination状态机与retained Query/no-resend。

observer从persisted canonical intent分支，独立检查native settlement envelope/
stamp、Task birth、source/q/none及signed publication/activation plain contract，
原E/S完整语义由唯一严格create-once writer/native adapter保证，不复制schema。
继续核原domain/Claim/Guard/TTL/prefix/clock/journal/PID1/proof。请求128KiB、
回复256KiB必须分别验证，保留旧v1实际合法形状。所有静态节点候选仍待实际pb.Size。

先测试v2 fixed keys、foreign variant拒绝和128KiB请求上限，再实际native live
authority/observer与历史Query。Task3真实writer尚未可用时，不拼签名字段伪造正向
authority；同一独立Task审查在该子任务实际完成后进行，不能当自动consumer完成。

Task3 为原 dual-drain 后的持久 no-effect settlement 与原 Claim/Run历史认证，
Task4 为新终止/owner-release authority 与实际 actor/配额/终态 reader 接线，
Task5 为显式部署/HTTP及新真实 Linux组合。每项开始前冻结具体原生读写/RPC/预算、
恢复引用和精确接口到本计划，再测试先行、实现、验收、小步提交；不能用当前
Task1/2的单元通过把这些门槛记为完成。旧local与已验收quota底层只测必要回归，
不重放旧完整矩阵或重复创建已完成任务。
