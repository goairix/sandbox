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

`internal/storage/state/redis/store.go` 有四组单键/缺席 Lua 辅助；`active_sandbox.go` 有发布、操作租约、控制权与删除等十五组 Lua 转换；`pool.go` 有 prepare/reserve/cleanup、索引、计数与维护锁等十三组 Lua 转换。下步将每组的读写 key、TTL、原子性和失败返回逐项列出，再判断可否在 etcd 事务限制内保持行为。仅按函数名移植会遗漏原脚本的多键条件。

本轮未运行基线测试，也未创建 etcd 产品代码。下一门先补完整脚本/调用点表和基线测试记录；如果原接口无法维持安全语义，先报告阻塞和最小接口变化，不扩大业务范围。
