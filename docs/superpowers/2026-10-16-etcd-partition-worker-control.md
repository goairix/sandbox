# 分区 worker 会话与共享预算

这组接口提供分区调度的控制基础：一个 worker 会话使用一个原生 etcd Lease，承载固定 256 个逻辑分区的归属，以及全 cell 的有限预算份额。它尚未接入生产调度循环；现有主程序默认仍走 Redis。due/dirty 索引、Watch 恢复、有界缓存、实际任务执行器和持久的 32 个任务并发槽仍需后续实现。

## 开启与初始化

`etcd.Options.WorkerControl` 非 nil 才开启本组件。`WorkerControlOptions.SessionTTL` 为零时请求 15 秒；显式值必须为 3 至 60 秒的整数秒。`New` 复制配置，只读取并固定已有的 operator profile，不创建 profile、会话、Lease 或分区归属。缺失或不一致的 profile 导致初始化失败。

profile 位于当前 namespace 的 `meta/worker_control_budget`。operator 应在应用启动前以独立管理身份一次性创建，事务确认该 key 尚不存在、identity 和 restore epoch 仍为预期值。profile 必须是永久记录：Lease=0、Version=1、CreateRevision=ModRevision>0。禁止应用自动覆盖它或忽略错误后继续工作。

记录使用以下字段顺序的紧凑 canonical JSON，不附加空格或换行。替换示例中的 namespace、restore epoch 和 digest；digest 是**当前 identity key 原始 value 字节**的 SHA256 小写十六进制值，不能先格式化 JSON 再计算。

```json
{"version":1,"profile_id":"partition-worker-v1","namespace":"/sandbox/authority/cell/","restore_epoch":"operator-restore-id","identity_digest":"<64位小写SHA256>","partition_count":256,"relist_slots":4,"range_slots":20,"range_bytes_per_second":1048576,"range_burst_bytes":262144,"claim_slots":100,"claim_per_second":1,"claim_burst":1}
```

本版本固定上述槽数和速率，不提供每副本覆盖值。profile 的 key、value、Lease、原 CreateRevision/ModRevision 均进入后续事务比较。修改或重写 profile 会使持有原权限的会话停止工作；恢复相同字节也不会复活已判定失权的原对象。恢复环境应使用新的 restore ID，并重新准备对应 profile，遵循整体恢复和隔离流程。

生产仍需独立的 mTLS/RBAC 接线。worker 需要读取本 namespace 的 identity、restore epoch 和 profile，并操作本组件的 session、partition、budget key；这不授予 runtime、workspace owner、quota 或 task 的修改权限。

## 会话与分区

调用 `OpenWorkerSession(ctx, workerID)` 创建新 incarnation，`RenewWorkerSession(ctx, session)` 显式续租，`ReleaseWorkerSession(ctx, session)` 释放。WorkerID 是 1 至 128 字节的合法 ASCII domain segment；同 WorkerID 再次打开仍得到新的 SessionID 和 Lease。会话的父 ctx 应属于 worker 生命周期，取消会终止该原会话。

每个 Backend 最多保留 32 个初始化或活跃会话。该上限是本地记账上限，不能解释为全 cell 的任务并发限制。没有按 sandbox 数量建立的常驻会话或续租 goroutine。调用者负责及时续租，且应给原 deadline 前的校验和 RPC 留出时间。

etcd 可以提高请求的短 TTL。本组件保留实际原生 TTL，以请求发送时刻计算单调 deadline；续租只对原 Lease 执行 KeepAliveOnce，并在原 deadline 内完成前后权限复核后才更新本地 deadline。续租不改变 SessionID、LeaseID、guard revision，也不扫描或重写已领取的 256 个分区。

`ClaimWorkerPartition(ctx, session, partition)` 对指定 uint8 分区执行一次 CAS。分区 key 为 `p/<两位小写hex>/worker`，所有分区归属绑定该会话的同一个原 Lease。正常竞争失败返回冲突，保持候选会话有效；成功的重复领取返回同一原对象。`ReleaseWorkerPartition` 仅按原 key/value/Lease/CreateRevision/ModRevision 精确删除，不能删除后来接管者的记录。释放后该分区在同一会话中保留终态标记，不能重新领取。

同一会话最多有一个在途原生操作，重叠调用立即冲突，不排无限队列。引用仅用于诊断，复制 Backend、session、partition 或 share，或持有它们的 Reference，都不能构造执行权限。

## 固定共享份额

`AcquireWorkerBudget(ctx, session, kind, slot)` 只尝试指定槽一次。占用失败表示该候选被占用，不能据此宣称全 cell 容量耗尽。一个会话可以持有多个不同槽，最多固定 124 项，方便少量 worker 使用全部额度。重复领取同 kind+slot 返回原对象并保留原桶状态；释放后的原会话不能再次领取该槽。

| kind | slot | key | 每槽额度 |
| --- | --- | --- | --- |
| `WorkerBudgetRelist` | 0–3 | `workers/budget/relist/00` 至 `03` | 一个当前重建份额 |
| `WorkerBudgetRange` | 0–19 | `workers/budget/range/00` 至 `13` | 1 MiB/s，桶容量 256 KiB |
| `WorkerBudgetClaim` | 0–99 | `workers/budget/claim/00` 至 `63` | 1 次/s，桶容量 1 |

槽绑定原 session Lease。新份额在 CAS 和原权限复核成功、对外发布前以空桶开始计时，慢领取不能预先攒满桶。`ConsumeWorkerBudget` 使用 Go 单调时间和整数余数记账，本地立即收费或拒绝；不访问 etcd 计数、不退款、不排队。Range 的 amount 为 1 至 262144 字节，claim 必须为 1，relist 不通过该方法消费。

持续持有全部份额时，合计赚取额度为 20 MiB/s 和每秒 100 次 claim，允许空闲后积累最多 5 MiB/100 次的桶容量。这不是任意滑动窗口内绝不突发的承诺。交接后的新桶不赠送额度；已经发出的旧请求仍可能有尾部。

后续真正的 Range/claim consumer 必须在发请求前收费，每份额最多保留一个真实在途请求；Range 每页不超过 256 条且完整响应不超过 256 KiB，并受原请求 deadline 限制。数量限制本身不能保证字节限制，应按实际记录上界选择更小页数。上述 consumer 约束尚未生产接入，单独调用 Consume 也不授予原生事务或 runtime/task 权限。

## 分区重建与实际退出

`RunWorkerRelist(ctx, partition, relistShare, work)` 要求分区和份额来自同一原会话，执行原生权限前检、同步调用 work，再在原权限后检成功时返回成功。每 Backend 固定最多四个本地实际 owner，每份额最多一个；取消 ctx 或 Lease 失效不会提前腾出仍未返回的 owner。

callback 使用接纳时的原 deadline。会话可以在 callback 执行期间续租，但已经运行的 callback deadline 不随之延长。callback 的执行期间不持有会话的原生操作许可或生命周期锁。

释放 share/partition 会取消关联 callback，等待实际返回，再进行原 CAS 清理；会话释放和 Backend.Close 同样等待实际 RPC、callback 及已登记的释放调用退出，再清理原 Lease 和关闭 client。若 callback 忽略取消，Close 会继续等待，不能把超时当作退出。

callback 不得同步调用会等待自身退出的 share/partition/session Release 或 Backend.Close，也不得等待一个反过来 join 自身的关闭 goroutine；应先返回，再由外部 owner 执行释放或关闭。

普通本地 callback 错误或本地 ctx 取消会丢弃该次结果，但不直接证明整个 session 失权。实际在途 RPC 取消或响应未知、原 guard/profile/identity/restore/partition/share 不一致，则停止旧权限继续使用。旧结果不得发布到后来会话的缓存。跨进程 Lease 交接可能与旧读取尾部重叠，四个软槽不能解释为严格的全局物理在途上限。

## 错误、关闭与验证边界

纯非法参数或发请求前已经取消的 ctx 不破坏仍有效的原会话。原生结果未知时不自动重新 Grant、换 UUID、尝试其他槽或依据相同 WorkerID 认领旧权限；已知原 Lease 的清理保持有界。未知删除或 Revoke 返回不确定结果，不能宣告已释放。并发会话 Release/Close 共享同一次清理及其结果。

本组件的 record 上限为 2048 字节，key 上限为 1024 字节，事务仍遵守 64 个节点及 256 KiB 的请求/响应预算。首次 Grant 前执行最坏控制事务的 wire 预检。后续 consumer 使用私有原比较组合，将 identity/restore、profile、session、partition、share 一起放进其实际事务；诊断 ref 和本地 token 不能代替这些比较。

验证由 host 协议模型、race、Linux vet 及单独的 `TestPartitionWorkerControlNative` 组成。原生测试拥有三个独立 etcd 成员和一个真实 worker 子进程，验证竞争、原租约、份额和进程退出后的接管；具体运行结果以本单元验收报告为准。它不代表大存量容量、吞吐提升、完整调度器或生产切换已经通过验收。
