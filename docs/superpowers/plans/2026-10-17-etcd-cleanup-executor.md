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
- [x] Step3 永久记录、占用/轮转CAS、原预算与Claim复用和22类142Txn/5Lease预检已实现；真实链验证了正向managedGrant、实际run/join及release proof绑定结算，slot、wire、consumer报告保留原验证。四Backend、原100份预算和同cell33条真实生产Task的32槽仲裁、原候选ACK未知、ABA和失租保留已通过host/race及Linuxvet，见persistent32报告。公开请求32注册/第33拒绝也通过真实Linux验证，见close报告；不能记作32个物理任务并发或完整容量。
- [x] Step4 明确Task消费者与原actor mailbox已接通关闭排空、归档同步、两Claim、终止、安全释放及真实join；首条Linux complete及原存活job未知progress/join修复通过，见consumer、progress-join验证报告。cold joined已有真实release时的独立结算通过，见terminal验证报告。archive-bound续接和local-complete后observer ACK丢失的Query-only恢复均通过真实双worker验证，见bound、2026-10-17-etcd-cleanup-observer-verification.md。deployment loader通过严格配置/四用途issuer host/race及真实mounted Open→Execute→Close、部分启动失败/同份额重开验证，见loader报告；真实时钟暴露的终止窗口缺陷已独立提交7f0d95f。早期joined closed/quiescent progress ACK丢失后，原历史Query→新Run/两Claim→fresh dual-drain及完整链通过，见2026-10-17-etcd-cleanup-early-verification.md。claimed/pending-close 与 closed/pending-quiescence 的业务回执已存在、progress请求未提交恢复，已通过定向3组83子例host/race及真实双worker，见2026-10-17-etcd-cleanup-pending-verification.md。cleanup-complete owner-held直接释放恢复已通过真实双worker（1新Claim、0归档/provider/bridge、原5组history保持）及定向4组110子例host/race，见2026-10-17-etcd-cleanup-owner-held-verification.md；完整Task及限定修复轮已通过Step6审查。
- [x] Step5 新Linux complete、release-settlement-recovery、bound-recovery、observer-ack-recovery、close-join、mounted-loader和closed/quiescent-progress-recovery已通过，并核对独立源码/工具/输入/真实退出与清理；跨Backend真实33Task持久槽仲裁在真实三节点host/race验证。公开mailbox32/33、Stop/Backend.Close本地实际等待和原生退出见close报告；mounted真实挂载/两Claim/borrowed Kernel见loader报告；早期恢复12组98子例host/race、新43/12节点wire、双进程进度ACK丢失及normal/bound直接影响回归见early报告。pending历史未提交场景已通过最新双worker及完整原Pending/旧history不变检查，见pending报告。owner-held真实恢复、observer/bound/mounted直接影响回归和4组110子例host/race已通过，见owner-held报告；完整Task及限定修复轮审查均已通过。不是全部旧矩阵、全Pod32容量或生产环境切换。
- [x] Step6 唯一完整Task spec+quality review已覆盖immutableBASE至6519d28的18个提交，提出2项Important：完整wire拒绝晚于永久占槽、目标文件IO持actor owner。两项已修复并通过13组43子例host/race、Linuxvet及7场真实native；原审查员已限定复核通过，I1/I2关闭、Spec与Quality均Approved、无新增或未关闭C/I/M，见2026-10-17-etcd-cleanup-preflight-fix-verification.md。范围验收见2026-10-17-etcd-cleanup-executor-acceptance.md。本Task已完成，继续实际pending发现/共享actor/worker角色与完整迁移，不停止。
