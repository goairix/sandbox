# Kubernetes FUSE 热池创建延迟整改记录

## 结论

本次整改移除了 FUSE 热池创建对 Kubernetes `PodReady` condition 的串行等待。API 现在直接
等待真实 mounter ready 证明，随后执行原有 sandbox 侧 `write-read-delete` 传播探测；Pod
readinessProbe 仍保留用于 Kubernetes 运维状态，不再阻塞请求返回。

本地实现与回归已完成。线上 30 次热池基准必须等用户构建并部署新的 sandbox-api 镜像后
执行，本记录不把本地测试写成线上 p95 已达标。

## 根因证据

在 `ds-ai-research`、`aiadp-sandbox-fuse` 的同一次创建请求中测得：

- mounter 直接 `health ready`：1.261 秒；
- Kubernetes PodReady：9.849 秒；
- API 返回：10.191 秒。

mounter readinessProbe 的 `periodSeconds` 为 10。真实挂载完成后若错过 kubelet 上一轮
探测，请求仍会等待下一轮 probe 和 Pod condition 传播，产生 0～10 秒的随机附加延迟。
旧 `WaitSandboxReady` 随后又执行 mounter 状态及传播探测，因此 PodReady 是重复门槛。

## 实现

`internal/runtime/kubernetes/runtime.go` 的 FUSE readiness 路径调整如下：

1. 在一个共享 `readyTimeout` 内读取 exact Pod；
2. 校验不可变 Pod UID、FUSE Pool 身份标签、PoolKey 和 bootstrap contract；
3. 校验 Pod 未删除、workspace-mounter 未重启且仍在运行、sandbox 容器仍在运行；
4. 执行 mounter `health ready`，临时控制错误只在总超时内重试；
5. 成功后再次读取 exact Pod，关闭 exec 期间的身份/容器状态竞态窗口；
6. 严格校验 RuntimeUID、PoolKey、Generation、mount type 和 cache contract；
7. 执行原有 sandbox 侧 `write-read-delete` 传播探测后才返回。

精确的 `state=mounting` 状态只有在身份、代次、空 mount type、重启和 cache 字段全部符合
预期时才允许继续等待。任何其它已返回状态字段不匹配立即 fail-closed。

阶段指标由容易误解的 `pod_ready` + `mounter_status` 串行阶段改为
`mounter_ready_wait`，传播探测继续记录为 `propagation_probe`。

## TDD 证据

新增回归首先在旧实现上运行：

```text
go test ./internal/runtime/kubernetes -run 'TestWaitReady' -count=1
```

新增用例按预期失败，主要错误为：

```text
wait for FUSE Pod Ready: context deadline exceeded
```

这证明用例捕获的是旧 PodReady 门槛，而不是实现后的内部结构。实现后相同测试组通过，并
覆盖：

- PodReady 始终 false、mounter 已 ready 时成功；
- 临时 mounter 控制错误后重试成功；
- 精确 mounting 状态后转为 ready；
- 持续控制错误在总超时内退出且不执行传播探测；
- sandbox 容器在重试前退出时立即失败；
- 现有 generation/cache/身份字段不匹配继续失败。

提交记录：

- `54f7678 test: expose kubernetes fuse readiness delay`
- `8e53555 perf: remove pod readiness delay from fuse creation`

## 本地验证结果

以下命令于 2026-09-16 执行并通过：

```text
go test ./internal/runtime/kubernetes -run 'TestWaitReady' -count=1
go test ./internal/runtime/kubernetes -count=5
go test -race ./internal/runtime/kubernetes -run 'TestWaitReady' -count=10
go test ./internal/runtime/kubernetes ./internal/sandbox ./internal/mounter ./cmd/workspace-mounter
go vet ./internal/runtime/kubernetes ./internal/sandbox ./internal/mounter ./cmd/workspace-mounter
go test ./...
git diff --check
```

完整 `go test ./...` 包含仓库内 API、runtime、Docker/Kubernetes、sandbox、Redis 状态、
Helm 和 workspace FUSE 测试包，命令退出码为 0。未配置外部环境而由测试自身跳过的场景不
视为真实集群验收。

## 变更范围

本次没有修改：

- readinessProbe 或其 10 秒周期；
- Helm values/schema/template；
- Redis/Sentinel 数据结构；
- API 请求/响应协议；
- workspace-mounter 控制协议或镜像；
- AppArmor loader；
- 网络策略和 CNI 逻辑。

因此只需要重新构建和部署 `sandbox-api` 镜像，其他镜像和线上 values 不需要因本次优化
变更。

## 部署后验收

用户部署新 sandbox-api 镜像后执行以下验收：

1. 连续创建不少于 30 个热池 FUSE sandbox；
2. 成功率 100%，热池创建 p95 不高于 3 秒；
3. 创建耗时不再出现由 10 秒 readiness 周期造成的随机分布；
4. 每次使用预先存在的 Pool Pod，API RuntimeUID 与 Pod UID 一致；
5. 每次完成真实文件读写、flush、销毁，Pool 自动恢复目标容量；
6. 三个 API 副本均参与请求且无误领取、误删或 Redis 状态残留。

如果后端存储本身偶发超过 3 秒，将通过 `mounter_ready_wait` 和
`propagation_probe` 分阶段数据继续区分挂载、传播和 API 其它耗时，不再与 PodReady
周期混淆。

