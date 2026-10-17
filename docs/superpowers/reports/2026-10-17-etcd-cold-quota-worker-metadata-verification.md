# Cold quota worker metadata 验证记录

日期：2026-10-17。范围为私有、独立计费的 metadata 点读及其固定传输入口。

## 行为与边界

`readColdQuotaWorkerPoints` 在原 enrolled owner、session、partition 与 Range share 下读取 1–14 个业务点。每次读保留完整 W20 fence，并仅支付原 Range 262144 字节，不读取或扣减 Claim。原 mutation 入口仍要求 Claim、保持 W24。共享准入继续在原生命周期锁内完成成员检查、试扣、call 注册及额度发布，失败不会部分扣费。

实际请求之前校验完整 request 与保守 reply，总和不得超过 262144 字节；request 单独不得超过 131072 字节。付费后以本次准入得到的 comparisons 替换预检快照并再次校验。两个分支都包含 identity、restore 与全部业务点，最多 52 个 comparison/operation 节点。

新增第六个独立 metadata cohort，不能与 journal 或 Grant/guard/Commit/Revoke 共用同一个 call。只允许非空、纯点 Range 分支。该阶段允许单 KV value 最大 65536 字节，实际 receive cap 等于 262144 减实际 request 字节；原五阶段的固定响应及 KV 限制保持原值。沿用原连接、一次序列化、零重试缓存及有限 wire graph 解码。

实际 native 返回后才完成 call；owner 留给后续阶段。进入 native 后遇到不确定结果、坏 ACK、false W20 或值超限，保守地使 session 失效。调用者在 reader 返回前保持 keys/caps 不变，在实际 RPC 返回前保持 request graph 不变。

## 执行结果

| 轮次 | 结果 | 输入数 / 原始输出字节 | 原始输出 SHA-256 |
| --- | --- | --- | --- |
| RED01 | 真实失败，仅缺少两个批准的新方法 | 9096 / 964 | `800cc08594f802c6e4e8f1ff7ca2830beba24248935e799cc37a45c4ad116573` |
| host01 | PASS，15 个顶层测试，0.973s | 9102 / 21790 | `5664420220106101fec7e62b1c403aa52e75baa0be0dbf011e997c9e1658bc22` |
| race01 | PASS，6.296s，metadata、admission、RPC、journal、cohort 与 wire model prefixes 回归 | 9107 / 73471 | `0a50ced9997e0a28ca423160da0c31d92d8200fd08f452a2f3a301ac23e01643` |
| Linux vet01 | PASS，Linux arm64、CGO=0，整个 etcd 包 | 9197 / 0 | `e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855` |

Root 执行上述命令。前三轮已在源码或工具变化前完成原格式的全部 17 项源码输入、工具、pre/post、归档与当时 current 审计。磁盘清理之后仅增加两条输入不可变性注释，并将 collector 改为共享缓存、压缩清单与仓库范围复用快照；Linux vet 使用该新格式，完成相应完整审计。历史命令及结果保留，不因 collector 变化重新执行已验收轮次。

测试覆盖大 history、完整 W20 字段、Claim 独立性、预检拒绝不扣费、实际动态 receive cap、坏 ACK、mutator/nested request 拒绝、cohort 互斥、旧阶段限制，以及取消后仍须等待实际 native 返回的 Release/Close 边界。gofmt 与 diff check 干净。

## 职责与限制

reader 文件负责预检、准入及 evidence；metadata RPC 文件负责固定传输入口。原 admission、codec 与 wire 文件各自维护原有生命周期及协议不变量。它们保持包内私有，避免为拆包导出持有 authority 的内部类型。

该 helper 返回普通 metadata，不产生 quotaCommitted、physical-death 或 preparation 删除权限。独立付费 postread、完整认证恢复、自动 actor、生产接线、容量与故障恢复验收仍未完成。本轮 host 协议测试及 Linux 静态检查不代表 native etcd、网络容量或生产迁移完成。
