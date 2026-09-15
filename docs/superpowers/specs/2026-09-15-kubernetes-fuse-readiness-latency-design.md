# Kubernetes FUSE 热池创建延迟优化设计

## 背景与实测根因

Kubernetes FUSE 热池 Pod 在待领取阶段已经启动 sandbox 容器和 native sidecar
`workspace-mounter`，但 mounter 处于 `prepared`，所以 `health ready` 有意失败，Pod 的
`Ready` condition 保持为 false。领取后 API 授权 mounter 挂载工作区，mounter 完成真实
挂载并转为 `ready`。

当前 `WaitSandboxReady` 先等待 Kubernetes `PodReady=True`，再读取相同 mounter 的 ready
状态，最后从 sandbox 容器执行精确 RuntimeUID/Generation 绑定的 `write-read-delete` 传播
探测。mounter readinessProbe 的周期是 10 秒，因此挂载若刚好错过 kubelet 上一轮探测，
API 会在真实挂载完成后额外等待接近 10 秒。

在 `ds-ai-research` 集群的一次同请求对照中：mounter 直接 `health ready` 在 1.261 秒成功，
PodReady 在 9.849 秒出现，API 在 10.191 秒返回。该请求约 8.6 秒消耗在 kubelet 下一轮
readiness 探测及 condition 传播，而不是后端挂载本身。

## 目标与边界

- FUSE 热池命中后不再被 kubelet 的 10 秒 readiness 周期随机阻塞。
- 只有真实挂载、精确身份、容器稳定性和 sandbox 侧读写传播全部通过后才交付 sandbox。
- 保留 Pod readinessProbe，继续用于 Kubernetes 运维状态、故障展示和其它控制器观测。
- 保持现有总就绪超时、context 取消、精确 Pod UID、租约代次和 fail-closed 语义。
- 不增加 Helm values，不改变 API 协议、Redis 状态结构、FUSE Pool 身份或网络策略。
- 第一阶段以热池连续创建不少于 30 次、零创建错误、创建 p95 不高于 3 秒为性能目标；
  冷池创建、存储异常和网络故障单独统计，不混入热池目标。

## 方案比较

### 方案 A：主动轮询真实 mounter 就绪状态（采用）

`WaitSandboxReady` 在同一个有界超时内反复读取 exact Pod 并验证身份、删除状态、mounter
重启次数和容器运行状态，然后调用 mounter `health ready`。状态尚未 ready 或控制命令发生
暂时错误时继续有界轮询；一旦返回完整 ready 状态，严格校验 RuntimeUID、PoolKey、
Generation、mount type 和 cache contract，再执行现有传播探测。

该方案直接等待业务真正依赖的条件，不增加所有池 Pod 的持续探测负载。它也保留 API 对
真实挂载和 sandbox 可见性的双重证明，是性能、安全和可用性的最佳平衡。

### 方案 B：把 readinessProbe 周期缩短到 1 秒（不采用）

改动简单，但每个尚未领取的 FUSE Pool Pod 都会持续执行较重的 `health ready`，增加 kubelet
exec、进程、挂载检查和节点负载。它仍然通过间接 condition 同步，无法消除状态传播延迟。

### 方案 C：增加 Kubernetes readinessGate condition（不采用）

由 API 或 mounter patch 自定义 Pod condition，Kubernetes 语义更统一，但需要新增 RBAC、
condition 所有权、冲突重试和恢复协议。最终仍需读取 API Server，复杂度和控制面依赖超过
本问题所需。

## 详细流程

1. 复用现有 workspace operation 锁，确认本地授权代次等于请求代次。
2. 建立一个 `readyTimeout` 约束的上下文，整个主动等待共享该时间预算。
3. 每轮 GET exact Pod，验证名称与不可变 UID、FUSE Pool 身份标签、prepared 状态及 bootstrap
   contract。
4. 验证 Pod 未进入删除、mounter 未重启且仍在运行、sandbox 容器仍在运行。身份变化、删除、
   重启或容器终止立即失败，不等待到超时。
5. 执行 `workspace-mounter health ready`：
   - 返回完整 ready 状态时，先验证所有状态字段；匹配后进入传播探测；
   - 返回合法但 state 尚非 ready 的状态时继续轮询；
   - exec、解码或临时控制面错误在 exact Pod 仍健康时允许重试，但只在总超时内重试；
   - RuntimeUID 不一致等不可变身份错误立即失败。
6. 从 sandbox 容器执行现有 `write-read-delete` 探测，校验 RuntimeUID、Generation 和空 token。
7. 使用最后一次通过身份与容器校验的 exact Pod 构造返回值，不依赖其 PodReady condition。

网络准备仍发生在 `WaitSandboxReady` 之前，不改变现有网络策略事务与可用性边界。

## 错误处理与可观测性

- 主动等待复用 `readyTimeout`，禁止在每轮 exec 上重新获得完整超时预算。
- context 取消或超时返回包含 `wait for workspace mounter ready` 的有界错误，不泄漏凭据、
  prefix、PoolKey、RuntimeUID 或控制响应。
- exact Pod 身份变化、删除、mounter 重启、容器退出和 ready 状态字段不匹配继续 fail-closed。
- 保留 `mounter_status` 与 `propagation_probe` 阶段指标；原 `pod_ready` 阶段改为真实 mounter
  等待指标，或新增明确的 `mounter_ready_wait` 阶段，避免仪表盘继续误解为 Pod condition。
- Kubernetes readinessProbe 及其 10 秒周期不变，PodReady 最终仍会异步变为 true。

## 测试与验收

### 单元与竞态测试

- PodReady 始终为 false，但 mounter 从 mounting 转为 ready 时，等待成功并执行传播探测。
- mounter 多轮未就绪后成功，且只使用一个总超时预算。
- Pod UID/标签改变、Pod 删除、mounter 重启、mounter 或 sandbox 容器退出时立即失败。
- ready 状态中的 RuntimeUID、PoolKey、Generation、mount type 或 cache contract 不匹配时失败。
- mounter 持续未就绪、持续 exec 错误及 context 取消时有界退出，不执行传播探测。
- 已经 PodReady 的 Pod 仍走相同强校验，避免形成安全性不同的旁路。
- 运行 Kubernetes runtime 目标包测试、race、vet 和全仓 Go 回归。

### 集群验收

用户构建并部署新的 sandbox-api 镜像后，在 `ds-ai-research`、
`aiadp-sandbox-fuse` 中验证：

- 记录连续不少于 30 次热池 FUSE 创建的 API、mounter-ready、传播探测和总耗时；
- 创建成功率 100%，热池创建 p95 不高于 3 秒，且耗时不再呈现 0～10 秒周期分布；
- 每个创建都命中预先存在的 Pool Pod，返回 RuntimeUID 与实际 Pod UID 一致；
- 创建后完成真实文件读写、flush 和销毁，Pool 自动回补到目标容量；
- 三个 API 副本均可参与请求，期间无错误领取、Pod 误删或 Redis 状态残留。

本地测试通过不等于线上性能通过；最终性能结论只在用户更新镜像后的集群验收中给出。

## 交付

实现只需要更新 sandbox-api 镜像。Helm values、workspace-mounter 镜像、AppArmor loader、
Redis/Sentinel 和 Chat 镜像均不需要因本优化而变更。用户继续统一构建、推送和部署镜像；
本地不直接修改线上 release。
