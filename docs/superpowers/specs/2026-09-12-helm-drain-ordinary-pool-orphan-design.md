# Helm Drain 普通 Pool 孤儿清理设计

## 背景与根因

Helm 在对象存储 backend fingerprint 变化时运行 `backend-change-drain` pre-upgrade
Job。Job 先把 sandbox-api Deployment 和 HPA 下线，再构造一个 Manager 并直接调用
`DrainRelease`。这个专用路径不会调用 `Manager.Start`，因此不会执行正常启动阶段的
`cleanupOrphanedPoolContainers`。

普通预热 Pool 只保存在 sandbox-api 进程内存中。进程异常退出、旧版本删除失败，或
Deployment 缩容时留下的 `sandbox.pool=true` Pod 不会出现在 drain Job 的新 Pool 实例
里。当前 `DrainRelease` 对空内存 Pool 调用 `Pool.Drain` 后即结束，最终 Kubernetes
零状态审计会看到遗留 Pod 及其 NetworkPolicy 并阻止 Helm 升级。

## 目标

- release drain 必须清理不属于 FUSE 的普通 Pool 孤儿 Pod 及其策略。
- 保持最终 `sandbox.managed=true` 零资源审计，不能通过忽略 Pool 资源解除阻塞。
- 清理必须 fail-closed：列举或删除失败需要从 `DrainRelease` 返回。
- 不启动 Manager、不补充预热池，也不改变常规启动阶段的 best-effort 孤儿清理语义。
- 不清理 FUSE Pool；FUSE 继续由 Redis inventory、UID fencing 和现有
  `FUSEPool.DrainRelease` 管理。
- 孤儿扫描仅用于 Kubernetes release drain。Docker 没有 Kubernetes 零状态审计，
  且历史普通/FUSE Pool 的标签契约不同，不能安全套用同一扫描规则。
- release drain 必须停止、取消并等待普通 Pool 后台 refill，避免清理快照之后又创建 Pod。

## 设计

在 Manager 中增加 drain 专用的普通 Pool 清理方法：

1. 仅当 `RuntimeType=kubernetes` 时，通过 runtime 列举
   `sandbox.managed=true,sandbox.pool=true` 的运行时。
2. 同时识别 `sandbox.workspace.mode=fuse` 与历史/跨后端
   `sandbox.role=fuse-runtime`，避免绕过 FUSE 的持久化所有权协议。
3. 再次验证返回对象同时带有 managed 与 pool 标识，拒绝删除身份不完整的对象。
4. 对每个普通 Pool runtime 调用 `RemoveSandbox`。Kubernetes 实现会读取 Pod UID，
   先精确删除 Pod，再清理与该 runtime identity 绑定的 NetworkPolicy/Cilium 策略。
5. 聚合全部删除错误并返回，确保一个失败不会阻止尝试清理其他孤儿。
6. 普通 Pool 进入 stopping 状态后拒绝新 refill，取消 pool-owned refill context 并等待
   已调度任务退出；任务若在取消后仍成功创建 runtime，则把它交回最终 drain inventory。
7. 在 `DrainRelease` 完成业务 sandbox、FUSE Pool 和内存普通 Pool drain 后调用该方法，
   随后继续执行 Redis 与 Kubernetes 零状态审计。

正常启动使用的 `cleanupOrphanedPoolContainers` 保持原样；它需要保护已恢复的业务
runtime，并且为了兼容性仅记录单个清理失败。release drain 已先清空所有业务状态，
因此专用方法可以安全地把所有非 FUSE Pool runtime 视为待删除资源。

## 错误处理

- `ListSandboxes` 失败：返回带上下文的错误，hook 失败。
- 某个 `RemoveSandbox` 失败：记录 runtime ID，继续清理其他项，最终返回聚合错误。
- FUSE Pool runtime：跳过，由已有 FUSE drain/reconcile 路径处理。
- Docker runtime：不执行 Kubernetes 孤儿扫描；仍清理当前进程内存 Pool。
- refill 正在创建 runtime：取消并等待；若创建实现忽略取消后仍返回成功，最终 inventory
  仍会删除该 runtime。
- 清理成功但 Kubernetes 仍存在受管资源：保留现有最终审计作为最后一道防线。

## 测试

新增 Manager 回归测试，构造一个没有进入当前 Pool 内存列表的普通 Pool runtime，调用
`DrainRelease` 后断言它被删除。再覆盖 FUSE Pool runtime 不被普通清理误删，以及删除
失败会从 `DrainRelease` 返回。运行 sandbox 包测试、Kubernetes runtime 测试、race
测试和全仓测试。

## 部署与当前阻塞恢复

代码修复仍只需要重新构建 sandbox-api 镜像。当前失败的 hook 需要先确认遗留资源带有
`sandbox.pool=true` 且不是 FUSE；确认后删除该 Pod，绑定的策略可由现有 API 精确清理，
或在 Pod 已不存在时按 identity 单独删除。然后删除失败 Job 并重试 Helm upgrade。生产
操作必须先读取资源标签与 owner identity，不能仅凭数量删除。
