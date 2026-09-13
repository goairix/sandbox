# Kubernetes 跨网络插件整改结果（待新镜像部署验收）

## 范围与结论

本次针对 v0.3.19 线上验证发现的两个问题整改：真实 Cilium 分配范围被旧 Node PodCIDR 掩盖，以及普通池领取修改标签触发网络身份重算。实现同时覆盖标准 NetworkPolicy、Calico Kubernetes datastore/IPAM 与显式云 VPC 分配范围，不把其它 CNI 绑定到 Cilium CRD。

此前线上现象和样本保留在 `2026-09-13-ds-ai-research-v0.3.19-validation.md`。该旧镜像报告不是新实现已在线通过的证据。

## 实现

- 普通池 Pod 的全部物理标签和策略 selector 在 prepared 发布、业务领取时保持不变。业务 ID 使用 claim ID/Pod UID 注解发布，prepared 也使用 UID 绑定注解。API/runtime 仍返回兼容业务标签视图，已领取 Pod 不再计入空闲容量。
- 领取保留原策略，使用 UID/resourceVersion CAS 和精确读回来处理丢响应；不同 owner、不同 UID、标签变化不解释为成功。清理继续使用物理身份及精确运行时 UID，不增加 API 间 HTTP 转发。
- Cilium 始终读取 CiliumNode 分配，而不是仅在 Node CIDR 为空时读取；Calico 读取全部 IPPool，包含禁用池及可跨节点借用的分配范围；标准 Node IPAM 读取 Node 和 ServiceCIDR。完整证据缺失、403、超时、分页或地址族不完整均 fail closed。
- 云 VPC/外部 IPAM 或其它无法可靠发现分配范围的环境使用现有完整 `podCIDRs` + `serviceCIDRs` 配置。此快路径不读取库存；配置必须覆盖当前和未来分配。Node CIDR 非空但与实际 IPAM 无关不能被自动识别，不能承诺任意 CNI 自动发现。
- Cilium 普通预备池发布前检查同一 Pod UID 的 Endpoint owner/ready/身份标签，并处理等待期间状态更新造成的版本变化；恢复 preparing 和孤儿 Pod 也必须完成 prepared 发布后才可进入可领取记录。其它 CNI 不查询 Cilium Endpoint。
- 稳态 refill 按当前池的共享记录检查精确 runtime，未绑定准备项按池 key/instance 查询，不扫描全部活动业务沙盒。启动保留孤儿恢复，存在旧指纹时保留退休流程；没有旧记录时不做退休库存扫描。
- warm-pool contract 升至 v3，正常更换预备池，不强制删除活动业务 Pod，不弱化销毁确认或 FUSE 证据链。

## 配置及权限

新增 `config.runtime.kubernetes.networkPolicyProvider`（auto/standard/cilium）。默认 auto 兼容按 Cilium API 可见性发现；生产建议按实际插件显式选择，遗留 Cilium CRD 的 Calico/VPC 集群必须选择 standard。当前 ds-ai-research 使用 Cilium，可显式设置 cilium。现有 `podCIDRs` / `serviceCIDRs` 和原生配置注释均已补齐跨 CNI 中文说明。

独立复审发现并整改了两个 Important：acquired UID 未绑定 name-only claim，以及 Cilium API 存在被错误视为 Endpoint provider 启用。增加 exact OrdinaryPoolClaimer/失败清理和显式 provider；对应同名替换、原始 RuntimeRef、遗留 Cilium API 回归通过。业务 ID 的最终唯一性仍由共享 active repository NX 保证，不新增热点锁或 Kubernetes claim 对象。

最终独立只读复核未发现剩余 Critical/Important。API availability 与 provider 分离后，standard 不等待 Endpoint，但仍可在既有清理流程中处理精确归属的旧 CNP；可选策略资源不存在的 404 不阻塞恢复。

必须同步新 chart：ClusterRole 增加 Calico IPPool 的只读 list；Cilium Endpoint 的 get 仅加入 runtime namespace 的 Role。无新增全局 Pod/Service List、全局 Endpoint get 或写权限。

所有部署仍须具备实际 NetworkPolicy 执行能力。仅有 CRD 或策略创建成功不证明真实流量受控；标准 NetworkPolicy 没有统一数据平面生效回执，Cilium Endpoint 身份就绪也不保证所有后续策略已收敛。

## 本地验证

两个独立临时 Redis 仅绑定本机 16381/16382；各套件内部 `-p 1` 顺序测试，避免共享全局索引相互影响。最终命令结果：

- `TEST_REDIS_ADDR=127.0.0.1:16381 go test -p 1 ./... -count=1 -timeout=120s`：通过。
- `TEST_REDIS_ADDR=127.0.0.1:16382 go test -race -p 1 ./... -count=1 -timeout=180s`：通过。
- `go build ./...` / `go vet ./...`：通过。
- `golangci-lint run --new-from-rev=HEAD`：0 issues。
- `bash scripts/test-helm-chart.sh` / `bash scripts/test-helm-backend-switch.sh`：通过；namespace Endpoint 最小权限和 Calico 库存权限有渲染断言。
- `go test ./test/integration/helm -count=1` / `git diff --check`：通过。

全仓 `golangci-lint run` 仍报告 83 项（errcheck 50、ineffassign 3、staticcheck 19、unused 11）；本次新增检查为零。没有把全仓 lint 写成通过，也没有借网络整改修改无关历史代码。

新增回归经历红绿循环，包括真实网段遗漏、稳定物理标签/策略、prepared 发布、稳态补池不广扫、就绪等待期间版本变化、恢复不能绕过发布；并覆盖 Calico 缺证据/权限/无效 CIDR/分页、标准双栈和显式 VPC 零库存路径。修复了测试阻塞夹具重复关闭通道，以及共享 runtime 夹具缺失返回/标签快照契约，未改变生产清理语义。

部署 API 和实网络套件需显式 opt-in；本地 go test 的成功不表示这些未接入的真实环境已通过。临时 Redis 容器在验证后已停止并移除，未修改业务集群、业务 Redis 或 Helm release；镜像构建、推送和部署仍由用户完成。

## 新部署后的必验项

1. 三副本 API 重新验证普通/FUSE 生命周期、stdin、文件与下载、资源、工作区同步/持久化、流式执行、并发池复用和最终清理。
2. 三个新领取普通沙盒的 Pod UID、全部物理标签、策略 selector 前后不变；业务 ID 与 claim UID 注解对应，空闲容量不包含 claimed Pod。
3. 设置部署测试的内部 Service 目标及真实可达 backend URL，验证许可后 TCP、移除许可后阻断、恢复许可后可达；5 秒有界收敛不能用固定长 sleep 隐藏。
4. 设置 `SANDBOX_API_CLUSTER_LITERAL_TARGETS` 为实际 PodIP、ServiceIP/相应 CIDR，要求跨副本 HTTP 400 / `NETWORK_TARGET_INVALID`；同 namespace 非匹配 backend、其它 namespace 同标签 backend、metadata/link-local/未许可私网独立验证。
5. 采样创建和实际连接延迟，核对准备 UID 复用、无主动 CNI 身份重算；审计本次测试生命周期/锁/会话及 Pod/策略完整清理。
6. Calico 和云 VPC 必须在其真实环境重复网络验收；当前 ds-ai-research 是 Cilium，不能据此宣称其它 CNI 的真实隔离或高可用故障切换通过。

详见 `docs/kubernetes-network-targets.md`。新镜像尚未部署，因此线上整改验收未完成。
