# ds-ai-research v0.3.19 部署验收

## 结论

2026-09-13 对实际部署进行了三副本直连测试。普通与 FUSE 核心功能、跨副本工作空间控制、持久化、池复用及有界文件系统压力回归通过，但不能判定为全部验收通过：发现真实 Pod 网段识别遗漏，以及普通池领取后的 Cilium 身份收敛延迟。

本次仅创建和销毁测试沙盒，不升级、回滚、重启或缩容业务部署，不强制删除 Pod，不删除 Redis 业务状态。

## 环境与方法

- 集群：`ds-ai-research`；命名空间：`aiadp-sandbox-fuse`。
- API 镜像：`registry.i.huaxisy.com/library/ai-infra/sandbox-api:v0.3.19`。
- 三个 API 副本均为新 ReplicaSet，Ready，重启次数为 0。
- 分别通过 localhost 18081、18082、18083 转发到三个 API Pod，不依赖 Service 的随机负载均衡来推测跨副本覆盖。
- Redis：`standalone / best_effort / requireHA=false`。
- FUSE：`minio-sigv4-path-style-v1`。
- Pod/Service CIDR 配置留空，使用自动发现。
- `workspace.allow_missing_lsm_for_kind=true`；不能据此验收生产 AppArmor/SELinux 强制执行。
- 独立 UUID 测试前缀；已知文件在销毁前删除并同步删除。仅通过正常 API 销毁测试资源。

## 功能结果

| 项目 | 结果 | 证据与范围 |
| --- | --- | --- |
| 三副本 health / ready | 通过 | 六次 GET 均为 200 |
| 未认证请求 | 通过 | 三副本受保护接口均返回 401 |
| 普通沙盒跨副本执行 | 通过 | 在不同副本创建、执行、销毁 |
| Python / Node stdin | 通过 | 三副本正确读取 stdin |
| 动态挂载、双向同步、卸载 | 通过 | 操作分别落在不同副本；三个副本读取一致状态 |
| ephemeral / persistent × sync / FUSE | 通过 | 四种组合销毁后，以同前缀重新创建并读取到原始 UUID 内容 |
| sync / FUSE 前缀互斥 | 通过 | 两种申请顺序均为 409；销毁后可切换挂载模式 |
| FUSE flush 共享状态 | 通过 | 三副本均读到 flushed 与非空 last_flushed_at |
| 缺失文件 / 空文件下载 | 通过 | 普通和 FUSE，三副本分别返回 404 FILE_NOT_FOUND / 200 空内容 |
| 64 KiB 二进制上传下载 | 通过 | 三副本下载 SHA-256 一致 |
| 资源限额 | 通过 | 实际 cgroup 限额：128 MiB、0.1 CPU |
| 一次性 SSE 执行 | 通过 | 三副本输出 done，HTTP 响应完整结束 |
| 普通池三并发 | 通过 | 三请求创建、跨副本执行、销毁；全部复用原有 Pod UID |
| FUSE 池三并发 | 通过 | 同上；全部复用原有 Pod UID |
| FUSE 文件系统压力回归 | 通过 | 目录、覆盖、追加、截断、重命名、删除、Git、256 小文件、16 MiB 上传、flush、销毁 |
| 扩展文件接口 | 通过 | 普通和 FUSE 的读取、按行读取、字符串/按行编辑、目录/递归/glob 列举；跨副本核对内容 |
| 分片上传 | 通过 | 普通和 FUSE；跨副本上传、查询、合并、取消；合并内容与大小正确 |
| TTL 更新 | 通过 | 其他副本更新为 300 秒，三个副本读取一致；未专项验收 TTL 到期故障窗口 |
| 技能接口基础路径 | 通过 | 三副本列表 200、不存在技能 404；未验收自定义技能内容和附件正向路径 |
| 已有沙盒 Exec 流 | 通过 | 普通与 FUSE 返回正确 stdout 和 done，响应完整结束 |
| 普通一次性执行 | 通过 | 三副本 Python stdin，exit_code=0、输出一致 |
| Service 白名单与移除 | 最终可达/可阻断，但延迟不合格风险 | 初次放行约 24–32 秒后恢复；详见问题 2 |
| 集群 IP / CIDR 字面目标拒绝 | 未通过 | Service CIDR 拒绝正确，但实际 10.0.x.x Pod /32 被接受 |

最终清理审计见报告末尾。

## 性能与池复用

30 次轮流请求三个 API 的串行 FUSE 创建采样，30 成功、0 失败；计时只包含 HTTP 创建请求，不含之后的 kubectl UID 查询。

| 指标 | 结果 |
| --- | --- |
| p50 | 5.725 秒 |
| p95 | 9.873 秒 |
| p99 / 最大值 | 16.931 秒 |
| 与请求前预备池快照同名、同 UID 的运行时 | 29 / 30 |

采样期间还运行了少量专项与三并发回归，第 15 次未命中请求前的预备池快照。因此这是混合占用下的小样本，不是纯热池基准，也不能据此声称长期 p99 或容量 SLO 已达标。

普通三并发创建耗时为 0.563、0.586、2.489 秒；FUSE 为 4.854、5.903、9.451 秒。这六次计时含创建后的 kubectl 查询，仅用于回归观察，不能和上表纯 HTTP 时间直接比较。

普通 sync 工作空间销毁观察为 32.506、37.249 秒，FUSE 为 3.798、7.118 秒。均最终完成并可重新领取原前缀，但普通同步销毁长尾仍应作为性能优化项；不以最终成功掩盖延迟。

## 问题 1：自动发现遗漏 Cilium 实际 Pod 网段

实际环境同时存在两份不一致的分配信息：

- Kubernetes `Node.spec.PodCIDRs`：`10.244.x.0/24`。
- `CiliumNode.spec.ipam.podCIDRs`：`10.0.x.0/24`，实际 API / Redis / runtime Pod 均使用这一组。
- `cilium-config`：IPAM 为 `cluster-pool`，IPv4 分配池 `10.0.0.0/8`，IPv6 禁用。
- Kubernetes ServiceCIDR：`10.96.0.0/16`。

`internal/runtime/kubernetes/service_targets.go:265` 只有在 Node 缺少 PodCIDR 时才查询 CiliumNode。Node 有非空但不适用的旧网段时，Cilium 实际网段被漏掉。

在测试沙盒中提交 `enabled=true, block_private=true` 的字面目标：

| 白名单目标 | 期望 | 实际 |
| --- | --- | --- |
| `10.96.14.111/32`（API Service） | 400 | 400 NETWORK_TARGET_INVALID |
| `10.0.5.51/32`（API Pod） | 400 | 200 |
| `10.0.3.137/32`（API Pod） | 400 | 200 |
| `10.244.3.0/24` | 400 | 400 NETWORK_TARGET_INVALID |
| `10.0.0.0/8` | 400 | 400（同时覆盖已知网段，不能说明真实 Pod /32 识别正确） |
| `8.8.8.8` | 200 | 200 |

这是目标校验缺陷。未据此宣称已经证明 Cilium 的实际跨沙盒访问被绕过；本次没有用未授权目标进行连接攻击。

整改建议：在 Cilium 场景始终核对 Cilium 的权威分配信息，不把 Node 字段非空当成充分条件；增加 Node/Cilium 网段不一致、双栈、缺失和扩容边界的回归。当前集群可显式配置完整分配池规避遗漏，但配置变更仍由部署方执行，本次未修改线上 values。

## 问题 2：普通池领取后，接口返回先于网络身份生效

第一次新沙盒白名单测试在短重试窗口内失败。三次新沙盒复测最终均可连接 API/Redis Service，但 API 连通时间分别约 27.56、31.35、24.33 秒；Redis 随后可连接。三次移除 Redis Service 白名单均最终阻断。

单变量对照 `block_private=false → true → false` 全部最终连接成功。因此不能把首次阻断归因为永久性的私网 deny / Service allow 冲突。

额外采集到普通池身份变化的直接证据：

- 已领取沙盒：`sandbox-s75kb2abgm`，运行时 `sandbox-pool-57xxelawql`。
- 网络更新 HTTP 请求耗时 2.519 秒。
- 更新起约 6.13 秒：Cilium Endpoint 为 ready，但身份标签仍是 `sandbox.id=sandbox-pool-57xxelawql`，TCP 被阻断。
- 更新起约 7.76 秒：身份标签变为 `sandbox.id=sandbox-s75kb2abgm`，TCP 成功。

Pod Ready / Kubernetes policy 对象意图验证不等于 Cilium 身份与数据平面已收敛。普通池迁移修改 sandbox.id，而策略选择该标签，现有成功返回没有覆盖这个网络就绪窗口。上述直接时序样本确认了机制；它不代表全部 24–32 秒样本的精确阶段分解。

整改建议：设计领取和网络更新的明确就绪语义，加入有界身份/策略生效确认，或者采用不需要领取时变更安全身份的稳定选择器；继续保留 UID、generation 与 attempt 校验。避免仅加固定 sleep 或把超时伪装为成功；优化时同时记录冷/热池各阶段延迟。

## 执行命令与验收限制

已部署整改套件：`TestDeployedAPIRemediation`，三副本，FUSE 3 个功能采样，124.46 秒通过。

有界压力套件：`TestWorkspaceFUSE/FUSE_filesystem_stress`，256 小文件、16 MiB 上传，83.47 秒通过。故障注入用例由于没有配置故障驱动而 SKIP，不计入通过项。

额外专项由本地临时 Python 驱动执行：功能、网络对照、身份时序、30 次性能采样、精确 Redis 键审计；逐项检查结果，而非以进程 exit 0 代替所有用例通过。

本次没有验收：Redis 主从/Cluster 故障切换、网络分区、API 滚动重启期间的故障恢复、真实 LSM 强制执行、大规模持续负载、多个对象存储 profile、网络放行在 Cilium 数据平面的即时生效保障。不能把“三副本正常路径通过”写成“高可用故障验收通过”。

## 补测与最终清理

- 扩展接口补测 64.22 秒通过；普通和 FUSE 两个测试沙盒正常销毁。
- 25 个功能/网络专项沙盒：175 个精确生命周期、操作锁和会话键，`EXISTS` 总计为 0。
- 30 个性能采样沙盒：210 个同类精确键，`EXISTS` 总计为 0。
- 两个扩展接口沙盒：14 个同类精确键，`EXISTS` 总计为 0；最终 release 活动生命周期记录数为 0。上述三组共 57 个显式记录 ID、399 个精确键；不包含一次性执行内部 ID 的逐键计数。
- 驱动逐一检查测试运行时的原始 Pod UID，全部不存在；不能用仅 API 返回 404 代替运行时清理证明。
- 各阶段验证工作空间 owner / lease 审计无残留。generation、空索引和当前预备池记录为正常保留状态，不作为泄漏删除。
- 最终现场收敛为三个 Ready API、一个 Ready Redis、三个普通 prepared Pod、三个 FUSE prepared Pod；重启次数均为 0，没有 Terminating 或 Failed Pod。
- NetworkPolicy 仅剩 Helm 基础策略和六个当前预备池策略；没有测试沙盒的普通/FUSE 用户策略或 CiliumNetworkPolicy 残留。
- 最后一分钟 API 日志没有匹配 ERROR、unconfirmed 或 failed to start。测试过程曾出现正常销毁等待期间的 cleanup pending WARN；最终状态已收敛，不把这些历史日志误报为持续泄漏。
- 已知对象存储测试文件在沙盒销毁前删除并同步；本次没有做全桶对象清零审计，也没有删除业务前缀。
