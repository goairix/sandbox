# Kubernetes FUSE 清理崩溃恢复设计

## 背景与根因

Kubernetes FUSE Pool 的 Redis 记录在进入 `cleanup` 后，只有
`RuntimeID/RuntimeUID`、cleanup lease 和 revision，没有记录 runtime 清理已经走到哪一步。
`RemovePreparedSandbox` 则把 `TerminationEvidence` 和策略删除状态只保存在
sandbox-api 进程内存中。

当前清理顺序是：通知 workspace-mounter 卸载、删除 exact Pod、等待 Pod NotFound、
在内存中生成终止证据、删除策略，最后删除 Redis cleanup 记录。如果进程在 Pod 消失后、
Redis cleanup 记录删除前退出，新进程只能观察到 Pod NotFound，却没有可信的终止证据。
为了避免仍在分区节点运行的 Pod 被误判为终止，现有代码会返回
`runtime termination is unconfirmed`。这是正确的 fail-closed 判断，但由于没有持久化恢复点，
同一个 cleanup lease 会被永久重复领取并失败。

本问题不是单纯的脏数据问题。直接删除 Redis tombstone、把 NotFound 当作成功，或只增加
重试次数，都不能构成安全的长期修复。

## 目标

- sandbox-api 在终止流程的任意边界崩溃或重启后，都能从持久状态继续清理。
- 继续使用 runtime ID 与不可变 Pod UID 的双重身份，不接管同名替代 Pod。
- Pod NotFound 且没有持久化证据时继续 fail-closed。
- 多副本并发、cleanup lease 过期接管和 Redis CAS 冲突不能绕过证据校验。
- Helm 在清理协议变化时执行一次 release-wide drain，避免不同协议的控制器共同管理
  同一批 FUSE Pool runtime。
- Docker runtime 保持现有基于不可变 container ID 的清理路径，不被 Kubernetes 专用协议
  回归影响。
- 错误日志能够直接定位 runtime identity、cleanup phase 和 record revision。

## 非目标

- 不把 Kubernetes API 对象删除等同于进程终止。
- 不通过缩短 lease、强制删除 Pod 或忽略残留 NetworkPolicy 来解除阻塞。
- 不自动清除升级前已经处于“Pod 缺失且无可信证据”的历史 tombstone；该状态无法仅靠
  Kubernetes API 安全重建，仍需要 infrastructure fencer 或经过审计的运维修复。
- 不改变 workspace owner 的 fail-closed 释放规则。

## 方案比较

### 方案 A：Pod finalizer + Redis 终止检查点（采用）

在 FUSE Pod 上使用受管 finalizer 保留 exact Pod 对象，等待 kubelet 报告全部容器终止后，
把终止证据以 CAS 写入 cleanup 记录；随后才移除 finalizer、等待对象消失并删除策略。
该方案可以在每个边界恢复，且不降低安全性。代价是需要扩展 runtime/repository 接口、Redis
状态机和 Helm 清理协议。

### 方案 B：Pod NotFound 且无引用时视为终止（拒绝）

实现改动最小，也能清掉当前 tombstone，但控制面 NotFound 不能证明分区节点上的进程已经
退出。该方案违反现有终止证据契约，也会重新引入已经回滚过的不安全行为。

### 方案 C：始终依赖 infrastructure fencer 或人工清理（不采用）

安全性足够，但当前生产配置没有 fencer，普通进程重启就可能永久阻塞 Pool 和 Helm。fencer
保留为节点不可达等异常场景的后备能力，不作为正常清理协议。

## 持久化状态

为 `FUSEPoolRecord` 增加 Kubernetes 清理恢复所需的可选字段：

- `CleanupPhase`：空值/`terminating`/`terminated`。历史记录空值按 `terminating` 处理。
- `TerminationEvidence`：仅在 `terminated` 阶段存在，保存 RuntimeUID、NodeName、
  GracefulUnmount、ProcessExited、InfrastructureFenced。

记录校验必须保证：

- 这些字段只允许出现在 `state=cleanup` 的记录上；
- `terminated` 必须携带有效证据，证据 RuntimeUID 必须等于记录 RuntimeUID；
- 有效证据必须满足 `ProcessExited=true`，或
  `InfrastructureFenced=true` 且 NodeName 非空；
- `terminating` 不得携带终止证据；
- cleanup lease 被其他副本接管时保留 phase 和 evidence，不得降级或清空。

Repository 增加一个专用 CAS 操作，把当前 cleanup 记录从 `terminating` 推进到
`terminated`。操作必须校验 preparation ID、cleanup token、expected revision、runtime
identity 和证据，并在同一个 Lua 脚本中更新记录、revision 和更新时间；Pool membership、
计数、RuntimeUID owner 与 reservation deadline 索引保持不变。
重复提交同一份证据可以幂等返回当前记录；不同证据或旧 revision 返回冲突。物理删除仍然
只能通过 `DeleteCleanup` 完成。

## Runtime 清理协议

增加可选的两阶段 exact-runtime 清理能力，由 Kubernetes runtime 实现：

1. `ConfirmPreparedSandboxTermination`：确保 finalizer 存在，发起并确认 runtime 终止，
   返回可信 `TerminationEvidence`，但不让 Pod 对象消失。
2. `FinalizePreparedSandboxRemoval`：接收已经持久化的证据，重新验证 exact identity，
   移除 finalizer、等待 exact Pod 消失，再删除该 UID 绑定的 NetworkPolicy 与
   CiliumNetworkPolicy。

FUSE Pool 检测到该能力后按如下顺序执行：

1. 领取或接管 cleanup lease；
2. 若 phase 不是 `terminated`，调用 termination 阶段；
3. 通过 repository CAS 持久化证据，并使用返回的新 revision；
4. 调用 finalize 阶段；
5. 使用最新 token/revision 调用 `DeleteCleanup`。

所有已领取 cleanup 的入口，包括普通 reconcile、release drain、publication 补偿和
Manager 的单次 runtime teardown，都必须汇聚到同一个两阶段 helper，不能继续直接调用
旧 remover。`RemoveClaimedRuntime` 相应返回推进后的 record（或等价地原地更新调用方保存的
record），使后续 owner release 与 `CompleteClaimedCleanup` 使用新的 evidence 和 revision。
Manager 优先使用该 record 中的持久化 evidence 释放 workspace owner；只有不支持两阶段
协议的 runtime 才继续调用 `RuntimeFencer.ConfirmTerminated`。进程重启恢复 ephemeral
lifecycle 时也从 cleanup record 恢复 evidence，不能依赖旧进程的 runtime cache。

不实现两阶段能力的 runtime 继续使用现有 `RemovePreparedSandbox` 路径。因此 Docker 不需要
伪造 Kubernetes Pod 证据，也不会改变现有行为。

## Kubernetes finalizer 与终止确认

FUSE Pod 使用固定 finalizer，例如
`sandbox.huaxisy.com/fuse-runtime-cleanup`。新 Pod 在 Create 请求中直接携带 finalizer；
领取、健康检查和删除路径把它作为受管 runtime 身份的一部分校验。

对升级前已经存在且尚未删除的合法 FUSE Pod，termination 阶段先使用 UID 与
resourceVersion 约束补加 finalizer，再发起删除。已经具有 `deletionTimestamp` 但没有
该 finalizer 的 Pod 不能再安全纳入协议，必须 fail-closed 或交给 fencer。

termination 阶段执行：

1. GET Pod 并验证名称、UID、managed/pool/FUSE 身份；
2. 确保 finalizer 已绑定到该 exact Pod；
3. 若 Pod 尚未终止，向 workspace-mounter 发送带 RuntimeUID 与 generation 的 shutdown
   请求；
4. 校验响应中的 RuntimeUID、generation 和 graceful-unmount；
5. 使用 UID precondition 删除 Pod，使其进入 terminating；若重试时容器已经全部终止但
   Pod 尚无 deletionTimestamp，仍先提交 exact UID 删除；
6. 观察同一 UID，直到 kubelet status 完整覆盖 Pod spec 中的 init containers（包括
   native sidecar）、普通 containers 和已有 ephemeral containers，且每个 state 都是
   terminated；缺失 status 不能当作终止；
7. 返回 `ProcessExited=true` 的证据。有效 shutdown ack 同时记录
   `GracefulUnmount=true`。

如果进程在删除请求后重启，Pod 因 finalizer 仍然存在。新进程可以从 container statuses
重建 `ProcessExited=true`，无需依赖旧进程内存。若节点不可达、状态未知或 Pod 被同名替换，
保持 `runtime termination is unconfirmed`。配置了 infrastructure fencer 时，仍可产生绑定
同一 RuntimeUID/NodeName 的 fenced evidence。

finalize 阶段只接受与 cleanup 记录完全匹配的持久化证据：

- exact Pod 仍存在时，验证 UID 后以 resourceVersion 冲突重试方式仅移除本系统 finalizer；
- Pod 已经 NotFound 时，只有已持久化证据才允许继续；
- 等待 exact UID NotFound 或确认名称已被其他 UID 替代，绝不删除替代 Pod；
- Pod 终止后再删除 exact UID 绑定的两类 FUSE 策略；
- 任一步失败都保留 Redis `terminated` 记录，后续副本可幂等继续。

普通 Pod 和非 FUSE Pool Pod 不添加该 finalizer。

启动孤儿回收没有对应 Redis cleanup record，因此仍可用 runtime 内部的
“confirm 后立即 finalize”包装方法：finalizer 在 confirm 前保护 Pod，若进程在 finalizer
移除前退出，下次可从 Pod status 继续；若在 finalizer 移除后退出，现有 orphan-policy 扫描
可按 UID 绑定清理残留策略。该路径不得用于仍有 Redis Pool/owner 引用的 runtime。

## 崩溃恢复矩阵

| 崩溃点 | 持久状态 | 重启后的行为 |
|---|---|---|
| 补加 finalizer 前 | Pod 运行，phase=terminating | 重新验证并补加 finalizer |
| shutdown ack 后、删除前 | Pod 运行且有 finalizer | 重发幂等 shutdown，随后删除 |
| 删除请求后、容器终止前 | Pod terminating 且有 finalizer | 等待 kubelet 状态或 fencer |
| 容器终止后、Redis CAS 前 | 终止 Pod 仍由 finalizer 保留 | 从 status 重建证据并 CAS |
| Redis CAS 后、移除 finalizer 前 | phase=terminated，Pod 仍存在 | 校验证据并移除 finalizer |
| 移除 finalizer 后、策略删除前 | phase=terminated，Pod 可 NotFound | 凭持久化证据继续策略清理 |
| 策略删除后、tombstone 删除前 | phase=terminated | 幂等确认资源消失并 DeleteCleanup |

## Helm 协议升级

Deployment Pod template 增加独立 annotation：
`sandbox.huaxisy.com/cleanup-protocol: v2`。它描述 FUSE runtime cleanup/Redis schema
兼容性，不复用现有 `drain-protocol`，后者继续表示 drain CLI 的调用兼容性。

pre-upgrade hook 同时比较已安装和目标 backend fingerprint、cleanup protocol：

- 两者都相同才跳过 release drain；
- backend 相同但 cleanup protocol 不同时，使用目标 sandbox-api 镜像执行完整 drain；
- backend 变化时继续执行现有 installed drain-protocol guard，然后完整 drain；
- drain 成功后由既有 post-upgrade resume Job 恢复副本；失败时保持 Deployment 为零，避免
  新旧协议并行写同一批记录。

目标镜像中的新 drain 逻辑可以先为仍在运行的 v1 FUSE Pod 补加 finalizer，因此 v1 到 v2
不需要把 NotFound 降级为成功。若升级前已经存在无 Pod、无证据的历史 tombstone，hook
明确失败并报告 exact runtime，要求 fencer 或审计修复。

只修改镜像 tag 而未修改 cleanup protocol 时，现有 backend 相同的快速路径保持不变。

## 并发、错误与可观测性

- termination evidence CAS 受 cleanup token 和 revision 双重保护；失去 lease 的副本不得
  finalize。
- finalize 前重新读取记录或使用 CAS 返回的新记录，禁止使用旧 revision 删除 tombstone。
- finalizer patch、Pod delete、Pod watch 和策略删除全部验证 exact UID；同名替代对象只作为
  旧 UID 已离开 API 的观察结果，不能成为删除目标。
- context timeout、watch 重连和 Kubernetes conflict 都返回可重试错误，不清除证据。
- Pool reconcile 错误和关键阶段日志增加 `preparation_id`、`runtime_id`、`runtime_uid`、
  `cleanup_phase`、`revision`；日志不包含 workspace credential 或 control payload。
- 增加各阶段计数/时延指标，并区分等待 kubelet、等待 fencer、finalizer、policy cleanup 和
  Redis CAS 冲突。

## 测试

### Repository 与状态机

- cleanup phase/evidence 编解码和非法组合校验；历史空字段兼容。
- termination evidence CAS 的成功、幂等、旧 revision、错误 token、错误 UID、lease 接管。
- `terminated` 不能回退到 `terminating`，`DeleteCleanup` 必须使用推进后的 revision。
- Redis Lua 测试验证索引、deadline、UID owner 和 pool count 不漂移。

### Kubernetes runtime

- 新 FUSE Pod 创建时携带 finalizer，普通 Pod 不携带。
- 合法 legacy Pod 可在删除前补加 finalizer。
- terminating 且无 finalizer、错误 UID、同名替代 Pod、未知 container status 均 fail-closed。
- shutdown ack 校验、UID-precondition delete、全部 container terminated 的证据重建。
- 持久化证据存在时可从 Pod NotFound 继续 finalize；无证据 NotFound 仍返回
  `ErrTerminationUnconfirmed`。
- finalizer patch 冲突、watch 重连、策略部分失败均可幂等重试。
- infrastructure fencer evidence 仍绑定 exact UID 与 NodeName。

### FUSE Pool 与崩溃注入

- 覆盖恢复矩阵中的每一个崩溃点。
- 两个副本竞争同一 cleanup、lease 过期接管、旧 token/revision 不得推进或删除。
- finalize 成功而 `DeleteCleanup` 失败时，下次不重新要求已经消失的 Pod 证明。
- Docker 和不支持两阶段接口的 fake runtime 继续走原有单阶段路径。

### Helm 与回归

- backend 与 cleanup protocol 都相同才 skip。
- 同 backend、protocol v1 到 v2 会 drain；backend 变化仍要求 installed drain protocol。
- Deployment、hook 参数和测试 fixture 均渲染目标 cleanup protocol。
- 运行 Kubernetes runtime、FUSE Pool、Redis repository、Manager、Helm render 定向测试，
  再运行 race 和全仓测试。

## 部署与运维影响

实现发布仍只需要构建包含二进制和 Helm Chart 期望 tag 的 `sandbox-api` 镜像；不需要构建
sandbox 或 workspace-mounter 镜像。Chart 模板必须随代码一起升级，使 cleanup protocol
annotation 和 hook 判断生效。

首次升级到 v2 会执行一次完整 drain，所以必须预留足够的 hook timeout。正常终止期间 Pod
可能短暂显示 `Terminating`，这是 finalizer 保留证据载体的预期状态。节点永久不可达时 Pod
会保持终止中并阻止 owner/cleanup record 释放，运维应使用配置的 infrastructure fencer；
不得直接移除 finalizer 后再删除 Redis 记录。
