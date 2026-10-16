# 真实 cleanup 消费者与持久32槽实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. One fresh sole author and one unique complete Task reviewer; Root executes tools and commits.

**Goal:** 用真实cleanup消费者消费明确的永久Task引用，在全cell持久32槽约束下完成既有物理关闭、同步、终止、安全释放及确切恢复。

**Architecture:** 永久槽协议属于etcd存储内部，实际阶段编排消费现有原TaskClaim与已验证runtime/worker依赖。明确引用通过原due actor的小型有界mailbox接入，共用其原partition/session/Claim份额；actual jobs与最终关闭同一生命周期，不做自动索引发现。只读preflight先决定最小清晰API及包边界、共享预算、终态独立结算和实际join图；之后用完整真实消费者验证这些原语。

**Tech Stack:** Go1.25.6、etcd3.6.14、原controltransport/launcher KernelBoundary、enrolled runtime与exact Kubernetes cleanup。

**Spec:** docs/superpowers/specs/2026-10-17-etcd-cleanup-executor-design.md，细化已批准主方案，不新增外部系统或部署范围。

## Global Constraints

Named spec的Binding constraints全部适用。原256分区/4Relist/20MiB/s/100claim-s/持久32任务，原Lease和未知效果不放宽；64native节点/128KiB请求/256KiB响应；明确Task真实重验，未决永不随Lease自动归还。Root独占Go/Git/Docker/gofmt，所有修改apply_patch；保留Sentinel，无CLOSED OWN/pair访问。已有功能分支codex/etcd-state-management，无默认切换或生产容量宣称。

## Review Focus

- 原release事务已满64节点，后续独立结算必须绑定确切物理终态，不能用本地返回nil/404/Lease消失填补证据。
- 相同Task跨进程/恢复不能多占槽；旧occupation和旧ACK不能返还新槽。unknown写始终保存原attempt与恢复位置。
- due actor已占有partition和预算；新消费者不能把另一套重叠worker或每副本32当作全cell预算。
- 实际归档/同步/observer/provider阶段发生取消、丢ACK、原权限变化或Close时，既有不可变结果与真实工作join仍须可恢复。
- 本地ordinary/enrolled支持边界、真实KernelBoundary及mounted依赖必须明确；不支持profile不能伪装为成功，不强行拆包导出权限。

## Task 1: 可调用的真实清理执行单元

**Files:** 只读preflight冻结新增的focused persistent-task-slot协议文件、实际cleanup执行编排文件和必要的现有Task/lifetime接线；新增focused tests及一个真实Linux组合用例。新导出API/永久key/record/交易模板和包边界必须先交付preflight，不允许实现时猜测。冻结后将exact paths记入当前ledger；无无关重构或松散后写索引。

**Consumes:** 现有真实Task/cleanup-intent、ClaimTask/Renew/Release、CloseData/Quiescence/dual-drain、sync/archive/finalize、TerminateTaskRuntimeLocal及ReleaseTaskWorkspaceOwnerLocal、KernelBoundary/transport/provider；常驻due消费者验收c6cee0f作为原共享权限约束，不重做。

**Produces:** 实际明确Task引用消费者与其生命周期结果、持久32执行槽及可恢复终态结算。精确函数名/签名、支持profile、目标配置和原权限关系是Step1强制交付；不是一个未被真实消费者使用的slot API。

- [x] Step1 read-only preflight：当前ledger Rulings1–6及精确preflight冻结原actor mailbox、两行CAS持久32、64节点受控claim、run/join及冷未join保留、全部新旧consumer请求128KiB预检，并补齐实际producer/snapshot2、原幂等兼容和缓存记账；源码尚未实现。首次只授权producer tests-only，所有产品冻结至Root RED/audit。
- [x] Step2 实际producer/snapshot2已实现并定向验证：AcquireIntent可选LocalCleanupPlan、严格版本/双摘要、旧v1默认重试兼容、深复制/完整cacheweight及真实PlainCold显式local workspace接线。Root验证56个状态层+12个创建入口测试组host/race通过、两包Linuxvet通过；真实行为RED后修复opaque null载荷与每位cache读取者的placement绑定，全部输入/工具/归档及测试资源清理已核对。范围和日志限制见2026-10-17-etcd-cleanup-snapshot-verification.md；不代表本Task或物理cleanup消费者完成。
- [ ] Step3 永久记录、占用/轮转CAS、原预算与Claim复用和22类142Txn/5Lease预检已实现；在首条真实链中进一步验证了两次正向managedGrant、实际run/join及release proof绑定的独立结算。首片19组、本片47组及消费者44组定向host/race与Linuxvet通过；记录分别见slot、wire、consumer验证报告。跨实例33条真实Task与恢复仍待验收，不能记作完整持久32执行能力。
- [ ] Step4 明确Task消费者与原actor mailbox已接通关闭排空、归档同步、两Claim、终止、安全释放及真实join；首条Linux complete及原存活job未知progress/join修复通过，见consumer、progress-join验证报告。cold joined已有真实release时的独立结算通过，见terminal验证报告。archive-bound且旧Run joined后的原归档续接通过48组host/race、Linuxvet及真实双worker恢复，见2026-10-17-etcd-cleanup-bound-verification.md。其他未知阶段恢复、后续native场景及deployment loader仍未完成，不把局部通过记为整Task完成。
- [ ] Step5 新Linux complete、release-settlement-recovery与bound-recovery已通过，并核对独立源码/工具/输入/真实退出与清理；observer-ack-recovery、persistent32/33真实Task及close-join仍必做。不是全部旧矩阵、全Pod32容量或生产环境切换。
- [ ] Step6 唯一完整Task spec+quality review从immutableBASE覆盖所有切片，same-pair限定修复及范围验收。关闭本Task后继续实际pending发现/共享actor/worker角色与完整迁移，不停止。
