# Sandbox Etcd State Replacement Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在保留基线业务行为的前提下，用 etcd 完整替换新部署的 Redis 状态管理，并验收大量存活沙盒。

**Architecture:** 复用基线的 `state.AtomicStore`、`state.ActiveSandboxRepository` 和 `state.FUSEPoolRepository` 契约。新增 `internal/storage/state/etcd` 实现；业务层只作经证明必需的有界遍历修改，最后替换 `cmd/sandbox` 的存储构造和配置。当前庞大的 etcd 分支仅供选择性参考，不作为合并来源。

**Tech Stack:** Go 1.25、etcd v3 client、基线 Kubernetes/FUSE 接口、现有 Go 测试与隔离的真实 etcd 实例。

**Spec:** `docs/superpowers/specs/2026-10-24-etcd-state-replacement-design.md`

## Global Constraints

- 全新部署，不迁移旧 Redis 数据或旧沙盒。
- 原 HTTP、FUSE、运行时和配额行为作为业务合同；不新建平行服务、actor、账本或凭据协议。
- 大量**已有**沙盒的维护、遍历及集中到期是容量验收内容。
- 共享主仓库 Go 构建/模块缓存；验证产物审计后清理。保护当前主检出的 Sentinel 暂存文件。
- 每个业务闭环独立验证并及时提交；一个工作日无业务进展即执行停损复核。

## Review Focus

1. 请求提交成功但客户端丢失 ACK：读回确认或保留 Unknown，不能重复创建/退款（任务 1、2、3）。
2. 租约失效后旧持有者提交：必须被 revision/lease 栅栏拒绝（任务 1、2、3）。
3. 多副本同时申请同一 workspace：只能有一个有效 owner（任务 3、4）。
4. N 很大或同时到期时：维护请求和遍历有界，不能全库扫描或 N 个长期 watch（任务 4、6）。
5. 应用重启与物理终止确认分离：不能凭 TTL 消失就归还配额（任务 2、3、5）。

---

### Task 0: 基线契约与变更预算

**Files:** Create `docs/superpowers/reports/2026-10-24-etcd-baseline-matrix.md`; inspect `cmd/sandbox/main.go`, `internal/storage/state/{state,active_sandbox,pool}.go`, `internal/storage/state/redis/*.go`, `internal/sandbox/*` 的 Store 调用点。

- [ ] 列出每类 Redis key、TTL、Lua 多键操作、`Keys(pattern)` 调用点、写入者和清理者；标记哪些属于普通 Store、活跃沙盒或 FUSE 池。
- [ ] 对每类列出对应的 etcd key/lease/txn、ACK 丢失处理与现有测试；凡无法对应者列为阻塞，不写产品代码。
- [ ] 确认基线相关 Go 测试可运行并保存命令、结果、输入版本；提交职责表。此门只读产品代码。

### Task 1: 普通 AtomicStore

**Files:** Create `internal/storage/state/etcd/store.go`, `store_test.go`; modify `go.mod`, `go.sum` only for实际所需 client 依赖。

**Interfaces:** Produce a `Store` satisfying `state.AtomicStore`, with constructor options containing endpoints、TLS、namespace prefix、operation timeout, and `Close()`.

- [ ] 先写真实 etcd 测试：Set/Get/Delete、`SetNX`、CAS、CompareAndDelete、Increment 溢出、TTL、租约失效、ACK 不确定结果和按前缀枚举；确认目标失败。
- [ ] 用最小 clientv3 实现；`Keys` 仅接受任务 0 已核对的前缀模式，遇到其他模式明确失败，不假装支持 Redis 任意 glob。
- [ ] 跑该包测试、race 与受影响的 `internal/sandbox` Store 测试；核对 GoDoc、函数分段和包边界，提交一个可复用适配器。

### Task 2: 活跃沙盒仓库

**Files:** Create focused files under `internal/storage/state/etcd/active_*.go` and tests; use `internal/storage/state/active_sandbox.go` as unchanged contract unless task 0 证明必须修正。

**Interfaces:** Produce `state.ActiveSandboxRepository` and基线实际使用的可选 checkpoint、completion、conditional-destroy 契约。

- [ ] 将 Redis Lua 每个状态转换写成“比较前像、事务写入、确定读回”表，先补跨实例冲突、过期 token、ACK 丢失和重启测试。
- [ ] 实现 Publish/Activate/操作租约/Destroy/Controller/Checkpoint/Delete 与有界 Scan；任何多键转换必须在真实 etcd 事务限制内。
- [ ] 跑仓库测试及原创建、销毁、恢复相关业务测试；确认原安全边界，提交活跃仓库闭环。

### Task 3: FUSE 池与 workspace 所有权

**Files:** Create focused `internal/storage/state/etcd/pool_*.go` and tests; only where `Keys` 无法安全映射时修改 `internal/sandbox/{workspace_lease,ordinary_pool_shared,fuse_pool_rollout}.go` 及对应测试。

**Interfaces:** Produce `state.FUSEPoolRepository` and use task 1 的 `state.AtomicStore`.

- [ ] 先固定池状态转移、终止证据、配额 admission、同 workspace 并发申请与 owner 租约失效的真实 etcd 反例。
- [ ] 实现有限事务与按前缀分页的索引；确认列表和计数不会读取全库或漏掉活跃记录。
- [ ] 跑 FUSE 池、workspace 和普通池业务回归；验证同一 sandbox 只释放一次并提交。

### Task 4: 最小生产接线

**Files:** Modify `cmd/sandbox/{main,redis_config}.go`、`internal/config/config.go` 及其直接测试；仅在确有需要时调整 Helm 新部署值。

- [ ] 在新部署配置中声明 etcd endpoint、TLS、namespace 和超时；启动前拒绝缺项或 Redis 与 etcd 混用。
- [ ] 将基线 `main.go` 的所有 Redis 消费者逐一接到任务 1–3 的 etcd 实现；原 handler、manager、runtime 保持同一条业务链。
- [ ] 跑配置/启动测试和真实双副本同 workspace 申请；核对重启后的所有权及可用状态，提交接线。

### Task 5: 自然 TTL 与退款整链

**Files:** 优先只增集成测试与运行脚本；若产品失败，仅修改对应任务 1–4 的最小责任文件。

- [ ] 新建沙盒，记录创建、配额与 runtime 身份；等待真实 TTL，观察原清理流程确认物理终止和唯一退款。
- [ ] 在申请、销毁和退款各一个阶段分别重启 API/worker，验证可恢复或保留 Unknown；不得手工清理/退款使测试通过。
- [ ] 每个失败保留原始记录和明确的责任层；只有真实整链通过才宣布功能完成并提交必要修复。

### Task 6: 大量存活 N 的容量和新部署收尾

**Files:** Create `docs/superpowers/reports/2026-10-24-etcd-capacity-and-release.md`; any benchmark/manifest only for this gate.

- [ ] 以 10k 和目标 100k（若环境不足，写明实测上限）构造有真实 TTL 分布的状态，测常态、集中到期和一次维护故障；记录 key 数、DB、QPS、p95/p99、watch 数、扫描时长、压缩后行为。
- [ ] 根据实测给出单 cell 上限与多 cell 分配规则；未满足目标即报告容量缺口，不凭静态子集宣称通过。
- [ ] 完成新部署 etcd 配置和 Redis 新入口/依赖移除，运行原功能回归与部署 smoke test；记录未覆盖的真实 FUSE/CNI 环境限制，提交最终报告。

## 停损与沟通

每任务最多一个工作日无可验证业务进展；届时暂停新代码，报告具体失败、范围内最小修法和是否需要改变本方案。进度只用“通过的业务闭环/待验证/失败”表示。每个任务按测试先红、最小实现、验证、连贯提交顺序执行。方案经用户审阅后才开始 Task 0 以后的实施。
