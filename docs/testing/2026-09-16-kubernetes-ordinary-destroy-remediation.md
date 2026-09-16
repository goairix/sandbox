# Kubernetes 普通沙盒销毁延迟整改记录

## 根因

2026-09-16 在 `ds-ai-research`、`aiadp-sandbox-fuse` 对一个从热池领取的普通临时沙盒进行
事件级观测：

- Pod 的 `spec.terminationGracePeriodSeconds` 为 30；
- sandbox 容器 PID 1 为 `sleep infinity`，没有 `preStop` 或其它容器内终止协议；
- DELETE 请求发出约 1.113 秒后，Pod 出现 `deletionTimestamp`；
- Terminating 期间容器仍为 Running；
- 原 Pod UID 在 32.561 秒时变为 NotFound；
- API 在 Pod 消失后约 7 毫秒完成后续清理并返回，总耗时 32.568 秒。

因此稳定的约 32 秒销毁时间来自 kubelet 等满 30 秒 Pod grace 后才结束不会自行退出的 PID 1，
不是 Redis、多副本 cleanup controller 或 NetworkPolicy 删除。

## 实现与安全边界

`deleteExactOrdinaryPod` 发出的 DELETE 现在同时携带：

- 原有 immutable Pod UID precondition；
- 显式 `gracePeriodSeconds: 1`。

改动没有修改普通 Pod 模板中的 30 秒 grace。只有经过 sandbox Manager 销毁编排的 exact ordinary
DELETE 会使用最短非零宽限期，因此部署前已经存在的旧热池/业务 Pod 可以直接受益，同时外部手工
删除仍保留原有 Pod spec 行为。

自审期间曾实现零宽限期，但在交付前识别并否决：Kubernetes 1.33 的零宽限属于 force deletion，
API Server 不等待 kubelet 确认节点进程终止，不能继续把 Pod NotFound 作为删除网络策略的安全证明。
一秒是最短的非强制删除；kubelet 在 TERM 后一秒发送 KILL，并在容器终止后移除 Pod API 对象。
参考官方文档：
https://v1-33.docs.kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/#forced-pod-termination

以下安全顺序保持不变：

1. 持久化 `BeginDestroy`，拒绝新业务操作；
2. 等待 live operations 归零；
3. cleanup controller 执行 generation/capability fencing；
4. sync workspace 在 runtime 删除前完成最终输出并写 checkpoint；
5. DELETE 仍绑定原 Pod UID；
6. 仍等待原 UID NotFound，或确认同名对象已经是不同 UID；
7. 只有 Pod 终止得到确认后，才清理其 CiliumNetworkPolicy/NetworkPolicy；
8. 删除或读取结果不确定时继续返回 termination unconfirmed/cleanup pending，保留恢复证据。

FUSE 使用独立的 mounter flush、unmount、`preStop`、cleanup finalizer 和终止确认路径，本次没有
修改其 DELETE options、终止宽限期或任何传播安全校验。

## TDD 证据

先新增删除参数测试，在旧实现上执行：

```text
go test ./internal/runtime/kubernetes -run '^TestOrdinaryDeletionRequestsImmediateExactUID$' -count=1
```

测试按预期只在 `options.GracePeriodSeconds` 为 `nil` 处失败；同一测试中的 UID precondition 断言
已经通过。这证明回归捕获的是无效 grace 等待，而不是现有 exact identity 行为。

进一步安全自审将测试修正为 `TestOrdinaryDeletionRequestsMinimumGracefulExactUID`；它在临时零宽限
实现上按预期失败，错误为 `expected: 1, actual: 0`。改为一秒非零宽限后通过。这一轮 RED→GREEN
证明最终回归不仅限制延迟，还明确禁止 force deletion。

提交记录：

- `a06f9d9 test: expose ordinary pod grace delay`
- `c269b19 perf: eliminate ordinary pod grace wait`
- `187d6ec fix: preserve graceful ordinary termination proof`

## 本地验证

2026-09-16 执行并通过：

```text
go test ./internal/runtime/kubernetes -run '^TestOrdinaryDeletionRequestsMinimumGracefulExactUID$' -count=1
go test ./internal/runtime/kubernetes -run 'Test(OrdinaryDeletion|RemoveSandbox|ExactOrdinaryRemoval|RemoveOrdinarySandbox)' -count=20
go test -race ./internal/runtime/kubernetes -run 'Test(OrdinaryDeletion|RemoveSandbox|ExactOrdinaryRemoval|RemoveOrdinarySandbox)' -count=10
go test ./internal/runtime/kubernetes ./internal/sandbox
go test -race ./internal/runtime/kubernetes ./internal/sandbox
go vet ./internal/runtime/kubernetes ./internal/sandbox
go test ./internal/runtime/kubernetes -run 'Test(ConfirmPreparedSandboxTermination|FinalizePreparedSandboxRemoval|PreparedSandboxTermination|RemovePreparedSandbox)' -count=10
go vet ./...
go test ./...
git diff --check
```

完整 `go test ./...` 覆盖 API、runtime、Docker/Kubernetes、sandbox、Redis 状态、Helm、mounter
和 workspace FUSE 测试包，退出码为 0。未配置外部环境而由测试自身跳过的场景不计作真实集群
验收。

## 自审

- 生产代码只改动 `internal/runtime/kubernetes/ordinary_network.go` 的 exact ordinary Pod DELETE；
- UID precondition 与一秒非零宽限期位于同一个 `DeleteOptions`；
- 仓库最终状态没有给任何 Pod DELETE 新增 `gracePeriodSeconds: 0`；
- NotFound/replacement UID 轮询和 termination timeout 未改；
- 网络策略仍在 `deleteExactOrdinaryPod` 成功后清理；
- 普通 Pod 模板、FUSE 删除、Helm、Redis、API 协议和镜像构建文件均未改；
- 本文没有把本地 fake-client 回归写成线上延迟已经达标。

## 部署范围

只需构建并部署新的 `sandbox-api` 镜像。没有新增或修改 Helm values；不需要重建
`sandbox-runtime`、`sandbox-fuse-mounter`、AppArmor loader、Redis/Sentinel 或 Chat 镜像。

普通热池 Pod 无需因本次改动主动重建：DELETE options 由 API 发出，所以现存
`terminationGracePeriodSeconds: 30` 的旧 Pod 正是兼容性验收对象。

## 线上验收

2026-09-16 在 `ds-ai-research / aiadp-sandbox-fuse` 完成部署后验收。Helm release revision 13
状态为 `deployed`；三个 API 副本均使用
`registry.i.huaxisy.com/library/ai-infra/sandbox-api:v0.3.31`，镜像 digest 均为
`sha256:1e8e95347f805417998d4555d6d107d37e0895929bed8d095b33eda79f15cd4a`，全程 3/3
Ready、零重启。

### 普通沙盒销毁性能与安全

通过三个 Pod 直连端点轮转完成 10 次热池领取、跨副本 Python exec、跨副本 DELETE，并在每次销毁后
从三个副本确认 GET 404。10/10 成功，销毁耗时为：

```text
2649ms 2767ms 3143ms 3234ms 3415ms 3434ms 3491ms 3522ms 3726ms 3891ms
min=2649ms p50=3415ms p95=3891ms p99=3891ms max=3891ms avg=3327ms
```

按 nearest-rank 计算，小样本的 p95/p99 均等于最大值 3.891 秒，低于 5 秒验收线。被领取 Pod 的
spec 仍为 30 秒；首个样本在 DELETE 后 218ms 观察到 `deletionTimestamp`，此时容器仍为 Running，
3389ms 后原 UID 才 NotFound，API 总耗时 3522ms。该时间线证明它经过了非零优雅终止和节点侧
终止确认，不是零宽限 force deletion。

`TestDeployedOrdinaryConcurrentDestroy` 连续执行三轮；ephemeral/persistent 各一例、每例由三个 API
副本同时 DELETE，共 18/18 个 DELETE 返回 HTTP 200 及有效 `sandbox destroyed` ACK，随后三个
副本查询均为 404，包总耗时 34.519 秒。

### 回归与收尾

`TestDeployedAPIRemediation` 通过，覆盖跨副本 Python/Node exec、缺失与空文件、动态 sync 工作区
挂载/同步/卸载、真实 cgroup v2 内存与 CPU 限额、one-shot SSE，以及 FUSE 写入、flush、三副本
共享状态和销毁。FUSE 单个创建样本为 6.625 秒；功能隔离通过，但该样本仍高于 3 秒性能目标，
不能据此关闭 FUSE 创建延迟优化项。

最终现场恢复为三个普通 prepared Pool Pod 和三个 FUSE prepared Pool Pod，没有 active 非池或
Terminating managed Pod；NetworkPolicy 恢复为三条基础策略和六条当前 Pool 策略，没有
CiliumNetworkPolicy。Redis 只读扫描结果为：active record 0、active controller 0、session v2 0、
ephemeral lifecycle 0、workspace owner 0、workspace lease 0。三个 API 副本仍为 3/3 Ready、零重启，
最近 30 分钟日志未匹配 ERROR、panic、termination unconfirmed、timeout 或 refill failed。

本轮通过 Pod port-forward 验收 API，不代表外部 Ingress/TLS 链路、Redis Sentinel 故障切换、网络
分区、其它 CNI 或持续容量压测已经完成。
