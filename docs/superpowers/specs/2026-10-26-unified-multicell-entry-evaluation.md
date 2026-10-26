# Sandbox 统一入口与多 cell 路由评估稿

**状态：待评估，不授权实施或切流。** 2026-10-26。单 cell 时客户端直接访问
该 cell 的 API，不部署网关或路由目录。真正启用多 cell 时才安装可选的
统一入口：客户端使用一个 API 地址和原有 Bearer key，网关将请求稳定转给
DS 或 CCE。两种形态共用现有 cell API；本方案不改变已上线的 etcd 状态
替换，也不把两个 cell 的 etcd 合并。

## 1. 现状与目标

DS 和 CCE 已分别运行独立的 API、Kubernetes sandbox、etcd 前缀与对象存储。
两边的 FUSE 主链和每 cell 20 个同时驻留的有界测试已通过，但客户端必须
选择 cell 的 API 地址。DS 使用业务 MinIO，CCE 仍使用测试 MinIO；CCE 目前
不能直接承接同一业务 workspace 的生产流量。

当前 `/api/v1/sandboxes` 创建请求可带 `workspace_path`，现有 Manager 在
**一个 cell 内**按 workspace 复用。返回的 `sandbox-...` ID 不携带 cell。
其余按 ID 的查询、执行、文件、TTL、网络、workspace、skills 接口及流式
请求都要求回到原 cell；`/by-workspace` 也只查本 cell。无 workspace 的
创建请求没有跨 cell 的通用幂等键。当前 API key 是单个 Bearer token。

本方案的目标是：

1. 单 cell 沿用当前直连路径；启用多 cell 时客户端最多切换一次 API 地址，
   现有请求、响应、sandbox ID 和主要状态码不变。
2. 同一业务 workspace 的并发申请始终进入同一 cell，后续操作回到原 cell。
3. 网关重启、请求超时和单 cell 不可用时不在另一 cell 重复创建或删除。
4. cell 内的池、owner、配额、TTL、清理及 FUSE 证明继续由原实现负责。

**不在首期交付：**运行中 Pod 跨集群迁移、旧 workspace 自动换 cell、
跨 cell 分布式事务、单 cell 失联时接管其原有 sandbox、10 万真实 FUSE
存活容量承诺。这些需要各自的存储和物理恢复协议，不能靠 HTTP 代理完成。

原 2026-10-06 设计已将稳定 workspace bucket 目录和 placement version
列为第六阶段；第一版明确不交付统一路由。旧分支的同 authority 固定目录
实验不适用于当前两个**独立 etcd 集群**，不能直接当作本方案已实现的部分。

## 2. 方案比较

| 方案 | 好处 | 关键问题 | 裁定 |
| --- | --- | --- | --- |
| DNS/L4 直接均衡两个现有 API | 部署快 | 创建与后续按 ID 操作可能落到不同 cell；无法保持 workspace 唯一 | 不采用 |
| workspace 固定哈希 + 对外改成带 cell 的 ID | 路由目录读写少 | 要重写公开 ID/响应及 SDK；扩 cell、旧 ID 和无 workspace 请求仍需兼容规则 | 暂缓 |
| 仅单 cell 直连现有 API | 零新增代理跳数、零目录依赖 | 不能给多个 cell 提供一个稳定业务入口 | **单 cell 默认** |
| 可选 HTTP 网关 + 持久归属目录 | 多 cell 时保持现有客户端协议；归属和故障行为可显式验证 | 多一项小型 HA 存储、跨集群网络和代理链 | **多 cell 时推荐** |

Kubernetes 多集群 Service 可以解决跨集群后端的可达性，但不会替业务决定
某个 sandbox ID 或 workspace 属于哪个 cell；仍需上述应用层归属规则。
[SIG Multicluster 说明](https://multicluster.sigs.k8s.io/concepts/multicluster-services-api/)。

## 3. 可插拔部署形态与持久边界

```text
单 cell：客户端 ── HTTPS ── DS API ── DS etcd/Pod/存储
```

单 cell 的 API、Helm 发布、健康检查和扩缩容独立运行，不引用网关服务、
路由目录或 CCE 配置。网关是**单独部署的入口组件**，通过配置决定是否
安装和接管外部地址；不把代理代码嵌入每个 cell 的 API，也不要求在只有
DS 时把生产流量先绕经一个“单后端网关”。CCE 测试期仍可直连其测试地址。

只有决定对同一业务客户端开放多个 cell 时，才切换到：

```text
客户端 ── 单一 HTTPS 地址 ── 无状态路由网关（至少两个副本）
                              │     └── 独立 HA 路由目录
                              ├── 私网 HTTPS ── DS API ── DS etcd/Pod/存储
                              └── 私网 HTTPS ── CCE API ── CCE etcd/Pod/存储
```

若希望客户端连地址都不用改，可以在启用多 cell 的切换窗口把原 DS
业务域名指向网关；这仍需先完成旧 owner 回填、写入栅栏和切换验证。
域名迁移本身不能代替这些条件。网关退出安装不影响单 cell API 启动，
但一旦已有 CCE 归属，不能通过撤掉网关、把域名直接指回 DS 来完成回滚。

路由目录只保存**归属**，不复制 session、FUSE owner 或配额账本。生产版
优先使用独立于 DS/CCE 故障域的 HA etcd，利用 Compare/Txn 对首次归属
做原子条件写入；若已有组织级 HA 事务数据库，也可按同一 CAS、持久性和
恢复验收比较其运维成本。不得直接把目录放在 DS 或 CCE 的 cell etcd
并同时声称另一 cell 在该集群失联时仍可独立接流量。
[etcd Txn 语义](https://etcd.io/docs/v3.6/learning/api/)。

最小目录内容：

- `workspace/<sha256(storage-identity || canonical-workspace)>`：不可变
  `cell_id`、placement version、创建修订和归属状态。规范化复用当前
  `BuildWorkspacePrefix` 规则，storage identity 来自可信网关配置。
  一个路径一旦绑定，即使 sandbox 被删除也留在原 cell；迁移另立方案。
- `sandbox/<opaque-id>`：原 cell、workspace 引用及删除后的有限墓碑。
  ID 仍原样返回；未知 ID 不通过搜索两个 cell 猜测归属。
- cell 列表、目标 endpoint、允许新建与 placement version 由审定配置
  发布。网关不能信任客户端自带的 `cell` header。选择新 cell 时可用
  冻结版本的 bucket 权重；已有 workspace 绑定不随权重变化。

目录只在多 cell 形态安装，届时需要独立快照、恢复和容量监控。
若回滚到旧快照，网关须停止新建，
核对两个 cell 的实际 owner 并重建缺失绑定后再开放；不能把“目录里没有”
解释成“所有 cell 都没有”。目录不可用时对依赖归属的请求返回 503，
不随机选 cell。首期先不用本地缓存授权路由；测到目录成为瓶颈后，
只缓存不可变绑定并对恢复 epoch 做失效控制。

## 4. 请求流程与失败语义

**带 workspace 的创建（首期重点为 FUSE）：**网关认证和校验规范路径，
读取或用 CAS 写入永久 cell 绑定，然后只向该 cell 发送一次原 POST。
两个网关并发申请同一 workspace 只有一个归属获胜。后端返回 201 后，
网关先条件写入 ID→cell，再向客户端返回原响应。后端可能已提交但响应
丢失时，绑定保持原 cell；网关仅向该 cell 的 `/by-workspace` 查询并
确定读回，不能切换 cell 重发。归属已定而尚未证实后端结果时返回
明确的暂不可用结果，保持 Unknown，不释放绑定。

这里的目录写入与 cell 内创建**不是跨集群原子事务**。持久绑定先于
后端效果、向客户端返回响应前完成 ID 映射，以及同 cell 确定读回，
是收敛这个间隙的必要顺序。若预留后网关崩溃，重试仍只走原 cell。

**后续请求：**所有 `/sandboxes/:id` 路由按 ID 绑定透传；
`/by-workspace` 按 workspace 绑定透传。执行流、一次执行流和文件上传
必须保持请求/响应流、取消、背压、状态码和体积限制，不把大文件缓存在
网关。网关对 POST、PUT、DELETE 的不确定上游结果不自动向另一 cell
重试。TTL 销毁和退款仍由原 cell 完成；目录墓碑不是退款证明。

**无 workspace 创建及 `/execute`：**首期固定走 DS，保留当前语义，
网关不对失败请求自动跨 cell 重试。要让这类请求真正跨 cell 分配，
需另加公开幂等键及其结果仲裁，或审定新的 cell-qualified ID 合同。
不把这项未完成能力混称为全 API 自动多 cell 调度。

**cell 故障：**已绑定 DS 的 workspace/ID 在 DS 故障时返回 503 并等待
DS 恢复；CCE 不接管该 sandbox。只有**从未绑定**且 CCE 已允许新建的
workspace 可以选择 CCE。CCE 停止准入时把新建权重设为 0；原 CCE
绑定仍须经统一入口访问 CCE，不能简单把整个网关回滚为 DS 直连。
若多 cell 启用前预检失败，DS 保持原直连服务即可，无新增入口依赖。

## 5. 接入前的硬条件

1. **私网可达：**网关到两个现有 ClusterIP API 尚无已验证的跨集群路径。
   先确定私有 LB/Ingress 或专线、TLS、健康检查和实际 RTT；不能把当前
   只在各自集群内工作的 ClusterIP 当成跨集群入口。
2. **业务存储：**CCE 现在是测试 MinIO。若接同一业务 workspace，
   必须使用审定的业务对象存储和一致的逻辑 storage identity，或把
   客户端/租户的存储域明确分开；不能用同一路径访问两套不同数据。
3. **入口封闭：**所有写入者切到网关后，限制直接访问 cell API 的
   业务来源。否则绕开目录仍可能在两个 cell 创建同一 workspace。
4. **旧 DS owner：**启用 CCE 新建前，按固定快照审计并回填 DS 活跃
   workspace/ID 绑定；在直接写入收口与回填之间设置切换栅栏。
   回填不完整时 CCE 新建保持关闭。
5. **认证：**对外保留一个业务 API key，网关分别使用受限的内部凭据
   访问 DS/CCE；不得将 CCE 的测试 key 或对象存储密钥发给客户端。
   原始 token、workspace 路径和凭据不写入路由日志。
6. **网关故障域：**两个副本不能仅靠数量宣称高可用。若都运行在 DS，
   DS 集群失联时统一地址也会失联；生产拓扑应让入口和目录处于独立故障域，
   并分别演练单副本、整个 cell 和目录故障。

## 6. 分段交付和验收

| 门 | 交付 | 通过条件与停损点 |
| --- | --- | --- |
| A：只读预检 | 保持 DS 直连；核实网关跨集群私网、CCE 业务存储、目录 HA 位置及旧 owner 清单 | 任一条件不成立，DS 继续直连；停止双 cell 业务写入 |
| B：离线验证可选入口 | 在隔离环境验证网关、目录及现有 API 合同，生产 DS 仍直连 | 31 条路由及流式/上传代表请求、FUSE、并发归属、超时与断连；网关不成为 DS 运行依赖 |
| C：启用多 cell 入口 | 收口直接写入、DS 回填和栅栏验证后，才把业务地址切到双副本网关；CCE 先仅接受专用测试 workspace | 网关崩溃、ACK 丢失、目录故障和恢复不跨 cell 重发；原 ID 操作路由正确 |
| D：CCE 业务准入 | CCE 存储与安全配置验收后，小比例**新 workspace**流量进入 CCE | 两 cell 的 FUSE 创建/执行/文件/TTL/删除退款、重启与故障演练均过；已有绑定不迁移 |

观察指标至少包括：每 cell 新建数与当前存活数、路由目录延迟/键数/DB
大小、绑定冲突/Unknown、cell 503、网关额外 p95、流式连接数、跨集群
网络错误及回填差异。`fusePool.maxSize=20` 只是准备池大小，不能当成
存活准入上限。当前 20/cell 功能通过与本设计的目录容量、10 万存活
或统一入口吞吐是三项不同验收。

首期成功的定义是**一个地址和一个 key，FUSE workspace 在两个 cell
之间只被确定分配一次，后续全部回原 cell**。不以 DS/CCE 各自独立
烟测、网络通达或两个集群的 Pod 总数替代这个结果。

## 7. 成本、风险与评估结论

粗略工程量（不含网络/存储采购等待及 10 万容量验收）：A 预检约 1–2 人日，
B 离线网关与目录约 5–8 人日，C 回填、切换及故障验证约 3–5 人日，
D 双 cell 业务灰度约 3–5 人日；总计约 12–20 人日。单 cell 直连不产生
这些网关运行成本。它是评估预算，不是发布日期承诺；
在 A 门拿到私网 RTT、CCE 业务存储和独立目录位置后再重估。

实施前需要明确三项决策：统一入口和目录放在哪个独立故障域、CCE 是否
共用 DS 的业务对象存储身份、首期是否接受无 workspace 请求固定 DS。
任一项未定时仍可完成 A 门预检，但不能开放跨 cell 业务写入。

- 新增路由网关、独立 HA 目录、两个集群到网关的私网 HTTPS 路径和
  备份恢复运维；网关是新的控制面，目录不可用会阻止归属判定。
- 只有多 cell 形态的请求会多一次归属判定及一个代理网络跳转；具体 p95
  必须先测网络 RTT，再与 DS 直连在同负载下比较。目录只保存归属，
  不参与每 5 秒的 sandbox 续租；但唯一 workspace 和历史 ID 的累计
  键数仍需容量与清理预算。
- 最大安全风险是目录/后端双写间隙、直接访问绕过和恢复旧目录快照。
  上述切换栅栏、只向原 cell 确定读回及恢复期停写是硬条件。
- 最大交付依赖是 CCE 当前测试存储和跨集群私网。它们未就绪前，
  保持 DS 直连，不能宣称真实双 cell 业务调度。

**建议：**保持 DS 直连，真正需要多 cell 时按 A→B→C→D 实施，
每门单独给出真实证据和可回退配置；
先决定是否需要 CCE 接同一业务存储。不要将这一架构扩展塞回已完成的
Redis→etcd 替换验收，也不要以增加 cell 数掩盖单 cell 仍存在的
O(N) 安全检查和物理 Pod/CPU 边界。
