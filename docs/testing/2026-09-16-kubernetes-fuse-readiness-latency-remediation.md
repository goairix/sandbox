# Kubernetes FUSE 热池创建延迟整改记录

## 结论

第一阶段移除了 FUSE 热池创建对 Kubernetes `PodReady` condition 的串行等待。API 现在直接
等待真实 mounter ready 证明，随后执行原有 sandbox 侧 `write-read-delete` 传播探测；Pod
readinessProbe 仍保留用于 Kubernetes 运维状态，不再阻塞请求返回。第一阶段上线后，旧的
10 秒周期长尾已消失，但 30 次创建的 p95 仍高于 3 秒目标。

第二阶段进一步移除 `workspace-mounter health ready` 中与强传播探针重复的远端 `/bin/ls`，
保留进程、真实挂载、exact mount ID 和 cache 安全检查。第二阶段本地实现与回归已完成，必须
等用户构建并部署新的 `sandbox-fuse-mounter` 镜像后再执行线上基准；本记录不把本地测试写成
线上 p95 已达标。

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

## 第一阶段变更范围

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

## 第一阶段线上验收结果

用户部署 `sandbox-api:v0.3.30` 后，在 `ds-ai-research`、`aiadp-sandbox-fuse` 完成以下验证：

- 三个 API 副本 `/ready` 均返回正常，副本零重启；
- 完整 deployed API remediation suite 通过，30 次 FUSE 创建全部成功，p50 为 3.879 秒、
  p95 为 5.652 秒、p99 为 6.685 秒；
- 另行执行的 30 次轻量跨副本创建、写入和销毁全部成功，p50 为 0.854 秒、p95 为
  3.695 秒、p99 为 4.066 秒，最小 0.645 秒、最大 4.066 秒，其中 27/30 小于 3 秒；
- 同请求观测中，mounter-ready 约为 0.598～2.150 秒，API 随后的强传播探针及其它收尾约为
  0.381～1.074 秒；
- 验收结束后普通 Pool 和 FUSE Pool 均恢复 3 个 prepared Pod，没有 active 非池 sandbox、
  deleting managed Pod 或 Redis session/lease/owner 残留，三个 API 副本无关键错误日志。

这些数据证明 PodReady 的 10 秒周期等待已被消除，但也证明热池 p95 不高于 3 秒的目标尚未
达到，因此继续进行第二阶段。

## 第二阶段：mounter 远端 I/O 去重

分段观测和代码检查确认，`workspace-mounter health ready` 在持有 Supervisor 互斥锁时执行
远端 `/bin/ls -U -- <mount>`，API 随即又在 sandbox 内执行更强的
`workspace-probe write-read-delete`。后者实际创建、写入、`fsync`、重新打开、读取并删除
探针对象，并绑定 exact RuntimeUID 和 Generation；前一个目录读取既没有增加交付证明，也会
引入对象存储延迟以及 kubelet readinessProbe 与 API 之间的锁竞争。

第二阶段实现已删除 `ReadyStatus` 中的 `/bin/ls` 及其专用 deadline context。以下保护保持
不变：

- 受监管 s3fs 进程早退检测；
- 在原 mount deadline 内等待真实 `fuse.s3fs` mount；
- exact mount ID 的记录、复检和替换检测；
- cache 扫描、soft-limit 检查及超限时的 fail-closed 终止；
- Kubernetes 与 Docker runtime 在 mounter ready 后执行的 mandatory
  `workspace-probe write-read-delete`，传播失败时仍不交付 sandbox。

Kubernetes readinessProbe 因此只表达本地挂载组件健康，runtime 强传播探针表达客户可用。
受管 runtime Pod 不是面向流量的 Service endpoint，API 也不使用 PodReady 作为交付授权，
所以不存在绕过传播探针的交付路径。

### 第二阶段 TDD 与回归证据

先新增回归并在旧实现上执行：

```text
go test ./internal/mounter -run '^TestReadyPollsStartupMountWithoutRemoteReadAndRecordsExactMountID$' -count=1
```

用例按预期失败，唯一行为失败为 `runner.runs` 包含：

```text
[/bin/ls -U -- <temporary-workspace-path>]
```

删除远端读取后，定向用例、10 轮 mounter race、Kubernetes 和 Docker runtime 的交付门测试
均通过。全量回归最初发现 4 个 shutdown 测试把旧 `/bin/ls` 计入固定命令下标；实际 flush、
unmount、取消和幂等行为均正确。断言改为直接验证 `verified-flush → fusermount3` 语义序列后，
完整 mounter 包和 race 通过。并行启动多个全仓命令时 sandbox 的一个 5 秒时序用例单次失败，
该用例独立连续 5 次通过，最终非并行全仓测试也通过，因此没有修改 sandbox 业务代码。

最终执行并通过：

```text
go test ./internal/mounter -run 'Test(Ready|SupervisorReady|FlushRejectsMountIdentity|Cache)' -count=1
go test -race ./internal/mounter -run 'Test(Ready|SupervisorReady|FlushRejectsMountIdentity|Cache)' -count=10
go test ./internal/runtime/kubernetes -run 'TestWaitReady' -count=5
go test ./internal/runtime/docker -run 'Test(DockerPoolHitAuthorizesSameContainer|WaitReady|DockerFUSEFixedControlExecs)' -count=5
go test ./internal/mounter ./cmd/workspace-mounter ./internal/runtime/kubernetes ./internal/runtime/docker
go test -race ./internal/mounter ./internal/runtime/kubernetes ./internal/runtime/docker
go vet ./internal/mounter ./cmd/workspace-mounter ./internal/runtime/kubernetes ./internal/runtime/docker
go vet ./...
go test ./...
git diff --check
```

第二阶段提交记录：

- `12c2971 docs: design mounter readiness io deduplication`
- `b6bfdb5 docs: plan mounter readiness io deduplication`
- `fa2aceb test: expose duplicate mounter readiness io`
- `0b26900 perf: deduplicate mounter readiness io`
- `3b68158 test: align shutdown assertions with local readiness`

### 第二阶段部署范围

本阶段只需重新构建 `sandbox-fuse-mounter` 镜像，并在 Helm values 中更新 mounter 镜像 tag。
不需要因本阶段重新构建 sandbox-api、普通 sandbox、AppArmor loader、Redis/Sentinel 或 Chat
镜像，也没有新增 values 配置。mounter 镜像变化会进入 FUSE Pool key/backend fingerprint，
现有 drain 与重建流程应替换旧 FUSE Pool，不影响普通 sandbox Pool。

## 第二阶段部署后验收

用户部署新的 `sandbox-fuse-mounter` 镜像并完成 FUSE Pool 替换后执行以下验收：

1. 连续创建不少于 30 个热池 FUSE sandbox；
2. 成功率 100%，热池创建 p95 不高于 3 秒；
3. 创建耗时不再出现由 10 秒 readiness 周期造成的随机分布；
4. 每次使用预先存在的 Pool Pod，API RuntimeUID 与 Pod UID 一致；
5. 每次完成真实文件读写、flush、销毁，Pool 自动恢复目标容量；
6. 三个 API 副本均参与请求且无误领取、误删或 Redis 状态残留。

如果后端存储本身偶发超过 3 秒，将通过 `mounter_ready_wait` 和
`propagation_probe` 分阶段数据继续区分挂载、传播和 API 其它耗时，不再与 PodReady
周期混淆。

## 第二阶段 ds-ai-research 现场验收结果

用户部署 `sandbox-fuse-mounter:v0.3.30` 后，于 2026-09-16 在
`ds-ai-research`、`aiadp-sandbox-fuse` 完成现场验收。三个 prepared FUSE Pool Pod 实际
运行同一镜像 digest `sha256:56db3a0e82c36259ff5325cf5294a5c3b876c085b8ccde147c41c76bca2e8931`，
均为零重启。

升级期间 Helm 先进入 `pending-upgrade`，pre-upgrade drain 按设计把 API 缩到 0 并清理旧
Pool；drain 完成后 resume hook 恢复 API 和 Pool，最终 release revision 12 为 `deployed`，
API 3/3 Ready。prepared FUSE Pool Pod 显示 `1/2` 且产生 mounter readiness warning 是当前
状态机的预期表现：`health prepared` 返回完整 prepared 状态，未领取前 `health ready` 必须
失败。该现象会造成 Dashboard 噪声，但不等于 FUSE 创建失败。

### 功能与并发

完整 deployed API remediation suite 执行 479.17 秒并通过，覆盖三副本直连、普通沙盒、
资源限额、动态 workspace、网络策略正负例、流式接口，以及 30 轮 FUSE 创建、跨副本 exec、
sync、三副本 flush 状态一致性、拒绝动态卸载和销毁。

另外同时领取三个 prepared FUSE Pool Pod，三次创建分别为 0.770、0.795、2.077 秒；跨副本
exec 和销毁全部成功，没有重复领取。三个普通沙盒并发创建分别为 0.200、0.198、0.183 秒，
跨副本 exec 全部成功。本次 mounter 变更未影响普通 Pool。

### 性能结果

完整套件的 30 次连续 FUSE 创建全部成功：

- p50：3.835 秒；
- p95：6.013 秒；
- p99：6.972 秒。

该循环在销毁返回后立即开始下一次创建，可能把异步补池竞争和冷创建混入样本，因此又执行
隔离热池基准：每轮开始前都确认三个 FUSE prepared Pod 已补齐，然后创建、跨副本写读删除
测试文件、销毁，再等待下一轮库存恢复。30 次全部成功：

- min：0.653 秒；
- p50：1.238 秒；
- p95：3.770 秒；
- p99/max：3.791 秒。

升级前轻量基准为 p50 0.854 秒、p95 3.695 秒。考虑样本量和对象存储波动，第二阶段没有证明
p95 得到实质改善，也仍未达到不高于 3 秒的目标。远端 `/bin/ls` 已确认不再是交付必需步骤，
但当前主要长尾位于 s3fs 启动挂载或 mandatory `write-read-delete` 强传播探针；下一轮优化必须
先补充这两段的逐请求分段观测，不再删减安全校验猜测提速。

普通沙盒三次并发销毁分别为 32.187、31.958、32.183 秒，仍复现此前记录的约 30 秒历史
问题；它不属于本次 mounter 变更，也没有因本次升级恶化。

### 最终清理与健康状态

- Helm revision 12 为 `deployed`，API 3/3 Ready；
- 普通 Pool 3 个、FUSE Pool 3 个，active non-pool 和 deleting managed Pod 均为 0；
- managed NetworkPolicy 6 个，managed CiliumNetworkPolicy 0 个，符合当前关闭用户网络的池
  基线；
- Redis session v2、ephemeral lifecycle、workspace lease 和 workspace owner 扫描均为 0；
- pre/post hook Job 已按策略删除；
- 三个 API 副本自升级后的关键错误日志计数均为 0；
- 本地三个 port-forward 已停止。

现场结论是：功能、跨副本一致性、并发和清理验收通过；FUSE 热池创建 p95 性能验收未通过。
