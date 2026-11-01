# etcd 请求协调降载设计

## 目标与范围

保留 etcd 集群作为跨副本协调和持久状态的权威来源，利用事务、revision、Lease 和 Watch，降低文件接口、Exec 等普通请求造成的重复注册、释放和轮询成本。用户已确认文件 List 首访问题解决；本次不修改文件枚举、FUSE 参数或镜像版本。

成功标准是：在保持跨副本销毁、独占、重启恢复及运行时身份隔离语义的前提下，普通请求的稳态协调成本降到一次只读事务，写入成本按 API 实例与沙盒的活跃使用关系摊销。状态变更继续保留独立事务。用户已于 2026-11-01 确认本文设计，尚无实现或性能收益实测。

## 当前成本

依据 `internal/sandbox/active_store.go`、`internal/storage/state/etcd/active_operation*.go`：

| 路径 | 当前正常路径成本 | 优化目标 |
| --- | --- | --- |
| 短 Data 操作开始 | Get、LeaseGrant、写事务，3 次 RPC | 使用关系首次注册时执行 |
| 短 Data 操作结束 | 两次读取、删除事务，3 次 RPC | 每次请求只减本地引用数 |
| Data 操作续期 | 两次读取、Grant、写事务、旧 Lease Revoke，通常 5 次 RPC | 共享记录使用原生 KeepAlive，辅以权威检查 |
| 销毁等待 | 每 25ms 查询 LiveOperations | Watch 通知唤醒，低频读回兜底 |
| 工作区独占等待 | 每 10ms 查询 LiveOperations | 同上，保留最终 fencing |

以上不含冲突重试、后台控制器或 Kubernetes API 查询。现有控制器已经有复用 Lease 的路径，不能把所有后台续期都计为 Grant-and-swap。Data 是操作分类，包含文件写入等业务动作，并不表示只读文件接口。

## 选择的方案

采用“每个 API 实例与沙盒共享一个 Data 使用记录、本地维护请求引用数、每个请求一次权威检查”。沿用现有 operation key 和 Data 记录格式，使销毁和独占仍能看到该参与者。

仅合并 RPC 和降低轮询频率，不能消除随请求数增长的注册/删除写入；完全依赖缓存或把请求路由到单一 owner，则扩大了一致性与故障转移范围。本次选择共享使用记录，同时保留每请求权威检查。

### 职责分工

| 职责 | 位置与规则 |
| --- | --- |
| 沙盒阶段、快照、Generation、RuntimeUID、工作区归属、清理进度 | etcd 持久记录；用完整身份与事务 CAS 变更 |
| 某 API 实例是否仍有沙盒使用关系 | etcd leased Data operation；进程重启不可复用旧 token |
| 本实例并发请求数、单请求取消、空闲计时 | 进程内 gate 和引用计数 |
| 阶段变化、操作退出通知 | Watch；只唤醒和关闭本地准入，不授权清理 |
| 是否可进入销毁或工作区独占副作用 | 权威读取、现有事务和运行时身份 fencing |

三成员集群用于故障容错与一致性；不把增加成员作为业务写吞吐扩展方案。保留少量线性一致读，优先消除不必要的写入、Lease 创建和历史版本增长。

## 共享使用记录

### 身份与生命周期

本地 registry 的身份包含 InstanceID（进程启动唯一标识）、SandboxID、Generation、RuntimeID 和 RuntimeUID。每组使用一个随机 operation token，并绑定独立服务端 Lease；不把不同沙盒绑定到同一 Lease，避免一个组的释放撤销其他组。

组状态为 `creating → open → draining → closed`，失去有效租约可从任意活动状态转入 `lost`。关闭的组不能重新打开；阶段恢复为 active 后须重新创建使用记录。相同沙盒的首次注册在本实例内合并，避免并发创建多个等价组。

初始边界：Lease TTL 30s、每 10s 维护一次；引用数归零后空闲保留 15s。registry 默认最多 1024 个组，含 creating/draining 的占位；超限请求走现有独立 operation 路径。只能淘汰引用为零的组，不能为控制内存删除在途保护。定时任务加抖动；所有数值是初始工程参数，容量测试后再调整。

### 普通请求流程

1. 找到或注册该身份的共享记录。注册复用 BeginOperation 的 CAS 准入，仍核对 active 阶段和不可变身份。
2. 在组锁内确认未关闭、租约确认期限有效，再预留本地引用，避免与 idle eviction 或 drain 删除组记录竞争。
3. 执行一次线性一致的只读 Txn，同时读取沙盒 record 和本组 operation key。在同一服务端快照检查 active 阶段、Generation、RuntimeID/UID、无工作区 transition、确切 token、Data kind 和 Lease 绑定。缺失或冲突则释放引用并拒绝请求。
4. 用本次读取的最新 snapshot 创建请求视图；保留现有运行时存活与 UID 校验。不能因共享组而永久缓存 TTL、网络或工作区状态，也不在本轮移除 Kubernetes 查询。
5. 业务结束后只减少本地引用；文件流必须在底层 reader/helper 结束并完成 Close/join 后才释放。取消单个请求不能取消同组其他请求。

这里将此前讨论的“每请求一次 Load”细化为“一次 RPC 的只读 Txn，包含 record 和 operation 两次 Get”，同时确认当前状态与共享保护仍在。请求与销毁并发时，以权威检查的快照确定准入顺序；已有共享 key 持续保护已预留引用，销毁必须等待其退出。

### Lease 维护与失效

新组使用专有 Data 维护能力复用原生 KeepAlive，不直接替换 Mutation/Exclusive 的 RenewOperation。后台每轮合并读取 record 和 operation，核对完整身份及 LeaseID，再对同一 Lease 续期；只有确认成功才延长本地有效期限。服务端 leased key 决定存活，持久 ExpiresAt 只作提示；不得用过期的持久时间字段拒绝仍有效的 KeepAlive 组，也不得只凭本地时间确认所有权。

维护上下文属于 Manager/组，不能继承第一个 HTTP 请求的取消。有效期限使用保守的单调时间估计，从请求发起时刻计算并预留余量，不能从延迟收到响应的时刻直接加完整 TTL。进程暂停后恢复必须先检查期限。

阶段进入 exclusive、destroying 或 cleanup_pending 时关闭新准入，但允许已持有的 Data Lease 在排空期间维持；引用为零后按确切 token 删除记录，再停止维护。续期失败、Lease/key 缺失、身份改变或期限不明时，关闭准入、取消组内请求，禁止透明补建 Lease 让旧请求继续执行。

原生 KeepAlive 仍有服务端维护成本；目标是减少重复 Grant、Revoke 与业务 KV 改写，不宣称心跳没有集群开销。若现有 operation 消费者依赖持久 ExpiresAt 的新鲜度，必须先解决兼容性，再启用这条路径。

## Watch 与排空

每个 API 实例在配置的状态命名空间建立有界数量的前缀 Watch，分发给本地活跃组和等待者；不随常驻沙盒数量建立独立 watcher。复用同一 etcd client。前缀包含的无关子 key 更新也会消耗传输和解析成本，须计入负载验证。

本地组观察到非 active、删除或身份变化立即关闭准入。组注册完成后必须补做权威检查，消除 Watch 先到、组后加入 registry 的竞态。后台维护即使没有事件，也读回状态，防止无限保留已关闭的空闲组。

等待销毁或独占遵循：

1. 先通过现有事务关闭新准入。
2. 权威读取 operation 数量及响应 header revision R。
3. 如未排空，订阅/接入从 R+1 开始的变化通知。使用 etcd revision，不混用业务 Record.Revision。
4. 相关事件到来后合并唤醒并再次权威读取；每个 sandbox 同时只安排一次检查，避免事件风暴。
5. Watch 无事件时，每秒至多一次带抖动的权威复核，取代 10/25ms 轮询。只有确认 live==0（销毁）或 live==1 且唯一操作为当前 exclusive，才进入既有 fencing 和后续步骤。

共享 Watch 必须按 revision 缓冲/分发并处理等待者注册竞态；无法回放到 R+1 时重新读取，不能从一个较新的 Watch 起点假设中间没有事件。断线从最后完整处理的 revision 恢复；遇压缩、溢出或不可恢复历史时，对相关组和等待者有界读回重建。检查并发受限，同 sandbox 合并；权威读失败不得视为已排空。

Watch 没有有界到达延迟且不是线性一致读，因此只能通知，不能授权。参见 [etcd API guarantees](https://etcd.io/docs/v3.6/learning/api_guarantees/)。Lease 绑定 key 的存活与续期规则参见 [etcd Lease API](https://etcd.io/docs/v3.6/learning/api/#lease-api)。

## 必须保留的安全边界

- 生命周期和工作区归属变更继续用 revision、Generation、RuntimeUID 与确切 token 事务校验；Mutation/Exclusive 不共享普通 Data 记录。
- BeginDestroy 或 Exclusive 成功后，新的权威检查不得准入；此前已准入请求必须保持引用到远端终态。idle timer、Watch 关闭和请求结束之间，删除记录必须在“gate 已关闭且 refs==0”条件下执行一次。
- HTTP 取消、远端 exec 断连、Lease 过期均不能证明远端进程已停止。普通可控关闭须保持 Close/join 边界；故障场景仍须验证既有运行时和工作区隔离机制足以阻止旧副作用。无法证明时不得报告安全验收通过，须标为阻塞并单独说明所需协议改动，不在本轮暗中扩大运行时协议。
- 写入或续期超时按结果未知处理，不盲重试，也不撤销可能已经生效的 Lease。精确结束失败可依靠 Lease 自然过期，不能删除后继 token。
- BeginShutdown 先关准入，保留在途请求所需维护直至正常排空。进程强制退出走现有失效恢复；持久沙盒/工作区归属不挂在该进程使用 Lease 下。
- etcd 无法确认权威状态时拒绝新业务准入；不自动切换 Redis 或以缓存继续授权。

## 接口、兼容与交付范围

通过可选仓库能力暴露“共享 Data 记录读取/续期”和“按 revision 等待变化”，业务 Manager 不依赖 clientv3。现有 Begin/End 与 Mutation/Exclusive 接口语义保留；不支持新能力的后端保留原路径。

共享记录仍是原 Data operation，LiveOperations 计数表示存活参与者而非 HTTP 请求数量。当前销毁和独占主要依赖零/唯一性，但必须用测试覆盖旧版实例与新版共享实例混跑，不能仅凭格式相同认定兼容。组的 KeepAlive 不改变原 token，旧版计数及精确删除应保持有效；持久 ExpiresAt 兼容检查是启用条件。

新增一个默认关闭、可部署配置的 `sharedDataOperations` 开关，贯穿配置与 Helm。通过下述验证后才评估默认开启。关闭开关或滚回旧镜像时，既有新版进程先关准入、排空再退出；不得手工清空 etcd key 或把旧组 token 交给新进程接管。单实例灰度期间其余实例继续使用原路径。

本轮实现单位为组 registry/gate、etcd 可选能力、共享 Watch/等待分发和配置接线。后台生命周期扫描、配额、FUSE pool、workspace owner 的协议与 TTL 不作顺手重构。仅测量其基线成本；若其成为主要负载，另立有数据依据的优化项。

## 验收与测量

使用真实 etcd 集群和可统计 RPC 的测试客户端；正确性测试不以 wall-clock sleep 假设时序，优先用受控屏障与故障注入。负载至少包含单实例单沙盒高并发、多实例同沙盒、大量低频沙盒、长文件流、销毁/独占突发及后台维护同时存在。

必须通过：

1. 新旧实例并发请求与销毁/独占；新准入关闭且在途流排空前不执行受保护副作用。
2. 首次建组并发、组创建时事件先到、idle eviction 与新请求竞争、registry 超限回退。
3. 单请求取消不影响同组；远端 Close 未 join 时组记录不可提前释放。
4. 进程暂停/崩溃、网络分区、续期结果未知、Lease 删除、Generation/RuntimeUID 更换；旧 token 不复活、不删除后继。
5. Watch 断线、压缩、缓冲溢出、长时间无通知，最终通过权威复核推进，读取失败保持关闭。
6. graceful shutdown、旧新格式混跑、开关关闭及镜像回滚；持续流量下没有组记录泄漏。

性能报告分别列出业务请求和后台任务的 RPC、读事务、KV 写入、Grant/Revoke/KeepAlive、watch 数量与字节量、活跃组/Lease/key 数、DB 增长，以及 API/etcd p50/p95/p99。指标不以 SandboxID/token 做高基数标签。

同一已建立有效组内，短 Data 请求正常稳态应为一次只读 RPC、零每请求协调写入；首建、空闲退出和维护成本另计。无事件等待复核不高于每 sandbox 每秒一次，事件唤醒读单独统计。大量低频沙盒可能不能摊薄组成本，必须报告与旧路径的对照，不能只测高复用场景。

通过并发与故障验收后再灰度；任何安全用例失败都阻止开启。本文的 RPC 变化是代码路径目标，集群容量、收益百分比及可承载沙盒数量均由实测给出。
