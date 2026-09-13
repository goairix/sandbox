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

默认自动发现仅读取分配网段，不遍历整个 Pod/Service 库：Node PodCIDR/PodCIDRs（同时排除 Node 已知 IP）、`networking.k8s.io/v1 ServiceCIDR`，以及在 Cilium Node 缺少 PodCIDRs 时的 `cilium.io/v2 CiliumNode.spec.ipam.podCIDRs`。Cilium 默认 cluster-pool IPAM 正是使用 CiliumNode 而非 Kubernetes Node 分配 PodCIDRs。每个 Kubernetes Node 都必须具有可验证的 Pod 分配网段，并覆盖 ServiceCIDR 所声明的地址族；只发现部分双栈分配数据也会拒绝，需要显式完整配置。缺少任一个 Node 的数据、没有 Node、ServiceCIDR API 不存在（404/405）、ServiceCIDR 为空或读取失败时，Cilium CIDR 白名单更新中止，不会根据 live PodIP/ServiceIP 猜测未分配地址的归属或返回成功。云 IPAM / multi-pool / Clustermesh 等不能以这些资源完整表达分配范围的环境，应使用下面的完整配置；不能只填写当前已使用 IP。非 Cilium 的旧 API 保持 NetworkPolicy 兼容路径，但缺失分配 API 无法提供同等级的完整网段验证。

每次自动发现是请求时的新快照，没有 permissive 缓存。全部目标解析/分配库存读取共享最长 5 秒上下文；每种库存最多 10 页、每页请求 100 项、最多 1000 对象、总计最多 4096 个网段。分页未结束、超限、continue 过期、权限错误或超时一律中止而非截断。典型单栈 Kubernetes Node IPAM 为两次 List（Nodes + ServiceCIDRs）；缺少 Node PodCIDR 时额外 List CiliumNodes。显式 Service 目标每项一次 Get；只有 Service 目标的白名单不需要分配网段 List。库存快照后的新网段分配必须通过网络地址管理约束保证不会把已批准的外部地址重新分配给集群。

推荐生产部署配置完整的当前 **及将来分配** 地址范围，避免每次变更读取库存：

```yaml
config:
  runtime:
    kubernetes:
      podCIDRs: ["10.42.0.0/16", "fd00:42::/64"]
      serviceCIDRs: ["10.96.0.0/12", "fd00:96::/112"]
```

原生配置键为 `runtime.kubernetes.pod_cidrs` / `runtime.kubernetes.service_cidrs`，环境变量为 `SANDBOX_RUNTIME_KUBERNETES_POD_CIDRS` / `SANDBOX_RUNTIME_KUBERNETES_SERVICE_CIDRS`（逗号分隔）。两个集合必须同时配置或同时留空，且包含完全匹配的 IPv4/IPv6 地址族；每组最多 256 个 canonical CIDR。部分双栈配置在 Helm 渲染、配置加载和 runtime 快速路径中均会被拒绝，不会因零库存读取而绕过检查。配置路径是权威运营声明，不是自动发现结果的缓存：部署方须保证这些 CIDR 覆盖所有受管理 Pod 和 Service 的当前及未来分配（含已启用地址族/额外集群）；扩容新增网段前须先更新配置并滚动所有 API 实例。程序不会静默补齐不完整配置；完整配置使 CIDR 检查的库存 List 次数为零，仍会拒绝与未来分配范围相交的 CIDR，并保留 5 秒/取消检查。示例网段仅作说明，必须替换成真实地址管理范围。

Helm 新增只读 `ClusterRole`/`ClusterRoleBinding`，绑定 API ServiceAccount，可 `get` Services，并 `list` Nodes、ServiceCIDRs、CiliumNodes。不新增 cluster-wide Pod/Service List 权限，也不新增写权限。需要 cluster-wide 范围是因为目标可跨 namespace，且分配网段属于集群级对象。升级时先应用 RBAC，再发送网络更新。

没有新增 `toEntities: all/cluster`、空 Pod/namespace selector，或修改 Cilium 全局 `CIDRMatchMode`。Cilium deny 优先级不变：外部私网白名单与私网 deny 的较小交集作为具体 CIDR exception；白名单 CIDR 完全覆盖某个私网 deny 时仅省略该私网 deny，永久禁止范围始终保留，覆盖集群权威网段的白名单仍被拒绝。Service endpoint selector 不产生私网 CIDR exception。FUSE 不可变 system-egress 策略、DNS/存储许可集及其端口保持原样，用户 Service 目标只加入 user-egress 策略。`enabled=false` 时不安装用户 Service 放行规则。普通沙箱创建/更新的 allow 和 deny 策略共用一个 attempt，并最终联合读回验证策略 immutable UID、runtime Pod UID、selector/spec 与 attempt（不需要 deny 时验证其确实不存在）。分步写入不是 Kubernetes 跨对象事务，任何不一致均返回 uncertain，并由现有 fail-closed/隔离流程处理，不提交共享网络配置。

上线后仍须在实际 Cilium 集群验证：指定 Service backend 可达、同 namespace 非匹配 Pod 与其他 namespace 同标签 Pod 不可达、metadata/link-local 与未白名单私网不可达、FUSE 存储和禁网状态正常。单元测试不能证明真实 CNI 转发或集群连通性。

依据：[Cilium v1.18.4 官方策略定义](https://github.com/cilium/cilium/blob/v1.18.4/Documentation/security/policy/language.rst)（Endpoints / Services / CIDR 与 deny priority）；[Cilium v1.18.4 cluster-pool IPAM](https://github.com/cilium/cilium/blob/v1.18.4/Documentation/network/concepts/ipam/cluster-pool.rst)；[Kubernetes 官方 NetworkPolicy selector 语义](https://kubernetes.io/docs/concepts/services-networking/network-policies/)。
