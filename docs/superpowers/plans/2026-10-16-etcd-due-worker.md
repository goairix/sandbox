# Resident due worker implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. One new sole author and one new sole whole-Task reviewer; Root executes verification and Git.

**Goal:** Run the real bounded expiry consumer with recoverable progress, continuous fixed-revision discovery and original-worker lifetime ownership.

**Architecture:** An owned worker actor coordinates original session renewal, partition/budget use and the existing due page/admission APIs. Permanent diagnostic checkpoints and bounded native Watch replay preserve discovery continuity; only actual admission transactions authorize metadata changes.

**Tech stack:** Go1.25.6, pinned etcd3.6.14 client/native, existing worker control and enrolled v3 due protocol.

**Spec:** `docs/superpowers/specs/2026-10-16-etcd-due-worker-design.md`, implementing the approved master scheduler design.

## Global constraints

All Binding constraints in the named spec apply verbatim. Existing feature branch `codex/etcd-state-management`; record immutable BASE only after the current due-discovery Task closes. No source edits before exact read-only preflight/API and Root tests-only RED gates. Root alone Go/Git/Docker/gofmt; apply_patch for edits. No closed OWN/pairs, Sentinel document, duplicate authors or production enablement claims.

## Review focus

- Late writes below a persisted frontier and events spanning list/Watch/window rollover must remain reachable.
- Unknown metadata results must retain the original attempt and block covered progress; no new-attempt retry from a diagnostic cursor.
- Native Watch buffering, fragment assembly and queue costs must be bounded before allocation and joined on release/Close.
- Early candidates and no-credit waits must survive restart and allow original session renewal without a busy loop.
- Huge overdue sets, compaction and invalid caches must expose lag and bounded rebuilding, never full-sandbox runtime reads or false readiness.

## Task 1: Real resident expiry consumer

**Files:** Focused new `internal/storage/state/etcd/due_worker*.go`, `due_checkpoint*.go`, `due_watch*.go` and tests; narrow existing due page/worker lifetime/Backend.Close integration only where exact preflight establishes need. One new `due_worker_native_linux_test.go`; operator documentation. Freeze exact files/interfaces in current Task ledger before tests. Do not refactor unrelated accepted protocols.

**Consumes:** `OpenWorkerSession`/`RenewWorkerSession`, original partition and Range/relist/claim shares, `ReadDuePage`, `AdmitExpiredTask`, existing exact Stage outcome resolution and Backend.Close lifetime mechanisms.

**Produces:** A real explicitly started/stopped worker, bounded synchronized/lag status and fenced persistent diagnostic scan progress. Exact exported signatures and strict schema/version/key layout are the first preflight deliverable, not permission to guess them during implementation.

- [ ] Step1 read-only preflight: freeze exact API/schema/Watch transport and limits, renewal/owner integration, checkpoint/replay safety and transaction bounds; Root rules on conflicts before tests.
- [ ] Step2 tests-only durable progress/restart/unknown and actual page-resume behavior RED; implement the minimal fenced checkpoint/continuation consumer, host/race/Linuxvet, immediate coherent commit.
- [ ] Step3 tests-only bounded actual native Watch/list bridge, fragment/gap/overflow/window/late-insert cases RED; implement on original authenticated connection, physical cancellation/join, verify and commit.
- [ ] Step4 tests-only real resident loop and renewal/backoff/status RED; execute real due/admission transitions, unknown resolution and lifecycle ownership, verify and commit. Do not replace actual work with callbacks that merely assert a counter.
- [ ] Step5 one fresh finite three-member native controller chain and cleanup, operator documentation, verified small commit. No old native matrix replay or large-N performance claim.
- [ ] Step6 whole Task spec+quality review from immutable BASE through all slices, scoped same-pair fixes, tracked bounded acceptance and CLOSED ledger; continue persistent task execution and production migration without stopping.

## Initial interface/conflict scan

| Producer → consumer | Concrete boundary to settle before implementation |
| --- | --- |
| fixed-R page → persistent resume | New authority must validate diagnostic checkpoint and build its own view; no old cursor adoption or newer-header R substitution |
| checkpoint → unknown admission | Only known settled coverage advances; original attempt identity survives loss of acknowledgement |
| listR → WatchR+1 | Late insert below frontier and future candidates must survive crash/window movement |
| Watch transport → bounded cache | clientv3 internal slice can grow; receive size/fragment assembly and original stream ownership need explicit bounds |
| session one-native owner → resident renew | Finite discovery operations and context-aware waits must yield for original renewal, not concurrent reconstruction |
| real controller → Backend.Close | Actual native receives, callbacks and cleanup must return before authority/resources are released |
| Task internal | Durable progress, Watch and actor form one real discovery consumer; physical task hard32/mounted executor remains separate |
