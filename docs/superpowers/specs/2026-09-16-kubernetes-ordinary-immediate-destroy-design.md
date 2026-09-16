# Kubernetes 普通沙盒快速安全销毁设计

## 背景与现场证据

`ds-ai-research` 集群的普通 Kubernetes 沙盒销毁稳定需要约 32 秒。2026-09-16 对一个由热池领取的
临时沙盒做了事件级观测：

- Pod 的 `terminationGracePeriodSeconds` 为 30；
- sandbox 容器 PID 1 的命令为 `sleep infinity`，没有 `preStop` 或其它退出清理逻辑；
- DELETE 请求发出约 1.113 秒后 Pod 进入 Terminating；
- Terminating 期间容器仍为 Running；
- Pod 在 32.561 秒时才变成 NotFound；
- API 在 Pod 消失后仅用约 7 毫秒完成网络策略清理并返回，总耗时 32.568 秒。

因此慢点不是 Redis、多副本协调或 NetworkPolicy 删除，而是 kubelet 等满普通 Pod 的 30 秒优雅
终止期后才结束一个不会自行退出的 PID 1。

同轮 FUSE 分段观测确认，FUSE 创建剩余长尾来自真实 s3fs 挂载和 mandatory
`write-read-delete` 强传播验证。本设计不删减 FUSE 安全校验，也不修改 FUSE 销毁的 flush、
unmount、`preStop` 或终止宽限期。

## 目标

1. 普通 Kubernetes 沙盒销毁不再等待无效的 30 秒终止宽限期。
2. 已经存在、Pod spec 仍为 30 秒宽限期的普通热池和业务 Pod，也能立即受益。
3. 保持 exact UID 删除、分布式 fencing、活动操作归零、同步 workspace 最终输出和网络策略清理
   顺序不变。
4. 删除结果不确定时继续 fail closed，不把超时、API Server 失联或同名 Pod replacement 当成原
   Pod 已安全清理。

## 非目标

- 不修改 FUSE Pod 的销毁流程或超时。
- 不给普通 runtime 镜像增加新的 PID 1、reaper 或信号转发协议。
- 不改变普通沙盒创建、热池领取、业务身份 annotation、Redis 数据模型或 HTTP API。
- 不以异步返回代替物理删除确认。

## 方案选择

在 `deleteExactOrdinaryPod` 发出的 Kubernetes Pod DELETE 中，同时携带：

- 原有的 immutable UID precondition；
- 显式 `gracePeriodSeconds: 1`。

不只修改新 Pod 的 `spec.terminationGracePeriodSeconds`。如果只改 Pod spec，Helm 更新后仍然存活的
旧普通热池 Pod 会继续使用 30 秒宽限期，直到它们被逐个消费或退休；在 DELETE options 中显式
设置一秒宽限期则对新旧普通 Pod 都有效。

不能使用零宽限期。Kubernetes 1.33 官方 Pod lifecycle 明确说明，零宽限是 force deletion：API
Server 不等待 kubelet 确认节点上的进程已经终止，资源甚至可能继续运行。当前清理链路把原 Pod UID
消失作为删除网络策略的前提，所以 force deletion 会破坏这一证明并产生失防窗口。一秒是最短的
非零 graceful deletion：kubelet 先发送 TERM，宽限期结束后向仍存活进程发送 KILL，确认 Pod
终止后再移除 API 对象。参考：
https://v1-33.docs.kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/#forced-pod-termination

也不新增 runtime PID 1 helper。普通 Pod 没有容器内持久化收尾协议；Kubernetes exec 启动的
进程也不一定是 PID 1 的直接子进程。为此引入一个信号转发器会额外扩大 runtime 镜像、进程回收
和超时协议，而不能替代 Manager 已有的操作 fencing 与 workspace 最终同步。

## 删除时序与安全边界

普通分布式销毁保持现有顺序：

1. `BeginDestroy` 持久化关闭新的业务操作入口；
2. 等待该 sandbox 的 live operations 归零；
3. 获取 cleanup controller 并执行 generation/capability fencing；
4. sync workspace 如有需要，先完成并持久化最终输出 checkpoint；
5. 用原 Pod UID precondition 和一秒非零宽限期提交 DELETE；
6. 轮询 exact Pod，直到原 UID NotFound，或确认同名对象已经是不同 UID；
7. 原 Pod 终止得到确认后，才删除它拥有的 CiliumNetworkPolicy/NetworkPolicy；
8. 持久化 `runtime_removed`，再删除 session/active record 并触发补池。

一秒非零宽限期只压缩第 5～6 步等待 kubelet 结束无清理协议容器的时间，不绕过前四步的业务
收口，也不改变后续策略和持久化清理顺序。即使 PID 1 不响应 TERM，kubelet 也会在一秒后发送
KILL；API 不依赖 PID 1 配合，也不需要重建 runtime 镜像。

普通 sandbox 容器的 `/workspace`/`/tmp` 是 disposable Pod 存储；需要持久化的 sync workspace
输出已在 runtime 删除前完成。普通 Pod 没有 mounter sidecar、durable flush、unmount 或 preStop
契约，所以不应沿用 FUSE Pod 的长终止保护。

## 失败语义

- DELETE 被 API Server 拒绝、请求上下文取消或超时：继续返回 termination unconfirmed/cleanup
  pending，不删除持久化所有权证据。
- UID precondition 不匹配：不能删除同名 replacement；按现有 exact identity 语义处理。
- DELETE 返回成功但原 UID 仍可见：继续等待，达到既有 termination timeout 后失败关闭。
- 原 UID 消失后 NetworkPolicy 删除失败：保留现有可重试的 cleanup pending 状态，不声称整体销毁
  成功。
- finalizer 或其它 Kubernetes 控制面条件阻止 Pod 消失时，一秒非零宽限期不绕过这些条件，仍由现有
  有界等待和恢复协调器接管。

## 测试策略

先写失败回归，再修改实现：

1. fake client 捕获 Pod DELETE action，断言 immutable UID precondition 与
   `gracePeriodSeconds == 1` 同时存在，并明确禁止零值；旧实现应因 grace 字段为空而失败。
2. 保留并复跑同名 replacement、删除结果不确定、Pod 消失后才清理策略等现有测试。
3. 验证普通 Pool、直接创建、create failure compensation 和 obsolete Pool retirement 都共用 exact
   ordinary delete，不误改 FUSE 删除。
4. 完成 Kubernetes runtime、sandbox、race、vet 和全仓回归。

部署后的 `ds-ai-research` 验收至少覆盖：

- 三个 API 副本健康；
- 领取部署前已存在、Pod spec 仍为 30 秒的普通 Pool Pod，再销毁成功；
- 10 次普通热池沙盒创建/跨副本 exec/销毁全部成功，销毁 p95 不高于 5 秒；
- 三副本同时 DELETE 同一普通沙盒，所有请求得到一致成功结果；
- 最终普通 Pool 恢复目标容量，没有 active 非池 Pod、Terminating managed Pod、孤儿网络策略或
  Redis active/session/controller 残留；
- FUSE 创建、读写、flush 和销毁冒烟仍通过，证明普通 DELETE options 没有泄漏到 FUSE 路径。

## 发布范围

实现只修改 Kubernetes runtime 的普通 exact Pod 删除参数及其测试、文档。只需构建和更新
`sandbox-api` 镜像；不需要重建 `sandbox-runtime`、`sandbox-fuse-mounter`、AppArmor loader、
Redis/Sentinel 或 Chat 镜像，也不新增 Helm values 配置。
