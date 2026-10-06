# etcd stage 协议验收

日期：2026-10-06；分支：`codex/etcd-state-management`。本提交独立于已提交的客户端与后续故障测试提交。

## 交付

- 单key持久Put/Delete及受限Compare，不接受任意嵌套Txn。64操作/比较综合上限，协议预留14项；记录64KiB、mutation与协议总预算256KiB。RangeEnd≤1024，在复制前拒绝超限。payload及Cmp oneof私有深拷贝，摘要固定。
- BeginStage生成新attempt UUID及独立Lease guard；guard创建unknown不返回能力，不重建原guard。Commit比较永久identity/restore以及原guard的value/Lease/create revision，业务状态与committed receipt原子持久写入。
- Resolve以永久aborted marker与commit竞争，缺少receipt或业务CAS冲突仍是unknown，不能猜测aborted。receipt格式/reference/digest/永久性不符时failclosed。StageReference可由新Backend仲裁，不可恢复旧提交能力。
- Release仅撤销原Lease，持久receipt和业务状态保留。没有暴露receipt GC，亦不推断外部效果已撤回。

## 审查与修复

独立spec与quality复核通过，当前无剩余必要P1/P2。metadata/ref无界问题通过客户端上限和Grant前receipt编码校验修复；RangeEnd超限测试先出现RED（1025 bytes仍被接受），加复制前验证后GREEN。全局NOSPACE测试ownership问题在独立故障测试提交修复，不属于本协议公开API。

## 实际验证

`bash scripts/test-etcd-state.sh -v`最终完整运行：etcd3.6.15，三distinct members，`go test -race -count=1`通过，9.262s，无集成skip。该运行包含下一提交的故障测试，本提交仅纳入stage原子/CAS/receipt/identity/预算测试。

验证了control、snapshot、workspace owner、runtime index四个业务key与receipt同revision且无Lease；16并发owner CAS仅一胜者；abort后原attempt不能写；重试使用新attempt；receipt经Revoke保留；creator关闭后新Backend凭reference仲裁；corrupt JSON/digest/Lease不重写；CAS冲突保持unknown直至仲裁；mutation不受caller后续变更影响。

`go vet ./internal/storage/state/etcd`通过。最新全仓测试结果与故障注入明细在最终首批验收记录补全。

## 范围限制

仅元数据事务原语。workspace/sandbox领域Repository、request/operation/pool、runtime launcher、partition调度、collector/sync、部署/drain与Redis删除尚未实施。本批不能开启新allocator，持久stage回执GC及容量验收也仍待后续实现。
