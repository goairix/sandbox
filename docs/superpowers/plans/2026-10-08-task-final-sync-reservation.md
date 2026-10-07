# Task final-sync reservation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox syntax for tracking.

**Goal:** 在已验证双排空之后持久登记唯一 final-sync producer attempt，保留未知结果且防止新 claimant 盲目重发。

**Architecture:** 原 TaskClaim 私有 draft 固定 UUID/wire/原 prerequisite/deadline。原生 immutable reservation Txn 携带53完整 fences和absence compare；只读仲裁不重构能力。

**Tech Stack:** Go1.25.6、etcd server3.6.15、api/client/client-pkg3.6.14。

**Spec:** docs/superpowers/specs/2026-10-08-task-final-sync-reservation-design.md

## Global Constraints

- 当前 codex/etcd-state-management checkout；不创建重复任务/worktree。
- 不读取、修改、暂存或回退无关 Sentinel Kubernetes兼容计划。
- 所有源代码/测试/规格/计划/报告使用 apply_patch；不使用cat/heredoc写代码。
- 每task一个永久final-sync-attempt，不删除、不覆盖；unknown不变成no-send或aborted。
- 固定原对象/deadline/claim/pins；native写58 literal nodes<=64、wire<=256KiB，record<=8192bytes。没有额外租约、签票、发送效果、控制状态变更或owner释放。
- 不重跑已验收单元；只新增native单元及必要受影响包检查。任何SKIP/诊断分别记录，三成员同host不声称HA/生产/容量。
- 小步提交，精确路径stage和空index检查，OWN/git-commit.lock；不force-add ignored OWN。

## Review Focus

- Write response丢失但真正commit：same original只读确认，新的claim绝不能恢复sender或再次登记。
- Lost response且key absent：unknown保留且本地writeAttempted不可回退，不能再次Put。
- 同bytes Delete/re-Put：known native revision必须拒绝ABA，不把相同JSON当同一reservation。
- 计划声明none：不返回或命名为NoRemoteEffects证据。
- Metadata commit和失租交错：诊断可以committed但不得返回能力；保存deadline固定。

### Task 1: Durable original final-sync producer reservation

**Files:**
- Modify: internal/storage/state/etcd/task_claim.go (仅新增私有draft字段)
- Create: internal/storage/state/etcd/task_final_sync_records.go
- Create: internal/storage/state/etcd/task_final_sync_reservation.go
- Create: internal/storage/state/etcd/task_final_sync_records_test.go
- Create: internal/storage/state/etcd/task_final_sync_reservation_test.go
- Create: internal/storage/state/etcd/task_final_sync_reservation_native_test.go

**Interfaces:**
- Consumes: RevalidateTaskQuiescence(ctx,c,prepared,destination), taskEmptyComparisons, taskCheckWire, pinTaskDualDrain/checkTaskDualDrainPins, immutable task codecs and actual existing native fixture helpers.
- Produces: TaskFinalSyncPlan{Mode,Digest}; diagnostic TaskFinalSyncAttemptReference/Record/Entry and TaskFinalSyncReservationResult{Reference,Outcome}; ReserveTaskFinalSync and LoadTaskFinalSyncAttempt with signatures in Spec. No exported sender/permission reconstruction.

- [ ] **Step 1: RED.** Add host strict codec and original validation tests plus native success/58node/repeatedcall/changedplan/newclaim/response-loss/absent/ABA/pin changes/claim cancellation cases. Compilable temporary all-reject implementation must fail positive reservation assertions against genuine three-member etcd; preserve full raw commands/exits.
- [ ] **Step 2: Implement.** Strict canonical permanent record and fixed-purpose namespace key; one original draft, no-write replay; fresh dual-drain then fenced reservation/only-read arbitration. Record unknown/committed distinction without public authority, no newStage/Lease/remote effect.
- [ ] **Step 3: GREEN.** Run scripts/test-etcd-state.sh -v -run '^TestTaskFinalSync' on new native/host tests; full affected etcd package race once, vet and CGO0 Linux arm64 compile. Read/classify all failures/SKIPs/JSON diagnostics; exact owned compose project cleanup.
- [ ] **Step 4: Commit/report/review.** Full source self-review, small coherent source commit under OWN lock with exact six files; author report/raw evidence in OWN. Root independent task/unit review, address blocking findings, explicitly adjudicate limits and submit tracked verification/plan acceptance separately.
