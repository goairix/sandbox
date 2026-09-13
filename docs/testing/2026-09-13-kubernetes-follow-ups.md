# Kubernetes 后续优化与未完成事项

## 范围与状态基线

现场核对时间：2026-09-13 16:57（Asia/Shanghai）。本记录以最新 [v0.3.22 部署验证](2026-09-13-ds-ai-research-v0.3.22-validation.md)、当前代码及本次只读现场核对为依据，不把旧报告中的失败直接当作当前未修复问题。

本次只记录和审计，不修改运行逻辑、values 或部署，不创建测试沙盒、不删除线上 Pod、策略或 Redis 状态。下面两个性能项按用户要求暂缓，后续再做专项优化；这不是实施计划或已修复声明。

现场仍为 `ds-ai-research / aiadp-sandbox-fuse`，三个 API 都运行 v0.3.22，Ready、零重启；一 Redis、三普通池、三 FUSE 池，无 Terminating Pod。NetworkPolicy 为两条基础策略和六条当前池策略，没有 CiliumNetworkPolicy。最近 15 分钟各 API 日志在每 Pod 至多 1500 行的采样内未匹配 ERROR、stale-token、reconciliation failed、cleanup deferred 或 unconfirmed；不代表整个历史日志没有错误。

## 暂缓的性能优化

### PERF-01：普通沙盒销毁约 31–33 秒

- 状态：已定位主要等待机制，尚未优化。
- 最新验证：普通 sync 并发 DELETE 约 31.0–32.5 秒，未挂载工作区的完整生命周期也约 32.30 秒。不能将这段等待主要归因于最终同步存储。
- main 的 `RemoveSandbox` 只提交 Kubernetes Pod Delete，接受请求即返回；当前 `deleteExactOrdinaryPod` 保留 UID 删除前置条件，并等待原 UID 的 Pod 不存在后再收尾策略。因此旧 API 返回快不证明原 Pod 当时已经退出。
- 普通 Pod 模板及线上 Pod 的 command 都是 `sleep infinity`，宽限期为 30 秒。只读 `/proc/1/status` 确认 PID 1 是 sleep，`SigCgt=0`，未注册退出信号处理器。结合 PID namespace 的 PID 1 信号规则，这解释了等待耗满宽限期的现象；本次未向线上 Pod 发送信号。
- 30 秒是正常终止的宽限上限，不是必须付出的固定等待成本。上次报告仅描述 graceful termination，本记录补充具体运行时原因。
- 后续方向：采用能可靠处理退出信号、管理子进程的可信 PID 1，让进程及时退出；继续保留精确 UID 终止确认和策略最后清理。不得用强删、零宽限期或仅确认 Delete 已受理来伪装优化。
- 验收：普通无工作区及两种 sync 模式都测创建、最终同步、进程退出、Pod 消失、策略收尾；跨副本并发 DELETE、后台恢复、超时和同名替换保护仍正确。延迟目标在专项基准后确定，不先承诺秒数。

依据：`internal/runtime/kubernetes/pod.go`、`internal/runtime/kubernetes/ordinary_network.go`、`internal/runtime/kubernetes/runtime.go`；[Linux PID namespace 信号规则](https://man7.org/linux/man-pages/man7/pid_namespaces.7.html)、[Kubernetes Pod 终止流程](https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/#pod-termination)。

### PERF-02：FUSE 热池创建耗时及波动

- 状态：已确认多个串行阶段和额外就绪等待，尚未按请求量化各阶段占比。
- 最新小样本：三副本补测约 5.94–7.55 秒；与工作区矩阵并行的另外三次创建中位数 8.843 秒、最大 11.424 秒。样本负载不同，不能作公平基准或长期 p95/p99。
- 热池预热的是 Pod 和 mounter，不预先挂载某个租户的前缀。申请仍需领取、租约、后端前缀准备、绑定及挂载授权、网络更新、真实挂载就绪和跨容器读写验证。不是每次完整复制后端存储内容。
- `WaitSandboxReady` 先等 Pod Ready，再检查 mounter 状态及读写传播。线上 native sidecar mounter 的 readiness probe `periodSeconds=10`，引入 kubelet 探测及 Pod 状态传播的等待；不能据此断言每次都固定多等 10 秒。
- 存储响应、节点执行、网络策略收敛和 API Exec 路径也可能造成差异。本次未采集逐请求阶段耗时，尚不能判断每个慢请求的主因。
- 后续方向：先量化 `pool_acquire`、`lease_acquire`、`prefix_prepare`、`mount_authorization`、`network`、`pod_ready`、`mounter_status`、`propagation_probe`、`publish`；评估用可信真实挂载就绪信号减少重复等待，不跳过 UID/generation、租约、挂载身份及真实传播校验，不以高频探测换取无边界控制面开销。
- 验收：库存充足和库存耗尽分开，独占和并发负载分开；普通/FUSE 创建、销毁、错误率及阶段耗时一起记录，并覆盖慢后端、挂载失败、失效租约和多副本竞争。持续性能、长尾以及 FUSE flush/unmount 的后端相关长尾尚未完成专项验收。

依据：`internal/sandbox/manager.go`、`internal/runtime/kubernetes/runtime.go`、`internal/runtime/kubernetes/pod.go`、`internal/telemetry/metrics/metrics.go`。

## 其它尚未关闭的事项

| 编号 | 类型与当前证据 | 后续关闭条件 |
| --- | --- | --- |
| STATE-01 | 历史状态待归属审计：Redis 仍有一个历史 sync workspace owner 和另一个普通池 scope 的三条旧 prepared 记录；当前 namespace 没有对应 Pod。不是本轮 v0.3.22 测试产生的遗留。 | 核实后端、namespace/release、runtime UID、活跃记录、租约及最终同步证据，再走受保护恢复；不得批量裸删。 |
| SEC-01 | 生产安全验收缺口：线上 `SANDBOX_WORKSPACE_ALLOW_MISSING_LSM_FOR_KIND=true`，仍允许缺少 AppArmor/SELinux。当前功能测试不能证明生产 LSM 强制执行。 | 在具备真实 LSM 的目标环境关闭绕过配置，验证正常挂载、隔离负例和失配失败。不是直接在现环境翻开关。 |
| HA-01 | 部署可用性限制：Redis 为单 Pod standalone / best_effort / requireHA=false。按此前选择可用于当前阶段，但三个 API 不意味着状态存储已高可用。 | 根据生产可用性要求确定 Redis 拓扑及持久化策略，完成实际故障切换、ACK、不确定写入和恢复验收。尚未执行，不记为产品缺陷已复现。 |
| NET-01 | 跨 CNI 现场验收缺口：Calico 和双华云/VPC 尚未接入真实测试；当前 Cilium 的通过不能代表其它数据平面。 | 对目标环境验证真实分配范围、Service 放行/撤销、未许可私网及 metadata 阻断、跨 namespace selector 和双栈边界；VPC 无可靠自动发现时明确配置完整范围。 |
| FAULT-01 | 故障验收缺口：API 重启、网络分区、Redis 真正切换，以及运行时契约变化触发旧池退休时的删除预算中断路径尚未完成现场故障注入。 | 配置有界故障驱动，验证失效 owner 接管、旧 Pod 消失后精确策略收尾、新池可领取、写入不确定时 fail closed。已有本地回归不代替现场结果。 |
| OBS-01 | 观测缺口：线上 tracer/metrics 的 OTLP enabled 都为 false。代码已有阶段指标，但本次部署没有通过这两个 exporter 输出，缺少逐请求慢阶段证据。 | 在后续性能专项接入观测或有界测试驱动；保留低基数指标、脱敏和开销约束。不是要求现在修改配置。 |
| LOAD-01 | 容量验收缺口：最新矩阵压力为 256 小文件、16 MiB 上传；创建采样量有限，没有持续负载 SLO、完整分位数或目标大规模文件集验收。 | 在隔离且容量可控的环境按目标规模压测，记录错误率、资源、池耗尽/补池、p50/p95/p99 及最终清理，不把小样本称为完整容量验收。 |
| QUALITY-01 | 已确认的仓库质量欠账：本次重新运行全仓 golangci-lint，退出 1，仍有 83 项：errcheck 50、ineffassign 3、staticcheck 19、unused 11。 | 按类别整改并补相关回归，最后全仓 lint 通过；增量 lint 为零不等于这些存量已关闭。不能将所有 lint 项都认定为线上故障。 |

### STATE-01 的精确线索

- 历史 sync owner：`sandbox-8n2th3nl1q`，runtime `sandbox-pool-q9aqkmmqfx`，UID `dc230072-e992-41db-bed1-017dd00187b0`，generation 1，prefix `workspaces/validation/api-1789270566488168000/dynamic/`。本次 workspace lease 扫描为空，但没有据此删除 owner。
- 其它普通 scope：`97358698362833fd`，pool key `4b975abc5c6ffe5836c388e46166b8a1`。三条 prepared 记录分别绑定 `sandbox-pool-ovykiqgexb`、`sandbox-pool-kdjqn8tta3`、`sandbox-pool-fsxdpy3odp`，更新时间为 `2026-09-12T13:08:xxZ`，本次只读核对仍存在。
- 当前代码的 workspace owner 安全恢复在 Kubernetes namespace 归属无法证明时返回 `runtime_scope_unconfirmed`，故不能承诺上述历史 owner 因当前 namespace Pod NotFound 就自动清除。当前 namespace 缺失不证明其它 namespace/集群也缺失。
- 旧报告提到的历史孤儿 NetworkPolicy 当前未出现在 namespace 清单中；不把它继续列为当前存在的策略遗留，也不推断其具体删除来源。

## 不再当作当前未修复故障的项目

以 v0.3.22 实际执行的套件为依据：跨副本动态挂载/同步/卸载、FUSE flush 共享状态、Python/Node stdin、指定内存/CPU 的真实资源限额、缺失/空文件下载、Service 目标白名单、one-shot SSE 收尾、普通/FUSE prepared UID 复用，以及普通 sync finalizer 自抢占造成的 stale-token 503，均已有通过记录。正常测试精确 Pod/策略/对应 Redis 收尾也已通过。

普通池 Pod 已消失后的策略收尾代码缺口已修复，并有本地回归；其跨代预算中断的现场故障验收仍在 FAULT-01，不能混写成“代码未修”或“现场已全部通过”。历史 owner/其它 scope 记录留在 STATE-01，不以最新测试无遗留替代安全归属审计。

## 本次记录验证

- 只读检查 Pod、NetworkPolicy/CiliumNetworkPolicy、三副本部署和非敏感配置字段；Redis 使用 SCAN/GET，不打印 token、密钥或凭据，不使用 DEL/KEYS。
- 全仓静态检查：`golangci-lint run --timeout=3m --output.text.path=stdout --output.text.print-issued-lines=false --output.text.print-linter-name=true`，结果为上述 83 项、退出 1。
- 本次只改文档，未重新运行部署 API 套件或声称完成新的功能验收。
