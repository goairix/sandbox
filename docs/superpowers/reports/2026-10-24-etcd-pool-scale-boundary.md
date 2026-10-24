# FUSE 池的存活 N 边界（Task 3 前置审查）

基于 `feat/workspace-fuse-mount` 的实际接口与调用点。此页只裁定 Redis 状态替换所需的读写边界，不改变运行时协议。

## 已确认的阻塞

- `state.FUSEPoolRepository.ListByPoolKey` 返回整个池的 `[]FUSEPoolRecord`。相同配置经 `ComputeFUSEPoolKey` 得到同一键；池内 `consumed` 记录可随已有沙盒数量 N 增长。业务代码有 15 处生产调用，不可把新实现写成一次全池 etcd Range，再声称申请在大 N 下有界。
- 原 Redis Lua 的 `validatePoolInventory` 在操作中检查整池 membership 与六个状态集合。etcd 事务不能照搬 N 个比较条件；即使 etcd 允许，单次请求成本也随 N 增长。
- `CountPreparingAndPrepared` 和 `CreatePreparingWithAdmission` 必须在并发创建下共同维护 `maxSize`。单纯分页计数再写入会超卖。`ReservePrepared` 不能先读取全部 consumed 记录挑一个 prepared。
- Redis 脚本用 `TIME` 写入 `PrepareUntil`、`ReservedUntil` 和 `CleanupUntil`，接口另有 `ServerTime`。etcd KV 没有等价的服务端时间返回值。到期裁决必须基于服务端 lease key 的存在/消失；记录里的绝对时间只能作展示或调度提示。不能把 API/worker 的 `time.Now()` 当作可信租约有效性判断。

## 最小目标映射

| 需求 | 新环境 etcd 键/原子边界 | N 成本 |
| --- | --- | --- |
| 单准备记录与 UID 归属 | 每 ID 一条 record；UID owner 索引与 record 同事务比较、更新 | 单条常数级 |
| 准备/可用容量 | 每 PoolKey 一个 `preparing+prepared` 聚合计数；Create、Return、Reserve、Delete 按状态变更与 record/index 同事务 CAS | 单池常数级；同 PoolKey 竞争受一个计数键限制 |
| 挑选 prepared | `state/prepared/<ID>` 前缀有限读取候选；交易比较候选 record/index，再转状态 | 每次有限批；冲突可重选但须有重试上限 |
| 清理和回收 | cleanup token、终止证据、record、UID owner、state index、计数与锁按原状态转移同事务比较 | 单条常数级，不从租约过期推断物理终止 |
| 到期与锁 | reservation/cleanup/refill 的授权键交由 etcd lease 自动失效；每次效果事务比较授权键仍在，记录时间不授予效果权限 | 单条常数级；重启后以键状态恢复 |
| 定点恢复查询 | 按 PreparationID 读取 exact record；替换仅为找一条记录而调用全池 List 的代码 | 单条常数级 |
| 维护、滚动和审计 | 池/状态索引的有限分页；调用者边读边处理且可重启，明确扫描期间并发变化 | 每页有限，完整巡检仍为 O(N) |

接口 `ListByPoolKey` 本身无法保证上述最后两条。Task 3 必须先为需要定点查询和维护扫描的调用者加窄接口或分页消费方式，保留旧 Redis 实现作基线回归，然后再实现 etcd 池仓库。不能仅在 etcd 实现内部把全池分页读完后重新拼成同一大 slice。容量结论须由真实并发/大 N 实验决定，本页不声称可支撑 10 万存活沙盒。
