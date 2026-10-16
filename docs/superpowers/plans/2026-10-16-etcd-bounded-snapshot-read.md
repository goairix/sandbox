# 有界快照读取实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: subagent-driven-development。一个完整 Task，多次验证后小提交，一个 fresh wholeTask reviewer；连续接续总迁移。

**Goal:** coherent metadata 读取复用有限内存 immutable snapshot，合并有界 miss，并接入现有冷建恢复路径。

**Architecture:** 每 Backend 一个 weighted LRU/有限读取 owner；原 metadata view→快照→原 view确认。缓存不提供健康或执行权限，原历史 LoadSnapshot 行为不改。

**Tech Stack:** Go1.25.6、现有 etcd client、container/list/sync、原 TLS/native fixture。

**Spec:** docs/superpowers/specs/2026-10-16-etcd-bounded-snapshot-read-design.md

## Global Constraints

- 前置 quota-return Task 已验收36a5dd1，本Task immutable BASE为36a5dd13cc45d0287d0beeaa25d7f68ed8efa459；唯一fresh author先完成只读预检再START-GO。
- 单 Backend 默认64MiB缓存，Options显式256KiB–64MiB；16个不同key miss、256个读取调用；独立ErrSnapshotReadCapacity，无队列/perN工作。
- metadata view和最终确认原样使用；snapshot canonical/digest/placement/restore校验；缓存不能代表health/TTL/operation，不能追随并发替换owner。
- Backend Close封准入、取消、join所有新调用/worker再释放资源；原LoadSnapshot历史语义不改，不削弱原安全协议。
- Go1.25.6、apply_patch、Root-only精确小提交，唯一作者/wholeTask reviewer及同pair修复；无nested、无旧OWN/Sentinel/外部部署/依赖升级。
- 新限定验证和独立collector/epoch；actual17/prepost/归档/ELF/script及每次Root native preflight；实际Wait/Serve/IO/transport/container join和精确清理。

## Review Focus

- 缓存命中时restore变化或control更新：必须经权威metadata确认拒绝，不能因省RPC绕过。
- 首个waiter取消：其他waiter正常获取同一次miss；所有waiter离开后worker仍受固定timeout/Close约束。
- 大量不同key/tiny entries：memory、active调用及missworker都有显式硬上界，无无限排队或历史registry。
- Backend Close与cache-hit、miss启动、完成存储交错：不晚准入、不留worker、不在close后发布缓存结果。
- 当前owner释放后旧Create完成重放：历史路径继续可读，不能让新cache路径阻断原安全结算。

## Task 1: Bounded coherent snapshot read with cold recovery integration

**Files:** 新 `internal/storage/state/etcd/domain_snapshot_read.go`（入口/结果/校验）、`domain_snapshot_read_rpc.go`（固定两点profile预检）、`domain_snapshot_cache.go`（加权LRU/有界共享miss）、对应 `_test.go` 与新 `domain_snapshot_read_native_linux_test.go`；修改 `client.go` Options/构造、`domain_read_view_rpc.go`提取已预检后共享dispatch/strict response body、原Backend关闭实现接点、`internal/provisioning/plain_cold_create.go` 的进行中恢复读取。限定collector放本OWN，native selector位于tracked测试源码；可复用tracked collector helper，不复制其逻辑。

**Consumes:** 原 Backend、ReadWorkspaceView/ReadSandboxView/ConfirmSandboxView、原readViewResponse严格校验/原decodeSnapshot、Backend关闭屏障、原plainCold capsule/claim匹配。snapshot profile单独预检65536+4096，旧view4096/4点/4pins契约不变，不扩大readEnrolledPoints协议。

**Produces:** `ReadWorkspaceSnapshot(context.Context, WorkspaceIdentity) (*SandboxSnapshotReadResult,error)`、`ReadSandboxSnapshot(context.Context,string) (*SandboxSnapshotReadResult,error)`；result包含原opaque View与调用者独占Snapshot；`Options.SnapshotCacheBytes`及独立`ErrSnapshotReadCapacity`。

- [x] Step1 Root接受具体result/单Backend私有指针、life.mu唯一锁、16miss/256call、原seal/cancel/join顺序和ongoing-only consumer。现有readEnrolledPoints并非足够严格：Root裁决使用固定snapshot两点profile，提取原view已预检后dispatch+strict shape-before-Size body，旧预检不改；添加最小sharedbody旧view回归。先写新tests/collector，Root审collector并实际RED后写产品，无任意factory。
- [ ] Step2 实际RED后实现Options校验、weighted LRU/clone、有界共享miss与关闭；限定host/race验证实际行为及16/256硬上限。达到提交要求立即小提交。
- [ ] Step3 实现两个新coherent读取入口和真实TLS负例/并发/restore/命中trace，限定native按Root具体preflight执行；只读取指定snapshot/placement，不写元数据。验证后小提交。
- [ ] Step4 接入原cold service进行中恢复，保留历史完成读取；限定service regression及必要native证明新接点确实使用，原claim/capsule/结果语义不变。最终相关vet/来源冻结，及时小提交。
- [ ] Step5 完整Task报告，固定原BASE→最终HEAD一份reviewpackage，一个fresh spec+quality reviewer；同pair修复/限定复审、tracked验收/关闭本OWN，继续业务接线。

Ruling: 本单元落实总方案已有明确缓存与coherent read设计，不新增生产切换或权限。为避免只写未消费的cache，Task包含现有进行中冷建恢复入口接线；历史完成不强行改用当前owner view。前置已验收，当前文档和只读预检不表示本Task完成。
