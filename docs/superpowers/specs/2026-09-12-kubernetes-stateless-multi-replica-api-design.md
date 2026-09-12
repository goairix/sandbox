# Kubernetes 无状态多副本 Sandbox API 设计

## 目标

把 Kubernetes 环境中的 Sandbox API 改造成真正无状态的多副本服务。客户端请求可以到达任意健康 API Pod，由当前 Pod 直接读取权威状态并操作 sandbox runtime，不依赖以下机制：

- API Pod 之间的 HTTP 或 gRPC 转发；
- Service、Ingress 或客户端连接亲和；
- 创建 sandbox 的原始 API Pod 持续存活；
- 进程本地 Map 充当权威状态。

方案覆盖普通 ephemeral、普通 persistent、同步工作区和 FUSE 工作区。性能目标是 Redis 只承载短小的控制面原子操作，exec、上传、下载等数据流直接在当前 API Pod 与 Kubernetes API/runtime 之间传输。可用性目标是任一 API Pod 下线都不需要迁移请求连接，后台生命周期控制可以自动接替。

## 设计原则

1. Redis 中的活跃沙盒记录是 Kubernetes 模式下唯一权威状态。
2. 任意 API 副本都能直接操作同一个 sandbox，但必须先进入分布式 operation gate。
3. 普通数据操作允许并发；销毁会原子关闭 admission，并等待已有操作结束或租约过期。
4. FUSE 续租、健康检查和超时清理由后台 coordinator 负责，不属于创建请求的 API Pod。
5. Runtime ID 和 Runtime UID 始终一起校验，不能仅凭可复用名称操作 Pod。
6. Redis、Kubernetes API 或状态不确定时 fail-closed，不能用猜测换取表面可用性。
7. 非幂等请求发生不确定网络错误后不得自动重放。

## 适用范围

- Kubernetes runtime：启用本设计。
- Docker runtime：保持当前单进程生命周期模式，不伪装成多副本。
- 第一版从干净 Helm release 安装，或者先完成 release-wide drain；不推断旧版本从未持久化的 ephemeral sandbox。
- 新协议启用后的滚动升级不销毁活跃 sandbox。

## 权威活跃记录

每个 sandbox 使用一组 Redis Cluster 同槽 key。hash tag 同时包含部署 scope 摘要和 sandbox ID，确保 Lua 脚本在 Redis Cluster 下可以原子访问：

```text
sandbox:active:v1:{<scope-digest>:<sandbox-id>}:record
sandbox:active:v1:{<scope-digest>:<sandbox-id>}:operations
sandbox:active:v1:{<scope-digest>:<sandbox-id>}:mutation
sandbox:active:v1:{<scope-digest>:<sandbox-id>}:controller
```

`record` 是不自动过期的权威 JSON，包含：

```text
version
sandbox_id
phase: publishing | active | destroying | cleanup_pending
revision
generation
完整 Sandbox 快照
cleanup checkpoint
created_at
updated_at
```

完整快照覆盖所有模式，包括 ordinary ephemeral。sandbox timeout 是记录中的业务字段，不使用 Redis key TTL 代替。只有精确 runtime 和相关状态全部清理完成后，才能 compare-delete 活跃记录。

`operations` 是按过期时间排序的 ZSET，保存当前已经进入 operation gate 的操作 token。`mutation` 保存可选的单个生命周期变更 token。`controller` 是同步工作区或 FUSE 后台控制器的短租约。

所有 key 都由服务端根据经过校验的 sandbox ID 构造。禁止客户端提供完整 Redis key、scope 或 hash tag。

## 分布式 operation gate

### 开始操作

`BeginOperation` 使用一个 Lua 脚本完成：

1. 读取并严格解析 `record`；
2. 删除 `operations` 中已经过期的 token；
3. 要求 phase 为 `active`；
4. 必要时检查没有其他 mutation token；
5. 添加包含随机能力值、记录 revision 和操作类型的新 token；
6. 返回同一次原子读取到的完整 sandbox 快照和 record revision。

因此请求不需要先 GET 再加锁，冷路径也只发生一次 Redis 往返。

操作分为两类：

- 数据操作：get、exec、stream、上传、下载、读取、列举和普通文件编辑；允许多个操作并发。
- 生命周期变更：更新网络、更新 TTL、mount、unmount，以及会修改持久恢复状态的操作；同一时刻只允许一个 mutation，但仍可按现有语义与数据操作并存。

### 操作续期和结束

operation token 默认租约为 30 秒，每 10 秒通过 Lua 续期。续期要求 token、sandbox ID 和进入时的 sandbox generation 完全匹配。generation 在一次 runtime 身份有效期内保持不变；只有精确 runtime 被替换并完成恢复发布时才递增，普通 record revision 更新不能让在途操作失效。

长时间 exec、stream、上传和下载由一个轻量续期器维持 token。请求 context 结束后，无论成功、失败还是客户端取消，都调用 `EndOperation` Lua 删除精确 token。

若 API Pod 异常死亡，token 最多在 30 秒后过期。后台不得因为一次续期错误立即删除 runtime；销毁流程必须等待 token 真实结束或 Redis 中的过期时间到达。在途请求无法续期时，必须在本地 token 安全截止时间之前取消 runtime 调用和客户端连接，不能让请求在 Redis 已认为 token 过期后继续无限执行。

### 开始销毁

`BeginDestroy` Lua 原子完成：

1. 清理过期 operation token；
2. 将 `record.phase` 从 `active` CAS 为 `destroying`；
3. 清除尚未进入执行阶段的 mutation admission；
4. 返回当前未结束操作数量和精确 sandbox 快照。

phase 进入 `destroying` 后，所有新的 `BeginOperation` 都失败。销毁者等待已有 operation token 结束或过期，再运行现有的精确 runtime、工作区、FUSE pool 和 NetworkPolicy 清理。

清理成功后 compare-delete 最终 record，并删除同槽辅助 key。任何阶段失败都写入 `cleanup_pending` 和 checkpoint，由后台继续恢复，不能提前返回成功或删除证据。

并发 destroy 只有一个调用能完成 phase CAS。其他调用返回 cleanup pending 或等待同一清理结果，不能启动第二套销毁流程。

## 无状态请求流程

任意 API Pod 收到 `/api/v1/sandboxes/:id/**` 请求后：

1. 完成 API Key、参数和 body 大小校验；
2. 调用 `BeginOperation`，同时获得权威 sandbox 快照；
3. 使用 Runtime ID + Runtime UID 校验 Kubernetes Pod 身份和运行状态；
4. 当前 API Pod 直接调用 Kubernetes API 执行请求；
5. 如果修改了持久配置，使用 record revision 做 CAS 更新；
6. 结束 operation token。

请求不访问其他 API Pod。Kubernetes exec、文件上传下载和流式响应沿现有直连路径传输，不增加第二份数据流或中间缓冲。

进程本地 Map 只允许作为 revision-keyed 反序列化缓存和临时 runtime 客户端缓存，不能决定 sandbox 是否存在、是否 active 或能否销毁。缓存丢失只影响性能，不影响正确性。

### Runtime 重建边界

Kubernetes runtime 的请求路径必须能够仅依赖权威 record、Pod 不可变 UID、受管资源 label/annotation 和 Kubernetes 实时状态重建操作句柄。任何影响 exec、文件、网络或销毁正确性的字段，都不能只保存在创建副本的 Go 对象中。

FUSE 一次性 mount authorization 不会在其他副本上重放。它只用于首次准备 runtime；后续副本根据已经发布的 workspace mount 状态和 runtime UID 接管续租、健康检查及清理。若无法从持久状态证明授权已经完成，请求必须失败并进入精确恢复流程，不能再次授权或猜测成功。

## 创建与发布

create 请求由 Kubernetes Service 选中的任意 API Pod 执行：

1. 创建或领取精确 runtime；
2. 完成网络策略、依赖、工作区授权和 readiness 检查；
3. 构造完整 sandbox 快照；
4. 使用 SetNX 写入 `publishing` record；
5. 完成必须持久化的 FUSE pool、workspace owner 和 cleanup 状态关联；
6. CAS 将 record 改为 `active`；
7. 返回创建成功。

Redis 发布成功是响应成功的前置条件。发生不确定 Redis 回复时必须读取精确期望值验证。未能进入 `active` 的创建结果不会向客户端暴露；接管者对长期 `publishing` 记录执行精确补偿清理，而不是擅自发布。

runtime 可能在 `publishing` record 写入前创建成功。所有预发布资源必须携带 scope、sandbox ID、create attempt 和 runtime UID；orphan reconciler 只在创建宽限期后，且确认没有匹配 active/publishing record 时精确清理，避免创建副本崩溃造成永久遗留或误删新资源。

one-shot 请求不向客户端暴露 sandbox ID，但内部仍按相同记录和 operation gate 完成 create、exec、destroy，保证 API Pod 异常退出后能够继续清理。

## 持久状态更新

以下变化需要更新权威 record：

- network 配置；
- sandbox timeout；
- workspace mount 状态；
- Runtime ID、Runtime UID 和恢复 generation；
- FUSE pool 消费信息；
- destroy 与 cleanup checkpoint。

生命周期变更持有 mutation token，使用 record revision 做 CAS。CAS 冲突时重新读取权威状态并判断操作是否已经生效；不能无条件覆盖其他副本的新状态。

exec 展示用的 `running`、`idle` 状态不写 Redis。真实可执行性由 record phase、operation token 和 Kubernetes runtime 状态决定，避免高频 Redis 写放大。

## 后台生命周期 coordinator

### 职责

后台 coordinator 只处理生命周期控制，不承载用户请求数据：

- FUSE 和同步工作区租约续期；
- FUSE 健康检查；
- sandbox TTL 到期；
- `publishing`、`destroying` 和 `cleanup_pending` 恢复；
- owner API Pod 消失后的控制器接替；
- 无请求流量时的 orphan 清理。

普通 active sandbox 不需要常驻 controller；任意 API 请求都可以直接操作。

### controller 租约

需要后台控制的 sandbox 使用 `controller` key。租约包含 instance ID、Pod UID、随机 token 和 controller generation，TTL 默认 15 秒，每 5 秒精确 CAS 续期。

只有 controller lease 持有者运行该 sandbox 的 watcher 和 workspace renewal。每次续租、状态写入和破坏性动作前都必须校验 controller token 与 generation；旧 controller 即使因暂停后恢复，也会被 fencing 拒绝。失去租约后，本地循环在安全截止时间前停止。新副本只有在旧 key 过期或精确释放后，才能通过原子脚本取得下一代 controller lease。

取得新 controller lease 后，副本从权威 record 和现有 workspace owner 恢复控制状态。恢复不会重放 FUSE 一次性 mount authorization，也不会更换仍然有效的 runtime。

controller 发起健康失败或 TTL 清理时，同样必须调用 `BeginDestroy`，让分布式 operation gate 排空已有请求。

### 扫描与任务分配

新增独立的 `state.Scanner` 分页接口。Redis 实现必须使用 `SCAN`，禁止在生产协调循环中使用阻塞式 `KEYS`。

每个部署 scope 通过一个短租约选出 maintenance coordinator。它只负责分页发现和调度，不承载所有 sandbox watcher，也不进入用户请求路径。发现任务后，由健康 API Pod 按稳定哈希竞争对应的 controller lease。

maintenance coordinator 故障时，其他 Pod 在租约过期后接替扫描。即使扫描重复或实例列表变化，record CAS、controller SetNX 和 operation gate 仍是最终仲裁。

扫描周期、页大小、每轮最大恢复数量和并发数均有上限并加入 jitter，防止 API 扩容或 Redis 恢复后形成惊群。

## API Pod 生命周期

### 启动

API Pod 按以下顺序启动：

1. 验证 Redis 和 Kubernetes runtime 配置；
2. 注册带 TTL 的 API instance heartbeat；
3. 初始化无状态 Manager 和共享 pool；
4. 启动 operation、controller 和 maintenance 协调组件；
5. `/ready` 返回成功。

`/health` 只表示进程存活。`/ready` 要求 Redis 可完成控制面原子操作、Kubernetes runtime 可访问且本地协调组件处于可服务状态。

### 滚动升级和缩容

收到 SIGTERM 后：

1. readiness 立即失败，停止新 create；
2. 当前 Pod 不再领取新的 controller lease 和 maintenance lease；
3. 已进入的用户 operation 继续到完成或 context 取消；
4. 精确释放本地 controller lease，剩余副本立即竞争接替；
5. 等待本地 operation token 清理后退出。

因为用户请求没有 owner，API Pod 不需要迁移 sandbox 或代理连接。已存在的 runtime 和 active record 均保持不变。

异常退出时，operation token、controller lease 和 maintenance lease 分别在各自 TTL 后失效，其他副本自动恢复。release drain 与 Helm uninstall 继续走显式销毁和零状态审计流程。

## 不确定执行语义

ownerless API 解决了副本路由问题，但不能把网络中断后的非幂等操作变成“恰好一次”。如果 API Pod 在 exec 已提交到 Kubernetes 后退出，容器内命令可能仍然运行，但客户端没有收到结果。

服务端规则：

- POST、PUT、DELETE、exec、上传和文件编辑不自动重试；
- API Pod 死亡后 operation token 到期，只代表协调租约失效，不代表容器内副作用没有发生；
- 新副本可以接受后续新操作，但不会自动重放旧请求；
- GET 类请求在未返回响应 body 前最多安全重试一次；
- 若未来需要端到端幂等，由公共 API 单独增加 idempotency key，本次不隐式实现。

## Redis 高可用要求

Redis 是本设计的权威控制面，当前 Helm 内置的单副本 Redis 只能用于开发和测试，不能宣称生产高可用。

生产环境要求外部高可用 Redis，并新增模式配置：

```text
state.redis.mode: sentinel | cluster
state.redis.addrs
state.redis.master_name       # Sentinel
state.redis.username
state.redis.password
state.redis.db                # Cluster 模式固定为 0
```

要求：

- 至少跨节点主从部署并启用自动 failover；
- 开启持久化和合理的备份策略；
- 设置写入复制约束，并明确所选 Redis 服务的 failover 数据安全保证；普通异步主从不能承诺已确认写入在故障切换后绝不丢失；
- Redis 客户端具备连接池、拓扑刷新、有限重试、指数退避和熔断；
- Lua 脚本的所有 key 使用相同 hash tag，兼容 Redis Cluster；
- 对非幂等控制脚本发生连接错误时先精确 readback，不能盲目重试。

内置 Redis 保留为显式的非生产选项。Helm 在 Kubernetes 多副本或 autoscaling 配合内置单 Redis 时输出醒目告警；生产校验模式下直接拒绝该配置。

Redis 整体不可用时：

- 已经获得 token 的在途数据流可继续到本地 context 结束，但无法续期；
- 禁止开始新 sandbox 操作、创建、销毁或接管 controller；
- API 返回 503，而不是绕过 gate 直接操作 runtime；
- Redis 恢复后通过 token 过期、record phase 和 checkpoint 自动收敛。

这是为了防止网络分区下双销毁或越过 fencing。高可用依赖 Redis 本身采用生产级 HA 部署，而不是应用层牺牲一致性。

对 `BeginOperation`、operation 续期、`BeginDestroy`、controller 获取/续期和 record 发布等安全边界写入，生产模式必须使用具备线性一致或同步提交保证的状态服务。若当前 Redis 产品只能提供异步复制，则客户端必须在脚本成功后执行复制确认并在未确认时 fail-closed；这会增加一次小型控制面往返，且 Redis 的 `WAIT` 只能降低丢失概率，不能被描述为绝对强一致。部署校验和运行指标必须明确暴露当前 durability level，禁止把普通 Sentinel/Cluster 自动 failover 等同于零数据丢失。

## 性能设计

### 请求热路径

在状态服务本身提供同步提交保证时，普通 sandbox 请求的控制面固定为：

```text
BeginOperation Lua：1 次 Redis 往返，同时返回权威快照
直接 Kubernetes/runtime 操作
EndOperation Lua：1 次 Redis 往返
```

若使用仅异步复制的 Redis 并启用客户端复制确认，`BeginOperation` 和长操作续期会各增加一次确认往返。`EndOperation` 丢失只会保守地延迟销毁，可以异步复制；生命周期 mutation 和销毁边界不得省略确认。性能测试必须分别记录这两种 durability 模式，不能以关闭复制确认换取达标数据。

不增加 API Pod 间网络跳转，不需要额外 Redis GET，不传输两份 request/response body。

优化要求：

- Redis 客户端复用连接池；
- Lua 脚本预加载并使用 SHA 执行，NOSCRIPT 时有界恢复；
- record JSON 设置大小上限，禁止写入文件内容或凭证；
- 本地按 record revision 缓存解码结果，但不能绕过 BeginOperation；
- operation 续期按到期时间批量调度，避免每个请求创建独立 ticker；
- EndOperation 可以批量调度，但每个脚本结果必须被读取和记录；不能 fire-and-forget，失败时由 token 到期提供保守清理；
- exec、上传和下载继续使用现有流式实现，内存占用与 payload 大小无关；
- maintenance 使用分页 SCAN、jitter 和并发限额。

### 性能验收

分别测量：

- 本地 Redis 与跨节点 Redis；
- 100、1,000 和 10,000 个 active sandbox；
- 普通 get、exec、stream、并发上传和下载；
- operation token 续期风暴；
- maintenance 扫描期间的请求延迟；
- Redis 主从切换前后。

验收目标：

- 状态服务原生同步提交模式下，BeginOperation + EndOperation 的合计控制面 p99 小于 3 ms（同可用区）；异步 Redis 加复制确认模式单独建立基线，目标 p99 小于 6 ms；
- 相比单副本基线，非流式 API p99 新增开销小于 5 ms；
- exec stream 和文件传输吞吐不低于无分布式 gate 基线的 95%；
- maintenance 扫描不能让前台 Redis p99 上升超过 10%；
- API 副本数量增加时，单请求 Redis 往返次数保持固定，不随副本数增长。

这些是集成环境验收指标，不写成业务超时常量。若部署环境 Redis 无法满足延迟目标，应调整 Redis 拓扑，不能绕过一致性控制。

## 可用性设计

- 任意健康 API Pod 可以直接处理任意 sandbox 请求。
- 单个 API Pod 下线不会造成 sandbox owner 不可达，也无需等待 owner heartbeat。
- controller 故障接替目标为 15 秒 TTL 加一次恢复耗时；精确释放时可以立即接替。
- maintenance coordinator 是可租约接替的调度角色，不是数据面单点。
- Kubernetes runtime 和 active record 在 API 滚动升级期间保持不变。
- Redis Sentinel/Cluster 故障切换期间返回短暂 503，拓扑恢复后自动继续；在已声明的数据安全等级内不能产生双操作或假 404。
- cleanup failure 保留 checkpoint，不能把“清理未知”报告成成功。
- 无客户端流量时，TTL 到期、controller 丢失和 cleanup pending 仍会被后台发现。
- HPA 扩缩容只改变 API 和后台任务承载量，不改变 sandbox 权威身份。

## Helm 与配置

新增或调整：

- Kubernetes 多副本模式必须配置共享 Redis；
- 生产模式要求 external Sentinel 或 Cluster Redis；
- 增加 `/ready` readinessProbe，保留 `/health` livenessProbe；
- terminationGracePeriodSeconds 覆盖 operation 排空上限；
- Pod UID 通过 Downward API 注入，用于 operation/controller token 的诊断身份；
- ownership-routing 方案不需要 Pod IP、内部 routing key 或额外内部 Service；
- cleanup protocol 升级，首次切换要求 drain 或全新安装。

## 安全性

- operation token 使用密码学安全随机数，只在 Redis 和当前进程内存中存在；
- Redis key 和日志只记录 sandbox ID、operation 类型和不可逆 token 摘要；
- record 不包含 API Key、Redis 密码、对象存储明文凭证或文件内容；
- Lua 严格校验 phase、revision、generation 和 token；
- Runtime ID 必须结合 Runtime UID 做不可变身份检查；
- Redis 数据损坏、脚本结果非法或 readback 不确定时 fail-closed；
- cleanup 只能删除精确匹配的 runtime 和策略资源。

## 可观测性

日志、指标和 trace 至少覆盖：

- BeginOperation、RenewOperation 和 EndOperation 延迟及结果；
- active operations 数量和过期 token 数量；
- destroy admission 关闭与排空耗时；
- mutation 冲突和 record CAS 冲突；
- controller 获取、续期、丢失和恢复；
- maintenance SCAN 页数、记录数、延迟和限流；
- cleanup pending 数量、阶段和持续时间；
- Redis failover、连接池等待和 Lua 执行延迟；
- Runtime UID mismatch；
- API 503 的准确原因。

禁止在日志和 trace 中记录 token 原文、完整请求 body 或凭证。

## 测试设计

### 单元和 race 测试

1. 两个 Manager 共享同一 AtomicStore 和 fake Kubernetes runtime；A 创建 ordinary ephemeral，B 完成 get、exec、文件操作和 destroy。
2. 并发 BeginOperation 可以共存；BeginDestroy 原子关闭 admission，并等待全部 token 结束。
3. API Pod 崩溃后 operation token 到期，destroy 才能继续。
4. mutation 操作互斥，数据操作保持并发。
5. 多个副本并发 destroy，只有一个清理者。
6. FUSE 和 Sync controller lease 只有一个持有者；失效后另一副本恢复，不重放 mount authorization。
7. Redis timeout、CAS 不确定回复和 Lua NOSCRIPT 均执行正确 readback。
8. stream、上传和下载期间 token 持续续期，客户端取消后精确结束。
9. 非幂等请求在不确定错误后不自动重试。
10. maintenance 使用分页 SCAN，不调用 KEYS，并遵守恢复并发上限。
11. 所有共享状态测试运行 `go test -race` 并重复执行。

### Kubernetes 三副本验收

1. 通过 Service 创建普通、Sync 和 FUSE sandbox。
2. 分别固定到三个不同 API Pod 调用 get、exec、stream、文件、network、TTL 和 destroy。
3. 在长 exec 和上传过程中删除当前 API Pod，验证不出现错误重放，token 最终过期并可继续管理 sandbox。
4. 删除 FUSE controller 所在 Pod，验证新 controller 接替原 runtime 和 generation。
5. 在持续请求下滚动升级 API Deployment，验证活跃 runtime 不重建、不出现假 404。
6. 模拟 Redis 主节点 failover，验证有界 503 和恢复收敛。
7. 在 100、1,000 和 10,000 条记录下运行性能与扫描压力测试。
8. 最后执行 release drain 和 Helm uninstall，验证 Redis、Pod、NetworkPolicy 和 CiliumNetworkPolicy 均为零托管状态。

## 验收标准

- 任意 API 副本可以直接处理任意 sandbox-scoped endpoint，不发生 API Pod 间转发。
- ordinary ephemeral 不会因请求到达另一个副本而返回 `SANDBOX_NOT_FOUND`。
- BeginDestroy 之后不能进入新操作；已有操作结束或租约过期前不能删除 runtime。
- FUSE/Sync 同时只有一个有效后台 controller，接替不重放 FUSE 授权。
- API Pod 滚动升级不重建活跃 sandbox runtime。
- 状态服务原生同步提交时，请求热路径固定为两次短 Redis Lua；异步 Redis 的安全边界额外执行复制确认。两种模式都不额外 GET、不转发 payload。
- 流式和文件吞吐达到性能目标，内存占用不随 payload 线性增长。
- Redis failover 和 API Pod 故障期间不产生双销毁、假 404 或状态丢失。
- 无请求流量时，超时和 cleanup pending 仍能收敛。
- 内置单 Redis 不被标记为生产高可用配置。
- 完整 Go test、race test、go vet、Helm lint/template、性能测试和三副本 Kubernetes 验收全部通过。
