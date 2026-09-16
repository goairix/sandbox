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

当前仍是待部署状态，不宣称线上销毁延迟已改善。用户部署新 `sandbox-api` 后执行：

1. 确认三个 API 副本 Ready 且使用新镜像 digest；
2. 领取至少一个部署前已经存在、Pod spec 仍为 30 秒 grace 的普通 Pool Pod并销毁；
3. 完成 10 次普通热池创建、跨副本 exec、销毁，要求成功率 100%，销毁 p95 不高于 5 秒；
4. 连续三轮执行三副本同时 DELETE 的 ephemeral/persistent 回归；
5. 执行一个 FUSE 真实写读、flush、销毁冒烟，证明普通快速删除没有进入 FUSE 路径；
6. 最终确认普通 Pool 和 FUSE Pool 各恢复目标容量，没有 active 非池 Pod、Terminating managed
   Pod、测试策略或 Redis active/session/controller/lease 残留。
