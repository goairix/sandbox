# Task finalization preflight design

日期：2026-10-08。父方案：[etcd 状态管理迁移](2026-10-06-etcd-state-management-design.md)。接续已验收的 [user-quiescence 单元](../reports/2026-10-08-task-user-quiescence-unit-acceptance.md)。本单元落实父方案已经批准的最终同步入口复核要求，继续既有小步提交与独立复审；不改变对象布局或请求用户重新批准整个迁移。

## 目标与边界

已有 `PreparedTaskQuiescence` 是绑定原 Backend、原 TaskClaim、不可变历史、runtime terminal receipt 和一次 native operations 空观察的本地前提。它不能凭缓存或旧 revision 授权之后的 final sync/flush。增加明确的即时复核 API，供下一单元的可信副作用生产者在记录/发送效果前调用。

这是只读安全入口，返回 error，不产生可转交的 bearer grant，不记录 final sync 成功、不释放 owner、不执行存储或 runtime 副作用。生产消费者、远端效果 ledger、FUSE 管理回执、精确终止和安全释放仍为后续任务。此次单元完成不能称为生产迁移完成。

## 接口与实现

在 `internal/storage/state/etcd/task_finalization_preflight.go` 增加：

```go
func (b *Backend) RevalidateTaskQuiescence(ctx context.Context, c *TaskClaim, prepared *PreparedTaskQuiescence, destination *controltransport.Destination) error
```

`prepared` 必须是原对象（self 指针）、原 Backend、原 claim 指针与精确 ClaimReference；空对象、拷贝、其他 Backend、其他 claim、零 revision、零 deadline、空 receipt 均在 RPC 前拒绝。原 prepared deadline 是这次复核的上限，同 claim 续租不能延长旧 prepared。入口处已经过期的旧 prepared 在调用 `c.live` 前拒绝，不把仍有效的续租 claim 人为判 lost。进入有效复核后 deadline 越界或取消仍沿用原 `c.live(bounded)` 的保守 lost 策略，即使 claim 在期间续租；不能恢复 lost 位或放宽共享 claim/fence 策略。这种中途超时的可用性代价保留，下一 claimant 必须重新获取只读前提。

复用 `PrepareAfterDualDrain` 的 Query → 新 native whole-prefix limit1 Range → Query → final immutable-pin check 算法，提取一个内部共同函数以避免两个版本漂移。原 API 保持原有行为。复核路径在首次 runtime query 前用已有 `pinTaskDualDrain` 比较保存的 exact pins；两次经过当前 runtime 证书验证的 terminal receipt 都必须与保存的 receipt wire 摘要一致。新 Range 必须真正执行，不读取保存的 `emptyRevision` 作为结果；新观察 revision 可与旧 revision 相同（没有新 mutation 时），不能以 `>` 冒充“新查询”。

固定保留现有的 53 个 compare 加两个 bounded Range 分支、55 个 literal native nodes、原 claim/guard/restore/control/owner/runtime/历史 receipt/reservation 比较。观察完整 operations prefix，不用已读 page、缓存 count 或抽样代替。未知、非空、RPC 错误、runtime 证书/Boot 不匹配、取消、deadline、pin 变化都返回错误，且不发送效果命令、不 Grant/Put/Delete/签票。

这里的成功只表示这次调用完成上述即时复核。之后的生产者仍须在自己的授权/intent 原子事务和每次外部发送边界验证当前 claim 与相关永久 fences；此次结果不代替远端结算或终止证明。

## 备选与取舍

1. 缓存旧 prerequisite：缺少新操作观察和当前 runtime 认证，违反父方案，拒绝。
2. 将 fresh check 返回一个可存储的新 grant：扩大授权面且易被当成最终释放证明，拒绝。
3. 显式只读复核，共用原算法：本单元采用。多两次经过认证的 query 和一次 native empty transaction 属于每个活动 cleanup 的固定成本；没有每存活 sandbox 的定时器、常驻 goroutine 或全局扫描。吞吐代价由后续 N/并发容量阶段测量，此处不承诺性能提升。

## 验证与验收

- 纯 host 拒绝用例：nil context/claim/prepared、非原对象或外来 identity、空证据、旧 deadline；拒绝在任何 KV 或 Dial 前发生。
- 原生三成员 etcd + genuine mTLS 测试替身：保存合法 prerequisite 后再次复核，断言两次新 query、一次新完整-prefix limit1 Range、无发送/签票/Grant/mutation，前后 native 值/revision/lease 不变。
- 保存后重新加入 wire/原 lease tuple 有效的原 operation（测试操作员注入）。native 重建的 CreateRevision 晚于已冻结 destroy birth，按既有 decoder 视为 ErrCorruptRecord；新 Range 必须真实看到非空并拒绝，证明没有复用旧 emptyRevision。生产 closed admission 本来就不允许合法新 operation，此测试不伪造旧 native birth、不放宽 decoder、不声称出现了生产合法的迟到入场。
- 保存后在新 Range 前修改 receipt/reservation revision、撤销原 claim 或取消 context，均失败。入口已过期的旧 prepared 不毒化续租仍有效的原 claim；中途越界保留原保守 lost 策略，两者都不允许 deadline 延长。
- genuine mTLS 目标失联或返回仅 accepted/未知、不同 terminal receipt，均不得成功。测试目标的签名证明协议归因，不证明物理 PID1 排空；上一单元物理证据保留其原冻结范围，不重放。
- 受影响 etcd package 的 host race/vet 和 Linux arm64 CGO0 编译。三成员集成用例必须实际执行；缺失 fixture 只能记录 SKIP，不能当 PASS。

最终记录新提交与独立审查结论，并逐项保留无法验证的性能、生产消费者、远端结算、物理 runtime 和部署范围。
