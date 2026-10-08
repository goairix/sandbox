# Canonical workspace sync plan Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use subagent-driven-development for the sole implementation task and independent review.

**Goal:** Bind actual workspace target and exclusion/deletion semantics into original native final-sync reservation metadata.

**Architecture:** A pure immutable canonical plan produces the existing TaskFinalSyncPlan digest. One checked reservation facade verifies original task identity before reusing accepted native reservation. No provider effects or executable capability.

**Tech Stack:** Go1.25.6, existing WorkspaceIdentity/framedDigest, storage provider constants and etcd native client.

**Spec:** `docs/superpowers/specs/2026-10-08-workspace-sync-plan-design.md`.

## Global Constraints

Branch codex/etcd-state-management. All source/test/report/format edits apply_patch. Only four new paths below; existing fixtures and accepted APIs read-only. Fresh sole author, sole independent reviewer; no nested agents, CLOSED OWN reads, duplicate task dispatch, old selector replay or broad testing. Exact source commit under this OWN fcntl LOCK_EX covering empty-index/add/staged-exact-path/check/commit/post-empty. No Sentinel/dependency changes, credentials output, production/cloud/trust/deploy/push/merge/Redis removal. Bounds256 exclusion entries/4096 bytes per entry/16384 bytes total/depth128, native64nodes/64KiB record/256KiB mutation unchanged. No per-idle-sandbox activity.

## Review Focus

- Caller mutates/reorders supplied exclusions: original immutable plan unchanged; semantically identical valid policies hash identically.
- Wrong storage identity/bucket/prefix/provider or deletion/exclusion policy: canonical digest changes; mismatched actual workspace refuses before native/query effects and does not consume reservation.
- Exclusion prefix lookalikes, literal wildcards, directory suffixes and malformed paths: preserve exact valid semantics, reject traversal/invalid resource input without normalization widening.
- Zero/inconsistent plan, invalid/cross-task final-sync record or opaque digest: no structural match or reservation authority.
- Lost native response or repeated changed plan: reuse accepted conservative metadata outcome and original one-shot conflict behavior, never create sender/settlement proof.

## Task 1: Canonical plan and checked native reservation

**Files:** Create `internal/storage/state/etcd/task_workspace_sync_plan.go`, `task_workspace_sync_reservation.go`, `task_workspace_sync_plan_test.go`, `task_workspace_sync_plan_native_test.go`. No existing source/test edits without Root ruling.

**Consumes:** WorkspaceIdentity.validate/framedDigest; TaskFinalSyncPlan; original PreparedTaskQuiescence seals and pinned task; Backend.ReserveTaskFinalSync; storage provider constants. Read fixture definitions from task_quiesce_effect_test.go, task_final_sync_reservation_native_test.go, task_archive_command_native_test.go only as needed; no corresponding old test selectors.

**Produces:** Exact types/functions/method signatures specified in spec; pure canonical plan and native checked facade. No plan serialization/loading, generic guard getters, effect callbacks, arbitrary target URLs or artifact IO.

- [x] Write and run compiled meaningful RED for immutable input/canonical policy or cross-workspace refusal, with callable scaffold rather than undefined symbols; preserve raw log.
- [x] Implement bounded input validation, copied/sorted/deduplicated/redundant-parent-normalized exclusions, versioned framed digest, validated FinalSyncPlan/MatchesAttempt/Excludes. Pin spec limits and provider constants without hidden callbacks or activity.
- [x] Implement exact reservation facade, original seal checks and pinned workspace comparison under claim mutex before delegation. Preserve accepted native unknown/one-shot semantics and error contracts; do not duplicate reservation algorithm.
- [x] Add new host/native behavior assertions for all five review focuses, actual native reserved digest and original mismatch-then-correct admission. Reuse acknowledged-only cleanup before first inherited Release; preserve iteration failures.
- [x] Run scoped host/native race using `go test -race -count=1 ./internal/storage/state/etcd -run '^TestTaskWorkspaceSyncPlan' -v` and `bash scripts/test-etcd-state.sh -run '^TestTaskWorkspaceSyncPlan' -v`, package vet and Linuxarm64CGO0 compile. Format using apply_patch (gofmt diff inspection only), exact diff check. Record raw log hashes/exits and exact current/committed path hashes in OWN report/evidence.
- [x] Self-review and commit exact source paths promptly with fcntl gate; independent spec+quality review and Root tracked acceptance/status. Advance to original consumer/per-wire intent work; no overall migration/capacity claim.
