# Kubernetes 网络白名单与集群内 Service

`network.whitelist` / 更新网络请求的 `whitelist` 仍是字符串数组。外部域名、IP 和 CIDR 继续受支持；域名按更新时解析结果生成 CIDR，并非动态 DNS 策略。

集群内访问必须使用显式目标，例如：

```json
{"enabled":true,"block_private":true,"whitelist":["api.example.com","k8s-service://payments/ledger"]}
```

该语法仅适用于 Kubernetes runtime。namespace 和 name 都必须是 DNS label；不支持端口、通配符、查询参数或额外路径。白名单最多 256 个目标。Service 必须已经存在，并且是非 headless、非 ExternalName、具有非空 selector 的 Service。无 selector 的 Service（包括 `default/kubernetes`）不支持，不会退化为 EndpointSlice IP 或 namespace-wide 允许。

每个目标编译为同一个 NetworkPolicy peer 内的 **精确 namespace + 完整 Service selector**。Cilium 使用 Pod endpoint identity 执行该规则；标准 NetworkPolicy 也使用相同的 selector 语义。它允许该 namespace 内所有匹配 selector 的 backend Pod（所有端口，包括直接 Pod 访问），不是只允许 Service VIP、Service 暴露端口或某一个固定 Pod。backend Pod 的重建/IP 变化不需要重新配置；Service selector 被修改后必须重新提交网络更新，现有策略不会自动追踪 selector 修改。不要给无关 Pod 复用这个 selector。hostNetwork backend、跨集群 Service、无 selector Service 和 API server 访问不在此功能的支持范围内。接收方 ingress 策略仍可能拒绝连接。

## 兼容性与迁移

Cilium v1.18.4 的 CIDR/ipBlock 规则不能选择受管理的 Pod endpoint；把 PodIP 加入白名单并不等于真正放行。现在，用户 IP/CIDR（也包括域名解析出的 CIDR）只要与可发现的集群 Pod/Service 地址或网段相交，就会被拒绝，API 返回 HTTP 400，错误码 `NETWORK_TARGET_INVALID`。例如 `10.0.0.0/8` 不能用来绕过这一检查。请将原先的集群 ServiceIP、backend PodIP 或覆盖它们的 CIDR 改成 `k8s-service://namespace/name`。外部私网地址仍可显式白名单，但不得覆盖集群地址/网段或永久禁止的 loopback、link-local、multicast 等范围。

默认自动发现仅读取分配网段，不遍历整个 Pod/Service 库。所有 CNI 都要求完整证据，按实际 IPAM 来源处理：

使用 `config.runtime.kubernetes.networkPolicyProvider` 选择策略执行提供方（原生 `runtime.kubernetes.network_policy_provider`，环境变量 `SANDBOX_RUNTIME_KUBERNETES_NETWORK_POLICY_PROVIDER`）：`standard` 使用标准 NetworkPolicy，适用于 Calico 或其它具备策略执行能力的 CNI/VPC；`cilium` 要求 Cilium API 并启用其 deny 与 Endpoint 身份确认。默认 `auto` 保留按 Cilium API group 可见性发现的兼容行为，不能证明实际插件仍启用；集群遗留 Cilium CRD 但使用其它 CNI 时必须显式选择 `standard`，不会等待不存在的 Cilium Endpoint。生产建议按实际提供方显式设置。此选项不启用 CNI 的策略执行，也不替代真实网络验收。

策略提供方与可选 API 库存分开：standard 不构建 Cilium enforcement，也不查询 Endpoint；若 Cilium API 仍存在，会在原有恢复/销毁流程中清理精确归属的旧 CiliumNetworkPolicy，而不是将它们隐藏成“零残留”。缺失可选策略资源的 404 可忽略，权限/传输错误不能当作已清理。此配置用于部署适配，不代表支持活动沙盒存续期间任意切换集群 CNI。

- Cilium：始终读取 `cilium.io/v2 CiliumNode.spec.ipam.podCIDRs`，每个 Kubernetes Node 都必须有对应分配证据。不能因为 Kubernetes Node 的 PodCIDR 非空就跳过；该值可能与实际 Cilium 分配不同。
- Calico Kubernetes datastore/IPAM：读取 `crd.projectcalico.org/v1 IPPool.spec.cidr` 的全部池，包括禁用池。Calico 可跨 Node 借用地址块，禁用池也可能仍有已分配的 Pod，因此不能只看 Node PodCIDR。资源存在但库存为空、无效或不可读时拒绝字面目标更新。
- 其它 CNI：Calico IPPool API 不存在时，使用 Node PodCIDR/PodCIDRs；部署方必须确认 Node 是真实 Pod IP 分配来源。云 VPC、外部/etcd IPAM、Cilium multi-pool、Clustermesh 等无法用这些资源完整表达的环境，必须显式配置完整范围。非空但与实际 IPAM 无关的 Node CIDR 无法由程序自动判定。

以上路径均读取 `networking.k8s.io/v1 ServiceCIDR`，并保守排除 Node 已知 IP 和 Node 声明网段。没有 Node、任一 Node 缺少分配证据、ServiceCIDR API 不存在（404/405）、库存为空、地址族不完整或读取失败时，IP/CIDR/域名白名单更新中止，不会根据 live PodIP/ServiceIP 猜测完整范围。已知 Calico 池覆盖全局分配，不要求 Node 另外声明 PodCIDR。仅使用 Service selector 目标的请求不依赖 CIDR 库存。

每次自动发现是请求时的新快照，没有 permissive 缓存。全部目标解析/分配库存读取共享最长 5 秒上下文；每种库存最多 10 页、每页请求 100 项、最多 1000 对象、总计最多 4096 个网段。分页未结束、超限、continue 过期、权限错误或超时一律中止而非截断。自动路径读取 Nodes + ServiceCIDRs，以及 CiliumNodes 或 Calico IPPools（其它 CNI 的 Calico API 首次 404 后使用 Node 数据）。显式 Service 目标每项一次 Get。库存快照后的新网段分配必须通过网络地址管理约束保证不会把已批准的外部地址重新分配给集群。

推荐生产部署配置完整的当前 **及将来分配** 地址范围，避免每次变更读取库存：

```yaml
config:
  runtime:
    kubernetes:
      podCIDRs: ["10.42.0.0/16", "fd00:42::/64"]
      serviceCIDRs: ["10.96.0.0/12", "fd00:96::/112"]
```

原生配置键为 `runtime.kubernetes.pod_cidrs` / `runtime.kubernetes.service_cidrs`，环境变量为 `SANDBOX_RUNTIME_KUBERNETES_POD_CIDRS` / `SANDBOX_RUNTIME_KUBERNETES_SERVICE_CIDRS`（逗号分隔）。两个集合必须同时配置或同时留空，且包含完全匹配的 IPv4/IPv6 地址族；每组最多 256 个 canonical CIDR。部分双栈配置在 Helm 渲染、配置加载和 runtime 快速路径中均会被拒绝，不会因零库存读取而绕过检查。配置路径是权威运营声明，不是自动发现结果的缓存：部署方须保证这些 CIDR 覆盖所有受管理 Pod 和 Service 的当前及未来分配（含已启用地址族/额外集群）；扩容新增网段前须先更新配置并滚动所有 API 实例。程序不会静默补齐不完整配置；完整配置使 CIDR 检查的库存 List 次数为零，仍会拒绝与未来分配范围相交的 CIDR，并保留 5 秒/取消检查。示例网段仅作说明，必须替换成真实地址管理范围。

Helm 只读 `ClusterRole`/`ClusterRoleBinding` 绑定 API ServiceAccount，可 `get` Services，并 `list` Nodes、ServiceCIDRs、CiliumNodes、Calico IPPools。不新增 cluster-wide Pod/Service List 权限，也不新增写权限。CiliumEndpoints 的 `get` 权限仅加入沙盒 namespace 的 runtime `Role`，不授予全局 Endpoint 读取权限；身份确认只用于 Cilium 普通预备池，其它插件不会查询该资源。升级时须同步新 chart/RBAC，再发送网络更新。

没有新增 `toEntities: all/cluster`、空 Pod/namespace selector，或修改 Cilium 全局 `CIDRMatchMode`。Cilium deny 优先级不变：外部私网白名单与私网 deny 的较小交集作为具体 CIDR exception；白名单 CIDR 完全覆盖某个私网 deny 时仅省略该私网 deny，永久禁止范围始终保留，覆盖集群权威网段的白名单仍被拒绝。Service endpoint selector 不产生私网 CIDR exception。FUSE 不可变 system-egress 策略、DNS/存储许可集及其端口保持原样，用户 Service 目标只加入 user-egress 策略。`enabled=false` 时不安装用户 Service 放行规则。普通沙箱创建/更新的 allow 和 deny 策略共用一个 attempt，并最终联合读回验证策略 immutable UID、runtime Pod UID、selector/spec 与 attempt（不需要 deny 时验证其确实不存在）。分步写入不是 Kubernetes 跨对象事务，任何不一致均返回 uncertain，并由现有 fail-closed/隔离流程处理，不提交共享网络配置。

## 普通池的稳定网络身份

普通池领取和准备完成不再修改物理 Pod 标签，也不更换 NetworkPolicy selector。`sandbox.id` 仍是物理池身份，`sandbox.pool=true` / pool key / instance / 初始 state 标签保持不变；业务状态由 UID 绑定注解发布：`sandbox.claim.id` + `sandbox.claim.uid` 表示领取，`sandbox.pool.state=prepared` + `sandbox.pool.runtime.uid` 注解表示准备完成。注意 annotation 和 label 是不同字段。已领取 Pod 不应再被当作空闲池容量。

API/runtime 返回的标签视图仍使用业务 sandbox ID，并移除已领取 Pod 的池成员字段；多副本继续使用共享记录和精确 runtime UID，不依赖 API 间 HTTP 转发。补池仅检查当前池记录的运行时；启动保留孤儿恢复，升级保留旧预备池退休。模板契约升级会更换预备池，不销毁活动业务沙盒。

Manager 通过 exact `OrdinaryPoolClaimer` 传入 Acquire 返回的原始 Pod UID，失败清理也使用同一 RuntimeRef，不能领取/删除随后出现的同名替换 Pod。最终业务 ID 唯一性由共享 active repository 的原子 NX 发布保证；物理标签查询仅检查旧身份冲突，不作为注解业务 ID 的并发唯一性屏障。

## 实际网络验收

每种 CNI 部署都必须启用 NetworkPolicy 数据平面执行。仅安装 CRD 或成功写入策略对象不等于真实流量已受控；无策略执行的云 VPC/CNI 不能满足沙盒隔离要求。标准 NetworkPolicy 没有统一生效回执，Cilium Endpoint ready 也仅确认精确 Pod 的网络身份，不代表所有后续策略更新已生效。稳定身份消除本次领取触发的身份重算，但不承诺跨插件零策略收敛延迟。

上线后须在实际 Cilium、Calico 或对应云 VPC 环境分别验证：指定 Service backend 可达、同 namespace 非匹配 Pod 与其他 namespace 同标签 Pod 不可达、实际 PodIP/ServiceIP 字面白名单被拒绝、metadata/link-local 与未白名单私网不可达、删除许可后不再可达、FUSE 存储和禁网状态正常；跨三个 API 副本验证并核对准备/领取前后同一 Pod UID 和标签。单元测试不能证明真实 CNI 转发或集群连通性。

部署测试套件 `test/integration/api/remediation_test.go` 可通过 `SANDBOX_API_TEST_URLS` 注入三个独立副本地址。网络验证需设置 `SANDBOX_API_INTERNAL_TARGET=k8s-service://namespace/name` 和 `SANDBOX_API_INTERNAL_URL`（已知可达 backend 的 HTTP URL，推荐使用字面 ServiceIP，避免 DNS 故障混淆拒绝判断）；`SANDBOX_API_CLUSTER_LITERAL_TARGETS` 逗号分隔填写真实 PodIP、ServiceIP 或对应 CIDR。套件对三个新领取的普通沙盒分别验证许可、字面目标 400 拒绝、删除许可后阻断、重新许可后可达；连接收敛有 5 秒上界，失败不能通过固定长 sleep 隐藏。`SANDBOX_API_DENIED_URL` 可补充其他拒绝目标，但需独立确认其服务可用；最终仍须结合 Pod UID/标签、策略及清理审计验收。

依据：[Cilium v1.18.4 官方策略定义](https://github.com/cilium/cilium/blob/v1.18.4/Documentation/security/policy/language.rst)；[Cilium cluster-pool IPAM](https://github.com/cilium/cilium/blob/v1.18.4/Documentation/network/concepts/ipam/cluster-pool.rst)；[Calico IPPool 与禁用池](https://docs.tigera.io/calico/latest/reference/resources/ippool)；[Calico IPAM 与跨节点地址分配](https://docs.tigera.io/calico/latest/networking/ipam/get-started-ip-addresses)；[Kubernetes NetworkPolicy 语义与生效边界](https://kubernetes.io/docs/concepts/services-networking/network-policies/)。
