# Task finalization preflight Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox syntax for tracking.

**Goal:** 为最终同步生产者提供原 prerequisite 的即时只读复核入口。

**Architecture:** 新 API 返回 error，共用已有双排空观察算法。保存的原对象、claim、pins、receipt 和原 deadline 限制本次复核；两次新 runtime query 与一个新原生全前缀空观察确认当前状态。

**Tech Stack:** Go 1.25.6、固定 etcd/client v3.6.15、现有 controlprotocol/controltransport。

**Spec:** docs/superpowers/specs/2026-10-08-task-finalization-preflight-design.md

## Global Constraints

- 在 codex/etcd-state-management 当前 checkout 实施；用户已指定从 feat/workspace-fuse-mount 切出的功能分支继续，不创建重复 worktree 或实现任务。
- 不读写、暂存或回退 docs/superpowers/plans/2026-09-17-sentinel-kubernetes-129-compatibility.md。
- 所有源文件、测试、规格、计划和报告用 apply_patch 编辑；检查已存在文件后修改，不用 cat/heredoc 写代码。
- 保留原 53 compare、55 literal native nodes、完整 operations prefix limit1、新原生观察、当前 mTLS runtime/Boot 与全部不可变 pins。
- 无效果发送、签票、Grant、Put、Delete、owner 释放、最终同步成功或生产迁移完成声明。unknown 保留 owner/pending。
- 不重放已验收 Tasks1–5 或物理 PID1 gate；新三成员集成用例只验证新增入口和必要原接口回归。
- 小步验证后及时提交；精确路径暂存、空 index 前置检查、共享本单元 git-commit.lock；忽略的 OWN 报告不得 force-add。

## Review Focus

- 已保存 prerequisite 后的操作员重建 operation：必须新 full-prefix query 而非缓存 zero；新 birth 按原 decoder 归类 corrupt，不放宽生产 closed admission。
- receipt/reservation 相同 bytes 的 revision 改变：exact immutable pin 必须拒绝。
- 旧 prepared 过期但同 claim 已续租：原 deadline 不延长；入口过期在 c.live 前拒绝，中途超时保留原 lost 策略。
- runtime 只能证明 accepted 或失联：nil/未知/不同 terminal 不能复核成功。
- 原 API 回归与只读边界：提取共同算法不改变 PrepareAfterDualDrain，成功复核不能产生隐藏 mutation 或授权对象。

### Task 1: Original-prerequisite fresh finalization check

**Files:**
- Create: internal/storage/state/etcd/task_finalization_preflight.go
- Modify: internal/storage/state/etcd/task_dual_drain.go
- Create: internal/storage/state/etcd/task_finalization_preflight_test.go
- Create: internal/storage/state/etcd/task_finalization_preflight_native_test.go
- Optional modify: internal/storage/state/etcd/task_quiesce_transport_native_test.go (仅现有 mTLS test helper 的 concurrency-safe response override；保留默认行为，不复制 TLS server loop)
- Create after review: docs/superpowers/reports/2026-10-08-task-finalization-preflight-verification.md (Root)

**Interfaces:**
- Consumes: Backend.PrepareAfterDualDrain(ctx,c,destination), pinTaskDualDrain/queryTaskDualDrainLocked/observeTaskOperationsEmpty/checkTaskDualDrainPins, existing native three-member/mTLS fixture helpers.
- Produces: Backend.RevalidateTaskQuiescence(ctx context.Context, c *TaskClaim, prepared *PreparedTaskQuiescence, destination *controltransport.Destination) error. No returned authority object; future trusted final-sync producer calls it before its own fenced intent/effect boundaries.

- [ ] **Step 1: Write focused failing tests.** Host cases must assert invalid originals/zero evidence/expired saved deadline fail before KV/Dial. Native success counts exactly two new terminal queries and one whole-prefix Range, checks native snapshot immutability, issuer calls unchanged and effects zero. Native failure cases cover newly nonempty operations; same-bytes receipt/reservation revision mutation; claim revoke/cancel at the new Range; current target unavailable, accepted-only or changed terminal; saved deadline versus renewed claim. Reuse fixtures, keep cryptographic target substitution limits explicit.
- [ ] **Step 2: Run meaningful RED.** Use a compilable temporary implementation returning ErrInvalidRecord for all calls, then run the new selector against actual three-member etcd to observe positive-case assertion failure. Retain raw output and actual exit; missing fixture/compile failure does not count as behavioral RED.
- [ ] **Step 3: Implement the specified API.** Extract shared internal algorithm in task_dual_drain.go, compare original pins before first query and saved terminal digest around fresh Range. Limit context by saved deadline and existing claim/request deadlines; validate again before return. Original public PrepareAfterDualDrain follows its old nil-expected path.
- [ ] **Step 4: Run focused GREEN and required checks.** Run new selector plus existing TestTaskDualDrainNativeConjunction and TestTaskDualDrainNativeFinalBarrierMutation once against real three-member etcd; run full affected package race once, vet and Linux arm64 CGO0 compile. Record every SKIP/warning/failure separately, including gated unchanged physical suites. Do not rerun the old combined fixture or broaden to unrelated packages.
- [ ] **Step 5: Commit and report.** Self-review complete diff; commit only the four owned source/test paths and, if needed, the named optional existing test-helper path with feat(etcd): revalidate finalization prerequisites at use. Report complete commands/raw logs/TDD/commit/limitations in OWN/task-1-report.md; Root supplies sole independent review and records each CannotVerify disposition before marking this task accepted.
