# Kubernetes 插件无关的池网络身份整改

用户已确认根治方案，并要求覆盖 Cilium、Calico、云 VPC 等 Kubernetes 网络环境。

## 设计与验收

普通池 Pod 的安全标签、NetworkPolicy 选择器和网络身份在创建后保持稳定。领取使用 UID/resourceVersion 保护的注解发布业务 sandbox ID，runtime 返回兼容的业务标签视图；实际 Pod 不改标签，不换策略选择器，不重新创建池策略。prepared 状态同样以 UID 绑定注解发布，避免准备完成时再次重建 CNI 身份。旧 Pod 可读取，旧领取失败不能隐式解释为成功；claim 不可重入到其他 ID。

网络规则继续以标准 Kubernetes NetworkPolicy 为通用基线，Cilium deny 仅在其能力存在时使用。配置了完整 pod_cidrs/service_cidrs 时直接校验；否则采用有界权威信息发现：通用 Node/ServiceCIDR、始终核对 CiliumNode、Calico IPPool。信息缺失、读取被拒绝、双栈不完整或未知 VPC 分配信息不能满足完整性时，拒绝字面目标更新并要求管理员显式配置，不扫描全量 Pod/Service 推测网段。

独立复审后闭合两个边界：通过 `network_policy_provider=auto|standard|cilium` 显式选择策略提供方，遗留 Cilium API 的非 Cilium 集群使用 standard，不等待 Endpoint；API 可用性另行保留用于自有旧策略清理，404 表示不存在可选资源而非阻塞。Manager 通过 exact OrdinaryPoolClaimer 使用 Acquire 的原始 UID，失败清理同样 exact。最终业务 ID 唯一性由共享 active repository NX 发布保证，不新增运行时 claim 对象或锁。

稳定身份消除本次领取造成的标签收敛窗口，不承诺标准 NetworkPolicy API 未提供的跨 CNI 数据平面生效通知。通用部署必须实际启用 NetworkPolicy 执行；不支持策略执行的 VPC/CNI 不作为已满足沙盒隔离的环境。Cilium 普通预备池发布前进行 Endpoint 专属身份确认，不让其他插件依赖 Cilium CRD；读取权限仅授予沙盒 namespace。

回归覆盖：Node/Cilium 网段冲突、Calico 网段与 Node 不一致、无分配证据、ServiceCIDR 不可用、双栈、显式 VPC 网段与零 inventory 快路径；普通池领取前后 Pod 标签/选择器完全相同，正常/丢响应/冲突/不同 UID/不同 owner、业务视图和清理恢复。全包、race、build、vet、lint 和 Helm 校验后再由用户构建部署，复测线上完整 API 和延迟，不以旧镜像验证新代码。

## 范围边界

不修改集群 CNI 配置，不回滚、不升级业务部署，不更改 Redis 拓扑或绕过 LSM。普通 sync 销毁长尾单独观察，不以强制删 Pod 或缩短安全证据链优化。未提供 Calico/VPC 实集群时，其代码矩阵可验证，但不得宣称这些环境的实际网络隔离验收通过。
