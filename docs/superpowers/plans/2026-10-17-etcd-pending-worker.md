# Pending cleanup 自动发现实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. One fresh sole author per Task, one unique complete Task reviewer. Root alone executes Go/Git/Docker/gofmt and commits.

**Goal:** 永久待办经原共享 DueWorker 自动进入已验收 cleanup 执行器，具有有界发现、恢复及实际关闭。

**Architecture:** 原子待办证据、固定 revision 有界分页和原 actor 的有限轮次组成单向发现管线；候选不能赋权。保留已验收物理协议和全 cell 原预算。独立进程角色若可独立验收则另立后续任务。

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

- [ ] Step1 read-only preflight：冻结入口/路径/原子键、所有 producer 与结算事务预算、历史兼容、固定 R/游标/重建、缓存计费、退避公平性、Kernel与实际 join 图，Root裁定后才授权测试。
- [ ] Step2 tests-only 写行为或缺 API RED，Root执行并审计；最小实现原子待办与有界读取，定向 host/race/vet 后小步提交。
- [ ] Step3 接入原 actor 自动发现与 registry，验证坏任务/容量/预算/续租/due公平性及重建，不新增竞争 session；定向验证后提交。
- [ ] Step4 真实 Linux 自动发现完整物理链及 restart/ACK故障组合，严格输入/运行时/真实退出/清理审计，保留失败；完成可调用 mounted 自动组合并提交。
- [ ] Step5 唯一完整 Task spec+quality 审查，原作者修复与限定复核、范围验收；继续下一生产角色/迁移单元。
