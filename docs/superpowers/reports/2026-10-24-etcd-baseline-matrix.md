# Redis 职责基线（第一轮只读盘点）

基线：`feat/workspace-fuse-mount` `c14a52c`。本报告只记录代码中已有的职责和迁移阻塞；尚未验证 etcd 实现或业务闭环。

| 现有入口 | 读写责任 | etcd 映射边界 | 首要验证 |
| --- | --- | --- | --- |
| `cmd/sandbox/main.go` 的单一 Redis Store | session、ephemeral、multipart、workspace、普通池、FUSE 池、活跃沙盒共同使用 | 接线必须逐项替换，避免新进程一部分读 Redis、一部分读 etcd | 无 Redis 的新进程启动与全部调用点 |
| `state.Store` / `AtomicStore` | 单键读写、TTL、SetNX、CAS、删除、递增及 `Keys` | 单键 etcd compare/lease 可实现；ACK 丢失须读回或返回不确定结果 | 双客户端冲突、租约过期、重启和 ACK 丢失 |
| `ActiveSandboxRepository` | 发布、操作租约、控制权、销毁、checkpoint、删除和 Scan | Redis Lua 多键转换须以原记录、操作、控制权的同一事务边界重建 | 同沙盒并发、旧 token 拒绝、真实到期清理 |
| `FUSEPoolRepository` | prepare/reserve/bind/cleanup、维护锁、索引和计数 | Redis Lua 同时更新记录/索引/配额；etcd 事务必须有界且保留这些不变量 | 池容量竞争、清理证据、只退款一次 |

## 明确的阻塞与下一步

`Keys(pattern)` 并非全是尾部前缀匹配。`internal/sandbox/ordinary_pool_rollout.go` 使用 `ordinarypool:v1:*:<pool>:owner:*`，不能直接改成 etcd prefix range；必须先选定索引或改写该调用的有界遍历。`internal/sandbox` 的 session、ephemeral、workspace owner、普通池和 FUSE 池还有多处前缀枚举，应逐个核对 N 很大时的读取量。此项属于原存储替换范围，不授权重做运行时协议。

`internal/storage/state/redis/store.go` 有四组单键/缺席 Lua 辅助；`active_sandbox.go` 有发布、操作租约、控制权与删除等十五组 Lua 转换；`pool.go` 有 prepare/reserve/cleanup、索引、计数与维护锁等十四组 Lua 转换。仅按函数名移植会遗漏原脚本的多键条件。

### Key、脚本及读写者

| Key 家族 | 原读写者与 Lua 边界 | TTL/索引约束 | 迁移时必须证明 |
| --- | --- | --- | --- |
| `sandbox:session:v2:*`、`sandbox:ephemeral:v1:*` | `session.go`、`ephemeral.go` 使用普通 Store；旧 session key 仍有读取兼容分支 | session 有 TTL，ephemeral 为持久记录；两者都有枚举 | 新环境是否可以删除旧 session 回退须明确；枚举不能读取其它家族 |
| `sandbox:workspace:{lease,owner,generation}:*` | `workspace_lease.go` 的 SetNX/CAS/Increment；`workspace_owner_audit.go` 额外使用 `CompareAndDeleteIfAbsent(owner, lease)` | lease 可过期，owner/generation 持久；owner 枚举 | lease 消失不能使旧 owner 重新授权；跨副本只能一个有效 owner |
| 普通池 `ordinarypool:v1:*` 与 FUSE rollout owner | `ordinary_pool_shared.go` 的锁、记录和 CAS；rollout 的跨配置 owner 扫描 | 短租约锁和持久记录并存 | 一次滚动更新不能漏掉旧配置的存活 owner |
| `sandbox:active:v1:{scope:bucket}:*` | 活跃记录、操作 ZSET、mutation/exclusive/controller 锁及 16 个 bucket index；15 组 Lua 覆盖 Publish、Activate/Update、Begin/Renew/EndOperation、Destroy、Checkpoint、Delete、Controller | 操作和控制租约过期，record/index 持久；部分转换按 Redis `TIME` 判断 | 同一原子边界内先关 admission、排空操作再删记录；失效 token 不能更新 snapshot |
| `fusepool:*` | pool 记录、按池/状态 membership、count、deadline、UID 归属、generation 和 refill lock；14 组 Lua 覆盖 create、bind、reserve、transition、cleanup、列表、计数和锁 | 短锁与持久索引并存；列表以 generation 前后核对 | 容量 admission 与索引/计数同事务；终止证据确认后才能删除并归还池名额 |

`AtomicStore` 不是全部契约：`internal/sandbox/state_absence.go` 可调用 `ConfirmAbsence`；`internal/sandbox/workspace_owner_audit.go` 可调用 `CompareAndDeleteIfAbsent`。如果 etcd Store 不实现这些可选方法，业务层可能走较弱的回退路径，必须在接线前固定目标行为。

### Lua 转换目录

- 普通 Store：`compareAndSwapScript`（值匹配后重写并可换 TTL）、`compareAndDeleteScript`（值匹配后删）、`compareAndDeleteIfAbsentScript`（第二 key 不存在才删 owner）、`confirmAbsenceScript`（只读缺席证明）。后两项是可选接口消费者，不能在迁移时漏掉。
- 活跃记录：`publishActiveSandboxScript` 同时写 record 与 bucket index；`changeActiveRecordScript` 对 revision/phase 做状态变更；`updateActiveOperationScript` 要求 live operation/mutation/exclusive 并可同时撤销 controller。
- 操作与销毁：`beginActiveOperationScript`、`renewActiveOperationScript`、`endActiveOperationScript` 同时触及 record、过期集合和互斥锁；`beginActiveDestroyScript` 先关 admission，再返回 live count；`liveActiveOperationsScript` 先去过期成员再计数。
- 控制与收尾：`checkpointActiveSandboxScript`、`checkpointActiveControllerScript` 分别作普通/持有者限定 checkpoint；`deleteActiveSandboxScript`、`deleteActiveControllerScript` 验 live 操作为零后同时删 record/index/锁；`acquireActiveControllerScript`、`renewActiveControllerScript`、`releaseActiveControllerScript` 管带 TTL 的 owner token。
- 池变更：`createPreparingScript` 写 record、member、state、deadline、UID 映射与计数；`bindPreparingRuntimeScript` 绑定真实 runtime；`reservePreparedScript` 从集合选候选并转状态；`transitionScript` 覆盖普通转移、归还与带 refill 锁转移；`claimCleanupScript` 抢独占清理；`confirmCleanupTerminationScript` 固定终止证据；`deleteCleanupScript` 同时清记录、多个索引、计数和 UID owner。
- 池观察及锁：`listMembershipSnapshotScript`、`listBatchScript`、`countPreparingAndPreparedScript` 读索引并交叉验证；`drainRefillLockScript`、`tryRefillLockScript`、`renewRefillLockScript`、`unlockRefillScript` 管短期锁。后续逐方法测试必须把只读校验与写入原子性分开审查。

### 不能直接逐字移植的地方

1. 活跃沙盒的 `beginActiveOperationScript` 和 `beginActiveDestroyScript` 先清过期 ZSET 成员，使用 Redis `TIME`，并读写 record、operation 与互斥 key。etcd lease 自动删除与本地时钟不能直接等价；需要显式定义请求线性化点、过期判断和 Unknown 行为。
2. FUSE 池的 `reservePreparedScript` 从 prepared 集合选择成员，`countPreparingAndPreparedScript` 验证整组 membership，`ListByPoolKey` 用 SSCAN 分批读并按 generation 重试。若在一次 etcd 事务中比较全部成员，所需比较数随存活池大小增长，违反有界事务目标。需要先设计保持容量 admission 正确性的聚合索引；这是 Task 3 的设计门，不是性能优化尾项。
3. `ordinary_pool_rollout.go` 的中间通配符无法用单个 etcd prefix 查询。Task 0 需把原 `Keys(pattern)` 的所有调用分类成固定前缀、跨配置 owner 和运维全量审计；后两者不得悄悄退化为每次扫描整个 N。

### 基线测试记录

`go test ./internal/storage/state/... -count=1` 在此分支通过编译和本地测试。没有 `TEST_REDIS_ADDR` 时，Redis 包 verbose 结果为 18 PASS、83 SKIP，因此那一轮不能称 Redis 集成合同已验收。

随后运行一次性本地 `redis:7.4-alpine`，容器 `eeebd6a06437`，端口仅绑定 `127.0.0.1:64103`：

| 运行 | 结果 | 日志 SHA-256 |
| --- | --- | --- |
| `TEST_REDIS_ADDR=127.0.0.1:64103 go test -v ./internal/storage/state/redis -count=1` | 100 PASS、1 SKIP；跳过 `TestSentinelAuthIntegration`，单节点不能证明 Sentinel 认证 | `dcea1732ffbcb9af6cf3d0f077b583af4b3593462b57570aad9b24fcca8a8710` |
| 同环境 `go test -v ./internal/sandbox -run '^(TestWorkspaceRequestRealRedisConcurrency\|TestWorkspaceLookupRealRedisAcrossReplicas\|TestDistributedFUSEQuiescedFlushRecoveredWithRealRedis\|TestOrdinaryPoolCheckoutContentionRefresh)$' -count=1` | 4 PASS | `208d0f86188500d208ee9af098d27cf116fbf88f37250693ae9d7cec5a56217d` |

Go 1.25.6；`GOCACHE` 使用主仓库 `.superpowers/sdd/_cache/go-build`，`GOMODCACHE` 使用已有共享 `go-mod-private`。完整日志暂存 `/tmp/etcd-reset-{redis-integration,sandbox-baseline}-20261024.log`；结果和哈希已记录。测试后容器及专用镜像已删除。这些结果只固定 Redis 基线行为，尚不能证明 etcd 版本通过。

本轮未创建 etcd 产品代码。下一门继续对照每个 Lua 调用的具体 key/TTL/失败码，特别是池索引的 N 相关成本，再做 etcd 最小事务设计；如果原接口无法维持安全语义，先报告阻塞和最小接口变化，不扩大业务范围。
