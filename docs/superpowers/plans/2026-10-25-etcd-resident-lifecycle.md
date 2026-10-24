# Etcd Resident Lifecycle Capacity Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Keep ordinary long-lived sandboxes from causing a full etcd record read on every lifecycle tick, while retaining the existing fenced cleanup and honest per-cell capacity limits.

**Architecture:** Keep `ActiveSandboxRecord` authoritative and its existing random-cursor index for administrative scans. Add transactionally maintained, time-ordered discovery for ordinary records and a separate workspace-ID index; use these only to select candidates, then reload and fence the record before any lifecycle effect. Active sync/FUSE workspaces remain a separate capacity class because each holds a controller lease; no new actor, refund ledger, per-sandbox watch, or credentials protocol is introduced.

**Tech Stack:** Go 1.25, etcd v3 transactions, existing `state.ActiveSandboxRepository`, current `Manager` lifecycle coordinator.

**Spec:** `docs/superpowers/specs/2026-10-24-etcd-state-replacement-design.md`

## Global Constraints

- Fresh deployment only. Do not migrate historical Redis keys or old sandboxes.
- An index hit is a scheduling hint, never proof of expiry, authority, physical termination, or refund.
- Keep the original controller, operation, generation, revision, and runtime UID fences.
- Do not increase scan intervals or hide failed cleanup to manufacture a capacity result.
- Reuse the shared Go cache. Stop this slice if it requires a parallel scheduler or broader runtime/FUSE protocol changes.

## Review Focus

1. TTL shortening must move a candidate earlier in the same transaction as the snapshot update; a lost ACK must not leave an unindexed live record.
2. TTL extension may cause an old candidate to be read, but must not destroy the newer sandbox snapshot.
3. Destroying and cleanup-pending records must remain discoverable after a crash, including when a bounded wake queue drops the ID.
4. Failed cleanup candidates must not starve later candidates with a larger due key.
5. Workspace controller renewals are measured separately; a faster ordinary due scan is not evidence for 100k sync/FUSE workspaces.

---

### Task 1: Durable ordinary due index

**Files:** `internal/storage/state/active_sandbox.go`, `internal/storage/state/etcd/active_record.go`, `active_scan.go`, the record mutation files under `internal/storage/state/etcd/active_*.go`, and focused `active_due_test.go`.

**Interface:** Add `state.ActiveSandboxDuePage{Records []ActiveSandboxRecord, Cursor string}` and optional `state.ActiveSandboxLifecycleIndexRepository` with `ScanDue(ctx context.Context, dueBefore time.Time, cursor string, count int64) (ActiveSandboxDuePage, error)` and `ScanWorkspace(ctx context.Context, cursor uint64, count int64) (ActiveSandboxPage, error)`. The string cursor is an opaque, ordered due-index key; an empty cursor starts or completes a pass. Keep the existing `Scan` contract unchanged for release drain and other administrative callers.

- [ ] RED with a real etcd: publish/activate, TTL shorten/extend, BeginDestroy, checkpoint, workspace attach/detach, delete, lost ACK, competing revision, and restart all preserve the correct candidate set.
- [ ] Derive the candidate time from the persisted snapshot and phase in one small storage helper. The helper parses only fields needed for scheduling; it never decides whether an effect is authorized.
- [ ] Move old/new due and workspace keys in the same existing record transaction. Keep a unique tie-breaker and bounded pages; compare old keys before deletion. A record may be in the workspace index or the ordinary due index, according to its current snapshot/phase, but never silently absent from both when recovery work is due.
- [ ] GREEN, race, vet, comments/package review, then commit this repository change before touching the coordinator.

### Task 2: Coordinator uses due candidates without losing recovery

**Files:** `internal/sandbox/lifecycle_coordinator.go` and focused tests. No new service or goroutine per sandbox.

- [ ] RED: 10k far-future ordinary records require no full record scan on a normal tick; an expired record, a durable destroying record, and a lost workspace controller still reach the existing `reconcileActiveLifecycle` path.
- [ ] Process due pages with a bounded fair cursor so one repeatedly failing cleanup cannot starve later IDs. Reload each candidate and retain `BeginDestroyExpected` and controller fencing.
- [ ] Preserve one full startup scan for recovery and use the workspace index on later ticks. A repository missing the optional lifecycle-index interface keeps the baseline periodic full scan behavior.
- [ ] GREEN against real etcd, restart/ACK-loss and existing sandbox regressions, then commit.

### Task 3: Measure and set a truthful cell limit

**Files:** `docs/superpowers/reports/2026-10-24-etcd-capacity-and-release.md` plus bounded benchmark inputs only.

- [ ] Measure 10k and 100k ordinary records with realistic snapshots and TTL distribution, then concentrated expiry, two API replicas, three-member fault, etcd DB/QPS, p95/p99, and cleanup lag.
- [ ] Measure sync/FUSE controller renew RPC rate and memory as a separate class. The current five-second renewal implies about 20k RPC/s at 100k workspaces before other traffic; do not infer support from the ordinary result.
- [ ] Publish the accepted per-cell limit and necessary cell count only from measurements. If 100k cannot pass, report the supported bound and leave the gate failed.

**Stop gate:** If Task 1 cannot keep every record/index transition atomic within etcd's transaction limits, stop before product code grows further and report the exact transition and minimal alternative. Do not silently fall back to a stale in-process timer.
