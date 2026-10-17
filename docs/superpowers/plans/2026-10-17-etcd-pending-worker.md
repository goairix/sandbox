# Pending cleanup 自动发现实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. One fresh sole author per Task, one unique complete Task reviewer. Root alone executes Go/Git/Docker/gofmt and commits.

**Goal:** 永久待办经原共享 DueWorker 自动进入已验收 cleanup 执行器，具有有界发现、恢复及实际关闭。

**Architecture:** 原子待办证据、固定 revision 有界分页和原 actor 的有限轮次组成单向发现管线；候选不能赋权。保留已验收物理协议和全 cell 原预算。独立进程角色若可独立验收则另立后续任务。

本任务限定有限 pending 轮询，pending Watch R+1 衔接和独立进程角色保留为后续明确交付；不宣称完整调度时延/性能。旧任务回填要求旧写入者已停止，不支持混合旧版持续无索引写入。

**Tech Stack:** Go1.25.6、etcd3.6.14、原 native wire 与 launcher KernelBoundary。

**Spec:** docs/superpowers/specs/2026-10-17-etcd-pending-worker-design.md；沿用已批准主方案。

## Global Constraints

256 分区/4 Relist/20MiB/s Range/100 claim-s/持久32任务；64 native 节点、128KiB请求、256KiB响应；原64MiB/10000keys worker预算。未知不可自动释放。所有编辑 apply_patch，AGENTS注释/分段/包边界审查。Sentinel及CLOSED OWN/pair不可访问。无默认切换、push或部署。

## Review Focus

- Task 创建已提交但 ACK 丢失：永久待办仍与它同事务存在，可由替代 worker 发现。
- 已有任务缺索引或历史持续增长：兼容路径明确且限速，不把反复全历史扫描当有界待办发现。
- 终态索引移除遇到 ABA/丢 ACK：原 exact fences 和满64节点 owner-release 不削弱。
- 坏任务、32槽饱和或未知效果：保留证据、有限退避，后续任务及原 due/renew 仍能进展。
- compaction、旧游标、失权和 Close：视图失效拒绝调度、重新计费，真实 join 后释放依赖。

## Task 1: 永久 pending 发现接入真实共享执行器

**Files:** Step1 只读 preflight 冻结精确路径、键/codec/事务形状及内部接口；之前禁止修改产品和测试。预计职责为 task producer 原子索引、focused pending page/checkpoint、原 due actor 接线和必要部署配置；不扩展已验收物理链。

**Consumes:** 已验收 StartDueWorker/ExecuteCleanupLocal 和原 enqueue/registry、workerCall/预算、永久 Task/cleanup-intent/slot/Run/release 证据。精确内部接口由 Step1 源码证据冻结。

**Produces:** 从持久任务自动发现到原执行器的完整可测行为及严格配置、恢复/退避/关闭状态；不是未被消费者使用的索引 API。

- [x] Step1 read-only preflight：已冻结原子pending、原worker预算/关闭、历史兼容和终态认证边界。历史停旧writer、不可变截止点/可更新读取视图/持久frontier、有限轮询及Watch/独立角色后续范围已明确；具体切片接口与原生尺寸须继续源码核对并验证。
- [ ] Step2 原子pending、80条固定视图分页、回填检查点、平坦intent历史分页及共同出生认证已实现，分别通过定向host/race和Linuxvet，详见2026-10-17-etcd-pending系列验证报告。受控签名释放调用、回填P/B原子推进与完成转换及Linux合法配置模型回归也已实现。新任务终态pending独立删除已实现并通过定向host/race/Linuxvet；签名成功删除、ABA及未知结果真实组合仍待Step4，不能将本步或整个Task记为完成。
- [ ] Step3 原 actor 自动发现/回填/终态删除与 registry 接线已实现；host/race/Linuxvet/build通过，新增并修复单Range饥饿回归。原会话/预算保持复用，见自动循环验证报告。真实物理配置启动、32/33尾部恢复及替代/压缩组合仍待后续验收，不能将本步完整记为通过。
- [ ] Step4 四个新真实Linux自动物理链/独立role/P ABA/P删除丢ACK组合已通过并提交9631088，完整运行及清理审计见automatic-native验证报告。仍需真实etcd分页/压缩、原Task创建丢ACK后积压接管、持久32槽及第33项恢复等针对性补验，不能将每场一条Task当完整矩阵。
- [ ] Step5 唯一完整Task审查在9631088发现I1回填阻塞、I2结果丢失、I3真实覆盖不足。I1修复acda95c与I2修复fd92f9a经同审查员限定复核，无新增代码问题，I1已关闭；I2代码已解决但真实物理恢复/关闭证据待补，I3仍开放。全部关闭后才范围验收。
