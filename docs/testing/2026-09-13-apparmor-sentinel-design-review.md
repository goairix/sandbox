# AppArmor / 内置 Sentinel 规格自审

## 范围与结论

用户要求先自审。此次核对两份已提交规格与当前 runtime、mounter、Redis client 和 Chart，查阅 Kubernetes/Redis/containerd 官方资料，直接修正规格；未进入功能实现、未操作线上部署或 Redis。

初稿存在需要修正的安全、初始化和可用性定义，不能直接据此实施。以下问题已在规格层面修正；这是设计修正，不是功能已实现或测试已通过。

## 发现及修正

| 编号 | 初稿问题 | 修正与对应验收 |
| --- | --- | --- |
| A1 | 按含最终 profile 名称的“完整内容”求名称哈希会产生循环定义，外部 include 也可能使相同名称对应不同策略。 | 对固定名称占位符的自包含模板求哈希，再替换；所有有效 LSM 使用点同源，测 profile/API/drain/production/fingerprint 一致。 |
| A2 | DaemonSet Ready 不证明 privileged mounter 真正受约束；初稿把检查主要放在人工验收。 | runtime 在 prepared 发布及授权前校验 actual exact profile/enforce 和 UID/零重启；忽略策略的运行时 fail closed，私有读取不向公共接口开放。 |
| A3 | 全节点 Ready 门禁会让单个故障节点阻止所有新 API 启动；空 nodeSelector 也不自动保证双方仅选 Linux。 | Linux 合入双方同一个有效选择器；启动要求当前策略非零可用容量，不做常驻全节点依赖，安全性由每个 mounter 的实际检查保障；完整覆盖仍单独验收。 |
| A4 | 加载器权限、token 和 profile 逃逸路径定义不够明确。 | 明确可信 privileged/securityfs 权限及 PSA 前提，禁止宿主机根/socket/hostPID/Node 写；无 API token，子进程继承策略，不允许 MAC_ADMIN/MAC_OVERRIDE/任意切换。 |
| R1 | Sentinel 首次发现依赖 Redis Pub/Sub，先要求 Sentinel 完整互相发现再启动 Redis 可能死锁。 | 用显式一次性新集群引导阶段启动 Redis/复制，再验证 Sentinel；Parallel 与未 Ready DNS 同时覆盖，初始化 Job 不使用 pre-install 等待未创建对象。 |
| R2 | 两个同地址/epoch 回复不是选主授权；两份空状态可能“投票”回旧 0 号，冷启动分歧没有定义。 | 初始化 clusterID/阶段与 PVC 绑定；初始化关闭后禁止默认重新引导，晋升由 Sentinel 负责，空状态只加入已有组，状态多数丢失/分歧时人工恢复。不能承诺任意冷停机都自动无损恢复。 |
| R3 | 认证漏掉 Sentinel 间链路，私有配置文件仍存在特殊字符注入风险。 | 覆盖 Redis 客户端/复制、Sentinel→Redis、客户端→Sentinel、Sentinel→Sentinel；Secret 引用同源，配置语法转义、控制字符拒绝、真实特殊密码测试。 |
| R4 | 笼统“就绪”可能误用为 liveness，造成 quorum 失效时重启风暴；仅 WAIT 也可能 offset=0。 | 分开本地 liveness、启动预算及角色/复制/CKQUORUM readiness；初始接入同连接实际 barrier/WAIT，稳态探针不写业务数据。 |
| R5 | ACK 数量任意配置会破坏单节点故障容忍；DNS 声明不足；uninstall 保留卷与初始化状态有歧义。 | 内置固定 replica_ack/一个副本确认；显式 hostname resolve/announce 和副本地址；Sentinel 独立卷组，初始化记录随卷保留，旧单实例卷不自动复用。 |

## 证据与未关闭的实现门槛

- 当前 `BootstrapConfig/MounterStatus` 没有 AppArmor 实际状态证明，因此初稿不能依赖已有 health 自动完成 confinement 检查；实现必须补可信私有读回及回归，不能只渲染 annotation。
- 当前 `defaultNoExecuteTolerations` 与 DaemonSet 默认容忍范围不同，调度集合还要验证 taint/selector 边界；不能把 DaemonSet desiredNumberScheduled 当作全部 runtime 可调度节点的精确集合证明。
- DaemonSet 汇总 Ready 数可能包含旧模板实例；启动门禁须复用 namespace Pod 读取权限，核对当前模板和 controller owner UID，不能仅凭总数确认新策略就绪。
- Redis `UniversalOptions` 接入尚无独立 Sentinel 认证字段；初始化阶段及精确 ConfigMap 权限同样尚未实现。
- 初始化 Job 无 PVC 读取权限，共享密码也不是卷身份凭据；本地标记验证结果的可信传输协议、防重放和部分初始化续跑须在实施计划明确并实验验证，不能把规格要求当作现成机制。
- 全组正常冷恢复、投票 epoch/配置 epoch 分歧和初始化中断是实施前必须用真实隔离实例验证的阻断项；若可行性实验失败，应调整恢复方案，不削弱判断或把 SKIP 记作通过。
- profile 的语法、镜像路径和真实 FUSE mount/unmount 拒绝边界须做 Linux 实验。普通沙盒已有默认 enforce 不证明 mounter 的自定义策略有效。

主要依据：[Redis Sentinel 的授权与配置传播](https://redis.io/docs/latest/operate/oss_and_stack/management/sentinel/)、[WAIT 的保证边界](https://redis.io/docs/latest/commands/wait/)、[Kubernetes StatefulSet](https://kubernetes.io/docs/concepts/workloads/controllers/statefulset/)、[Kubernetes AppArmor](https://kubernetes.io/docs/tutorials/security/apparmor/)、[containerd 2.2.0 Localhost 处理](https://github.com/containerd/containerd/blob/v2.2.0/internal/cri/sputil/apparmor_linux.go)。

## 本次验证

已完整重读两份修正规格，检查默认兼容、scope、命名/哈希、权限、故障行为、初始化循环和验收对应关系；使用 git diff --check 检查文档补丁。没有宣称运行时实现通过、本地 HA 实验通过或线上 enforcement/故障切换通过。
