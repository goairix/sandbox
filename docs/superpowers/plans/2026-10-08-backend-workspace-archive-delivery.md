# Original Backend workspace archive delivery Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use subagent-driven-development to implement this task and its sole independent task review.

**Goal:** Deliver one original current-fenced archive command and receive its bounded verified artifact while allowing current claim renewal and physically joining shutdown.

**Architecture:** Optional Backend-owned private spool admission precedes one-shot transport. Private pre-open/pre-send guards and an active-only native liveness owner join the accepted command and receiver; a sealed original result retains quota until Close. No claim mutex spans transport or joins.

**Tech Stack:** Go1.25.6, existing etcd native client/protobuf and pinned controltransport.

**Spec:** `docs/superpowers/specs/2026-10-08-backend-workspace-archive-delivery-design.md`.

Activate only after command source90d23d0 passes independent review and Root acceptance. Parent production/scheduler/provider/Redis migration remains open.

Review fix round1 additionally authorizes task_archive_spool.go solely to extract the existing pure bounds predicate for both callers; no bound/error/filesystem behavior changes or previous-selector replay. This expands final union to eleven source paths, while each fix commit stages only actually modified paths.

## Global Constraints

Authorized branch codex/etcd-state-management; sole fresh author followed by sole independent reviewer. All source/test/report/format edits apply_patch. Exact-path functional commit with empty index before/after under this plan OWN fcntl git-commit.lock covering add/check/commit. No unrelated Sentinel changes, closed OWN reads, old-selector replay, nested agents, dependency changes, push/merge/deploy/trust installation/Redis removal. Existing64nodes/64KiB records/256KiB mutations; optional0700 empty bounded private spool; one original send attempt; fixed session deadline. No per-idle-sandbox actor/timer/watch/scan. Parent whole-branch review at final migration completion.

## Review Focus

- A slow authenticated handshake or canceled pre-send guard must emit zero command bytes, and no retry may mint a second send attempt.
- A legitimate renewed claim must remain usable past its old deadline while an unrenewed or failed-renewal claim cancels and physically joins the receiver.
- Changes after Send, including restore/ABA/new operations, must prevent returning a usable artifact even if the remote signature and TAR are valid.
- Release/Backend.Close must not deadlock behind the receiver or report a physical join merely because context was canceled.
- Quota rejection and uncertain cleanup must preserve exact original accounting and not create activity proportional to existing sandbox N.

## Task 1: Original guarded long-session delivery

**Files:** Modify `internal/storage/state/etcd/client.go`, `task_claim.go`, `task_archive_receive_types.go`, `task_archive_receive.go`. Create same-directory `task_archive_delivery_options.go`, `task_archive_delivery_types.go`, `task_archive_delivery.go`, `task_archive_delivery_guard.go`, `task_archive_delivery_test.go`, `task_archive_delivery_native_test.go`. Existing command source/fixtures may be read, not edited without a concrete Root ruling.

**Consumes:** `PrepareTaskWorkspaceArchive`'s exact original known-successful Prepared; private `newTaskArchiveStore/reserve/receive/close`; `taskArchiveComparisons` validated immutable revision pins; existing RenewTaskClaim; sealed controltransport Destination and Session.

**Produces:** `TaskWorkspaceArchiveSpoolOptions`; optional `Options.TaskWorkspaceArchiveSpool`; `Backend.ReceiveTaskWorkspaceArchive(context.Context,*PreparedTaskWorkspaceArchive) (*ReceivedTaskWorkspaceArchive,error)`; original result `Close() error`; private pre-open/pre-send guard callbacks and active owner lifecycle. No provider/runtime termination effects.

- [x] Add meaningful compiled behavior RED for absent spool/invalid options/no duplicate original delivery, not unresolved symbols; preserve raw output. Constructor validation must neither open spool nor invoke operator callbacks.
- [x] Implement optional immutable options and actual store lifecycle after native namespace verification, failed-constructor cleanup, sealed original received result and idempotent physical Close. Reuse accepted limits and directory lock without broad refactoring.
- [x] Implement one-shot original admission and pre-reserved quota, fresh native comparisons/full-prefix emptiness and trusted original window before Open and immediately before Send; use private receiver seams without accepting externally supplied Session. Keep fixed caps and exact protobuf preflight.
- [x] Implement active-only original monitoring/latest published claim deadline, explicit Renew failure/Release cancellation and joins outside mutexes, bounded periodic native checks, post-receive current authority check, and shared Backend shutdown before client.Close. Long receive must allow successful caller renewal beyond the old deadline without extending ticket/session times.
- [x] Add new `TestTaskArchiveDelivery...` host/native tests covering all five review focuses, actual framed pinned TLS receive and quota/constructor cleanup. Reuse real native and producer fixtures definitions read-only; do not execute accepted original selectors. Scope iteration before final checks.
- [x] Run final new-selector host/native race (`scripts/test-etcd-state.sh -run '^TestTaskArchiveDelivery' -v` for native), package vet and Linuxarm64 CGO0 test compile. Preserve raw log hashes/current+commit exact path hashes, self-review and commit a coherent functional change immediately after checks pass. Sole independent spec+quality review then Root tracked acceptance/status; immediately advance to real provider per-wire intent/settlement work, not production completion.
