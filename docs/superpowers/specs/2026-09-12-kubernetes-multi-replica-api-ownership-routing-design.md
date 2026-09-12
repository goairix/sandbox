# Kubernetes 多副本 API 所有权路由设计

## 目标

让 Sandbox API 在 Kubernetes 多副本部署下具备真正的正确性：客户端的任意请求可以到达任意健康 API Pod，不依赖 HTTP 长连接、Service 会话亲和或客户端固定连接。

每个沙盒在任一时刻只能有一个 API 副本负责生命周期控制，其他副本通过集群内 HTTP 把请求透明转发给该副本。方案必须覆盖普通沙盒、同步工作区沙盒和 FUSE 沙盒，并保留现有的 Runtime UID 校验、工作区 fencing、FUSE 一次性授权和精确清理语义。

## 范围

- 适用于配置了 Redis 的 Kubernetes runtime。
- Docker runtime 保持现有单进程生命周期行为。
- 第一版所有权协议要求从干净 Helm release 安装，或者先完成 release-wide drain；不推断旧版本未持久化的 ephemeral 沙盒归属。
- 启用新协议后的滚动升级支持主动交接，不再依赖销毁所有活跃沙盒。

## `main` 分支为什么看起来没有问题

`main` 分支把活跃沙盒放在每个进程自己的 `m.sandboxes` Map 中，只把 persistent 沙盒写入 Redis。另一个副本收到 persistent 沙盒请求时，可以从 Redis 延迟加载快照；ephemeral 沙盒则无法跨副本加载。

Go SDK 长期复用同一个 `http.Client`。默认 Transport 会复用 TCP 连接，而 Kubernetes Service 通常按连接选择后端，所以 create、exec、destroy 经常落在同一 Pod 上，掩盖了多副本状态没有协调的问题。

FUSE 不能继续使用这种“各副本分别加载”的方式。FUSE 沙盒具有本地 operation gate、租约续期、健康 watcher 和销毁状态机；多个副本同时恢复同一个 FUSE 沙盒会产生重复控制器和跨副本销毁竞争。

## 方案比较

### 方案一：Redis 所有权路由 + Pod 间 HTTP 转发（采用）

每个沙盒只有一个 API owner。其他 API Pod 查询 owner，并把原始 HTTP 请求流式转发到 owner Pod。Redis 的实例租约和 CAS 所有权记录负责 fencing 和故障接管。

优点：保留已经验证的本地 operation gate 和 FUSE 生命周期状态机；owner 本地请求不增加 Redis 往返；只在请求落到非 owner 时增加一次 Pod 网络跳转。

### 方案二：所有 API 完全无状态，每个操作使用 Redis 分布式锁

每个副本在每次请求时重新加载沙盒，并把 operation gate、watcher、销毁同步、文件流生命周期和工作区续期全部改成分布式状态。

该方案会让 Redis 进入每个请求的关键路径，改动范围大，并显著增加长流式请求的锁续期复杂度，本次不采用。

### 方案三：Service 或 Ingress 会话亲和

ClientIP 或 Cookie affinity 只能作为流量优化，不能作为正确性机制。连接重建、Pod 替换、Ingress 变化以及多个客户端共享出口地址时都会失效。

## 总体架构

### API 实例注册表

每个 API Pod 在 Redis 注册一条带 TTL 的实例记录：

```text
sandbox:routing:v1:<scope>:instance:<instance-id>
```

记录内容包括：

- 协议版本；
- instance ID；
- Pod UID；
- Pod IP；
- API 端口；
- 随机 epoch token；
- 状态：`ready` 或 `draining`；
- 启动时间。

Deployment 使用 Kubernetes Downward API 注入 Pod UID 和 Pod IP。实例记录 TTL 默认为 15 秒，每 5 秒通过精确 CAS 续期。

进程在本地保存“最后一次确认租约有效的截止时间”。Redis 续期失败后，只允许在确认的有效期内继续服务；到达安全截止时间前主动停止处理 owner 请求。token 不匹配时立即失去 owner 资格。这样 Redis 网络分区不会形成双 owner。

实例只发布 Downward API 提供的 IP 字面量和配置中的 API 端口。代理目标拒绝任意 URL、主机名、userinfo、路径和非配置端口，避免 Redis 值被利用为通用 SSRF 入口。

### 活跃沙盒记录

每个成功创建的沙盒有一条不自动过期的权威记录：

```text
sandbox:routing:v1:<scope>:sandbox:<sandbox-id>
```

字段包括：

```text
version
sandbox_id
owner_instance_id
owner_epoch
owner_generation
state: publishing | active | transferable | recovering | destroying | cleanup_pending
revision
完整 Sandbox 快照
created_at
updated_at
```

所有模式都必须写完整快照，包括普通 ephemeral 沙盒。

该记录不依赖 Redis TTL 自动删除。沙盒超时时间保存在快照内，由 owner reaper 先完成精确 runtime 清理，再删除记录。这样不会发生 Redis key 先过期、runtime 却仍在运行的问题。

所有变更都使用 SetNX 或对原始字节进行 CAS。owner、恢复状态和沙盒快照放在同一记录中，避免多 key 更新产生不一致窗口。

### owner 本地快速路径

创建沙盒的副本会把沙盒发布到现有 Manager Map，并缓存对应的 owner generation。请求命中本地 owner 时：

1. 正常完成 API Key 认证和路由解析；
2. 检查本地实例租约仍在确认有效期内；
3. 校验本地缓存的 owner epoch 和 generation；
4. 进入现有 Manager operation gate 和 handler。

稳定状态下不执行 Redis 查询。实例心跳按 Pod 摊销，而不是按请求或沙盒数量执行。

### 非 owner 转发路径

对于 `/api/v1/sandboxes/:id/**`，路由中间件从有界本地缓存读取活跃记录。owner 不是本机时，再从实例缓存得到 Pod IP，通过以下地址直接转发：

```text
http://<owner-pod-ip>:<api-port><原始 path 和 query>
```

转发保留：

- HTTP method；
- path 和 query；
- Authorization；
- Content-Type 等业务 header；
- trace context；
- request body；
- status、response header、trailer 和 response body。

上传、下载和 exec stream 全程流式传输，不完整缓冲到内存或磁盘。客户端断开会通过 context 取消 owner 请求，并保留反压行为。

内部请求头包含 sandbox ID、owner generation、来源和目标 instance ID、时间戳、随机 nonce 和 hop count。Helm 为 API Pod 提供独立的 routing key；来源副本使用 HMAC 签名 method、RequestURI、owner generation、来源和目标 instance ID、时间戳及 nonce，不签名流式 body。

owner 只接受 hop count 为 1、generation 与本地一致、时间戳在允许窗口内、HMAC 正确，并且来源实例心跳有效的请求。每个 owner 使用有界的短期 nonce 缓存拒绝重放。无法验证为内部请求时，所有内部 header 都会被删除并按普通外部请求重新路由；第二次转发直接拒绝，防止缓存陈旧时形成代理环路。

每一跳都校验原始 API Key。路由中间件位于认证之后、限流之前；来源副本转发后不进入本地限流器，owner 副本执行一次限流和一次 handler，避免代理请求被重复计费。

### 路由缓存

路由缓存默认有效期为 1 秒，并同时限制最大条目数。缓存只减少 Redis 读取，不能提供所有权。

以下情况立即失效缓存并重新查询 Redis：

- owner Pod 连接建立失败；
- owner 返回 generation 不匹配；
- 实例 epoch 不匹配；
- 缓存超时。

旧 owner 只有在本地实例租约仍有效时才允许服务，因此即使其他副本短暂持有旧缓存，也不会越过 fencing 边界。

### HTTP 连接管理

每个 API 进程使用一个共享、受限的 `http.Transport`：

- 按 owner Pod IP 复用 keep-alive 连接；
- 设置连接池总量和每个 owner 的上限；
- 设置有限的 connect timeout、TLS handshake timeout（若未来启用 TLS）和 response header timeout；
- 不设置覆盖完整响应生命周期的统一 client timeout；
- 长 exec stream 和大文件传输由原始 request context 控制；
- streaming response 及时 flush。

代理不得自动重试 POST、PUT、DELETE、上传或 exec。连接中断时无法证明 owner 是否已经执行副作用，自动重试可能导致命令执行两次。只有尚未写出请求体的连接建立失败可以安全返回 503；幂等 GET 最多允许在刷新路由后重试一次。

## 生命周期

### 创建与发布

create 和 one-shot 请求由 Kubernetes Service 选择的副本直接执行。普通 create 按以下顺序发布：

1. 创建或领取精确 runtime；
2. 完成工作区授权、依赖安装和 readiness 检查；
3. 生成完整沙盒快照；
4. 使用 SetNX 写入本机 owner 的 `publishing` 活跃记录；
5. 发布本地 Manager 生命周期；
6. CAS 把记录从 `publishing` 改为 `active`；
7. 向客户端返回创建成功。

任何持久化步骤失败都不能返回成功。清理必须使用精确 Runtime UID 和现有补偿流程。Redis 返回不确定错误时，先读取并比对完整预期值，再决定是否补偿。

one-shot 在同一 owner 内完成 create、exec、destroy，不向客户端暴露临时 sandbox ID，但仍使用相同的精确清理保证。

### 状态更新

以下对外可见或恢复必需的状态必须 CAS 更新活跃记录：

- 网络配置；
- TTL；
- 工作区 mount 状态；
- FUSE pool 消费记录与 generation；
- destroy 和 cleanup 进度。

exec 的临时 `running`、`idle` 展示状态保留在 owner 内存，不写 Redis，避免 Redis 成为执行热路径。

### 销毁

owner 先关闭本地 operation gate 并等待正在执行的操作，再把活跃记录 CAS 为 `destroying`。随后执行现有 runtime、工作区、FUSE pool 和 NetworkPolicy 精确清理。

全部成功后 compare-delete 最终记录。部分失败时写入 `cleanup_pending`，保留 runtime 身份和清理证据，后台继续恢复；不得返回成功或提前丢失记录。

### owner 异常死亡与接管

只有满足以下任一条件时才允许接管：

- 沙盒记录已经是 `transferable`；
- 记录引用的实例心跳不存在；
- 实例 ID 相同但 epoch 已变化。

实例仅处于 `draining` 还不够；沙盒记录仍为 `active` 时不得接管。代理连接失败也不能单独作为抢占依据。

`active` 或 `transferable` 记录的接管流程：

1. 读取活跃沙盒记录和 owner 实例记录；
2. 证明旧 owner 已失去服务资格；
3. CAS 将记录改为 `recovering`，owner generation 加一并写入新 owner；
4. 从快照和现有 fencing 记录恢复普通、Sync 或 FUSE 生命周期；
5. 必要时启动本地 operation gate、工作区续期和健康 watcher；
6. CAS 将 `recovering` 改为 `active`，然后开始接收请求。

只有 CAS 胜者可以恢复。其他副本在 `publishing`、`recovering`、`destroying` 或 `cleanup_pending` 期间返回 `503 Service Unavailable` 和 `Retry-After: 1`，不能返回误导性的 404。

不同中断状态采用不同恢复方向：

- owner 在 `publishing` 阶段死亡：创建结果尚未对客户端可见，接管者只做精确补偿清理，不擅自发布为 active；
- owner 在 `destroying` 或 `cleanup_pending` 阶段死亡：接管者取得清理所有权并从持久 checkpoint 继续销毁，不能恢复为 active；
- 新 owner 在 `recovering` 阶段失败：保留失败原因和精确身份，在有界退避后重试；超过配置的恢复次数后进入 `cleanup_pending`，不得无限占用恢复状态。

现有 WorkspaceCoordinator 可以从相同的持久 owner 恢复租约，包括租约 TTL 已过期的情况；恢复不会分配新的 workspace generation，也不会重放 FUSE 一次性授权。

owner 异常死亡时，正在进行的 exec、上传或下载属于“不确定完成”状态：服务端绝不自动重放。新 owner 可以恢复沙盒后接受新请求，但不能宣称旧请求一定未执行。需要端到端恰好一次语义的调用方应另外使用业务幂等键，本次不扩展公共 API。

### 滚动升级的主动交接

SIGTERM 使用新的 handoff 路径，不调用 release drain：

1. readiness 置为失败，实例标记为 `draining`，停止新 create；
2. 逐个关闭本地沙盒 admission，并等待已进入的操作完成；
3. 从实例注册表选择一个 `ready` 目标副本；
4. CAS 将沙盒从 `active` 改为 `transferable`，保留旧 owner 作为 CAS 证据并记录目标 owner；
5. 通过内部 handoff 请求通知目标副本恢复；
6. 目标副本 CAS 为 `recovering`，恢复完成后 CAS 为 `active`；
7. 源副本确认新 generation 已 active 后，停止自己的 watcher 和续期，不删除 runtime 或持久状态；
8. 全部交接完成后删除实例心跳并退出。

如果源副本在任一步骤崩溃，剩余副本按照心跳过期流程接管。交接期间新请求可以短暂收到可重试 503，但不能收到 404，也不能同时由新旧 owner 执行。

Kubernetes `terminationGracePeriodSeconds` 必须覆盖最大普通操作排空时间和 handoff 控制面时间。超过期限的长请求由原始 context 取消，并进入异常死亡恢复语义。

release drain 和 Helm uninstall 是另一条显式流程：它们会销毁 runtime，并继续要求最终审计为零状态。

### 后台恢复与超时清理

不能只在客户端请求到来时才发现 owner 死亡，否则无人访问的超时沙盒会永久残留。

每个 API 副本运行低频协调循环，使用 Redis `SCAN` 分页读取活跃记录；禁止在生产路径使用阻塞式 `KEYS`。代码新增独立的 `state.Scanner` 分页接口，Redis 和测试 Store 分别实现，不把全量扫描塞进单 key 的 `AtomicStore` 接口。记录通过稳定哈希分片给健康实例，只有负责该分片的实例尝试：

- 接管失去 owner 的记录；
- 清理已超过 TTL 的沙盒；
- 恢复 `cleanup_pending`；
- 处理长期停留在 `publishing`、`recovering` 或 `transferable` 的记录。

CAS 仍是最终仲裁，因此分片变化或重复扫描不会产生双 owner。扫描分页大小、周期和每轮最大恢复并发均有上限，避免 Redis 或 Kubernetes API 被恢复风暴压垮。

## 可用性约束

- 健康 owner 的本地请求不依赖逐请求 Redis 查询。
- 非 owner 请求通过缓存路由和连接池访问 owner。
- Redis 暂时不可用时，已有 owner 只能服务到最后确认的本地租约安全截止时间；之后返回 503，禁止冒险产生双 owner。
- owner 心跳仍有效但 Pod 网络不可达时返回 503，不允许仅凭连接失败抢占。
- 优雅交接不重建 runtime；目标是在 termination grace period 内完成。
- owner 异常死亡后，默认在 15 秒心跳过期加恢复耗时内重新 active。
- 恢复和清理必须幂等；部分失败始终保留证据。
- create 自然由 Kubernetes Service 分散到多个 API Pod，因此 owner 和代理流量不会集中到单 leader。
- `/health` 表示进程存活；新增 `/ready` 只有在 Redis 实例租约有效、Manager 已启动且路由器可服务时返回成功。

## 性能约束

- owner 本地请求不增加同步 Redis 操作。
- 非 owner 路由缓存命中时不增加同步 Redis 操作。
- 非 owner 请求最多增加一次 Pod 网络 HTTP 跳转。
- 路由缓存默认 1 秒、限制容量，并支持主动失效。
- 代理连接必须复用；请求体和响应体必须流式传输。
- exec 临时状态不写 Redis。
- 实例心跳固定为每 Pod 每 5 秒一次 CAS，与沙盒数无关。
- 后台恢复使用有界 `SCAN`、分片和并发限制，不能使用 Redis `KEYS` 全量阻塞。

性能验收分别测量：

- 本地 owner 普通 JSON 请求；
- 跨 Pod 普通 JSON 请求；
- 跨 Pod exec stream；
- 跨 Pod 并发上传和下载；
- 路由缓存冷启动与热命中；
- 100、1,000 和 10,000 条活跃记录下的后台扫描影响。

验收目标：本地路由新增服务端 p99 开销小于 1 ms；同节点跨 Pod 代理控制面 p99 开销小于 5 ms；跨节点代理新增 p99 开销小于 10 ms。数据传输时间和 sandbox 实际执行时间不计入代理控制面开销。这些数字是集成环境验收指标，不作为代码中的硬超时。

## Helm 与配置

Deployment 注入：

```text
SANDBOX_ROUTING_INSTANCE_ID   <- metadata.uid
SANDBOX_ROUTING_POD_IP        <- status.podIP
SANDBOX_ROUTING_PORT          <- server.port
SANDBOX_ROUTING_SCOPE         <- release namespace/name
SANDBOX_ROUTING_KEY           <- Helm Secret
```

Kubernetes runtime 配置 Redis 后启用多副本路由。实例身份缺失或非法时启动失败。Helm schema 要求 Kubernetes `replicaCount > 1` 或开启 autoscaling 时必须配置 Redis。

API Pod readiness 在实例注册和 Manager 启动成功后才打开；进入 draining 时先关闭 readiness，再开始交接。

当前 API ingress NetworkPolicy 允许 Pod 间访问，不需要创建新的公开 Service。内部 handoff 和代理入口仍使用同一端口，但必须同时校验来源实例、owner generation 和内部路由 header。

所有权协议版本写入 Helm cleanup protocol。旧协议升级到新协议需要 release-wide drain 或全新安装；相同协议的后续滚动升级使用主动 handoff。

## 安全性

- 不在日志或 trace 中记录 API Key、Redis token、workspace credential 或完整内部 header。
- 不能通过 HMAC、时间窗口、nonce 和实例心跳校验的内部 header 一律删除。
- routing key 与外部 API Key 分离，不能出现在 values 渲染结果、日志或 trace 中。
- 内部请求来源和目标 instance ID 必须对应有效实例记录。
- owner generation 不一致时拒绝执行，不继续转发。
- Redis 不可用、记录损坏或状态不确定时 fail-closed。
- 代理目标只能来自经过严格解析的实例记录，不能接受客户端提供的地址。

## 可观测性

日志、指标和 trace 区分：

- owner 本地请求与代理请求；
- 路由缓存命中和未命中；
- proxy connect、generation mismatch 和 loop rejection；
- 实例续期成功、失败和本地 authority 失效；
- graceful handoff 与 TTL takeover；
- 各 mount 类型的恢复耗时和结果；
- ownership CAS 冲突；
- cleanup pending 数量和持续时间；
- 后台 SCAN 页数、耗时和限流情况。

内部 HTTP 跳转继续传播原 trace context，并建立 proxy child span。

## 测试设计

单元测试和 race test 使用共享同一个 AtomicStore 和 fake runtime 的两个以上 Manager/router 实例：

1. 在副本 A 创建 ordinary ephemeral 和 persistent 沙盒，通过副本 B 执行 get、exec、network、TTL、文件操作和 destroy。
2. 通过非 owner 副本操作 Sync 和 FUSE 沙盒，证明没有重复启动 renewal 或 watcher。
3. 验证普通响应、stream、上传、下载、trailer、取消和错误响应均可透明代理。
4. 统计调用次数，证明本地热路径和远端缓存热命中均为零 Redis read。
5. 并发发起 takeover，证明只有一个 owner generation 进入 active。
6. 在 idle、exec、upload、destroy 阶段终止 owner，验证有界 503、无双 owner，并最终恢复或继续清理。
7. 模拟 Redis timeout 和 CAS 不确定回复，验证精确 readback 和 fail-closed。
8. 在有在途请求时执行 graceful handoff，验证不返回 404、不重复执行、不重建 runtime、不丢工作区数据。
9. 验证非幂等请求在不确定网络错误后不会自动重试。
10. 验证后台使用 SCAN 分页，并在大量记录和恢复风暴下遵守并发上限。
11. 运行完整 Go test、race test、go vet、Helm lint/template 和至少三副本 Kubernetes 集成测试。

线上 Kubernetes 验收必须：通过一个 Service 创建沙盒，再刻意固定到不同 API Pod 执行和销毁；随后删除 owner Pod 并重复普通、Sync、FUSE 三种模式。还要在负载期间执行一次滚动升级，确认 runtime 未被重建。

## 验收标准

- 任意健康 API Pod 可以接收所有 sandbox-scoped endpoint。
- ordinary ephemeral 不会仅因请求落到其他副本而返回 `SANDBOX_NOT_FOUND`。
- 每个 sandbox generation 恰好只有一个有效 owner。
- owner 本地实例 authority 过期后不能继续服务。
- owner 故障期间返回有界、可重试的 503，恢复后继续使用原 runtime；不能返回假 404。
- 优雅滚动升级完成所有权交接，不重建活跃 runtime。
- 本地请求没有逐请求 Redis 操作；远端请求只经过一次连接复用的 HTTP 跳转。
- stream 和大文件不被代理完整缓冲。
- 非幂等请求不进行不安全的透明重试。
- 无访问流量时，过期和 cleanup-pending 沙盒仍能被后台协调器处理。
- release drain 和 uninstall 仍能通过零托管状态审计。
- 完整 Go、race、Helm、性能及三副本 Kubernetes 验证全部通过。
