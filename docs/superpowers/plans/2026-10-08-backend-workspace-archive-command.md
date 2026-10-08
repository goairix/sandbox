# Original Backend archive command implementation plan

> For agentic workers: use subagent-driven-development for this task and its sole independent review.

**Goal:** Prepare and persist one original archive command bound to current native etcd authority and the original final-sync reservation.

**Architecture:** Purpose-specific operator signing dependency and original Backend verifier produce a fixed command under exact native claim/dual-drain/final-sync fences. A private original draft survives ambiguous responses without reconstructing dispatch from history. Long receiving and provider effects follow this prerequisite.

**Tech stack:** Go1.25.6, etcd native client/protobuf, existing controlprotocol/controltransport and genuine dual-drain native fixture.

**Spec:** `docs/superpowers/specs/2026-10-08-backend-workspace-archive-command-design.md`.

Base `5bf4188`; source `90d23d0` independently reviewed and accepted. Verification: `docs/superpowers/reports/2026-10-08-backend-workspace-archive-command-verification.md`.

## Global constraints

Existing authorized branch codex/etcd-state-management. Activate only after preceding receiving core acceptance. Sole fresh author and independent reviewer; broad parent review at migration end. All source/test/report/format edits apply_patch. Exact-path small source commit under this plan OWN fcntl git-commit.lock covering add/check/commit; index empty pre/post. No source changes outside listed paths without Root ruling, no unrelated Sentinel changes, no old CLOSED OWN reads, no push/merge/deploy/trust installation/Redis removal, no duplicate implementation task. Lean hash-backed logs, no supply-chain custody framework or replay of accepted old selectors. Existing transaction caps64nodes/64KiB records/256KiB mutations remain.

## Review focus

- Original committed metadata without current claim must yield diagnostics with refusal, never original dispatch reconstruction.
- Slow/canceling signing and clock callbacks must not carry expired preparation authority into later native writes.
- A renewed claim must not revive the old PreparedTaskQuiescence deadline or extend fixed ticket/session times.
- Whole-prefix emptiness must be freshly observed, and native transaction preflight must account both branches/replies without widening shared caps.
- Historical quiescence under a new original claimant must bind present Current lineage while preserving exact old quiescence receipt attribution.

## Task 1: Purpose-specific authority and native original command prerequisite

**Files (exact eight):** Modify `internal/storage/state/etcd/client.go` and `task_final_sync_records.go`; create `task_archive_authority.go`, `task_archive_command_types.go`, `task_archive_command_codec.go`, `task_archive_command.go`, `task_archive_command_test.go`, `task_archive_command_native_test.go` in the same directory. Existing native fixture definitions may be read/reused but not modified; raise a concrete path ruling if indispensable.

**Consumes:** Original ReserveTaskFinalSync draft/PreparedTaskQuiescence, RevalidateTaskQuiescence, current task claim/guard fences, controlled authority clock, immutable command issuer records, original controlprotocol verifier and pinned Destination.

**Produces:** `TaskWorkspaceArchiveIssuer`; `Backend.PrepareTaskWorkspaceArchive(context.Context,*TaskClaim,*PreparedTaskQuiescence,*controltransport.Destination,time.Duration) (TaskWorkspaceArchivePreparationResult,error)`; instance-sealed `PreparedTaskWorkspaceArchive`; diagnostic `TaskWorkspaceArchiveReference` and preparation result; bounded private immutable command codec/native key. A loader if needed returns structural diagnostics only. Exact private field layout is author's routine implementation choice.

- [x] Write meaningful compiled configuration/codec behavior RED and preserve raw log; no missing-symbol-only evidence. Validate issuer typed nil/trust/clock and constructor callback noninvocation.
- [x] Implement exact bounded canonical command codec and original draft/prepared seals; bind original final-sync AttemptID/revision, real current claim, exact old quiescence receipt/context, issuer identity and fixed signed window/session. Reject copied/zero/foreign inputs and invalid durations without native/signing side effects.
- [x] Implement original current-fenced issuer/signature/native command preparation and exact protobuf preflight. Freshly revalidate dual drain/whole operations-prefix emptiness, immutable original pins and runtime/cert coverage around callbacks and native writes. Consume one write attempt before IO; exact original response-loss arbitration only.
- [x] Add new host/native tests for the five review focuses plus success/budget/strict codecs/signature/time/context mutation/response loss/ABA/restore/claim loss. Reuse accepted fixture definitions; run only new `TestTaskArchiveCommand...` and `TestTaskArchiveAuthority...` selectors, never old final-sync/dual-drain tests.
- [x] Focused iteration/self-review before final new-selector host/native race, package vet and Linux arm64 CGO0 compile. Preserve compact raw log hashes and exact committed/current source hashes; commit exact eight paths when checks pass.
- [x] Sole independent spec+quality gate, Root tracked acceptance and overall progress update. Close this unit and immediately continue original guarded long-session sender/receiver with concurrent renewal. Do not claim production migration, actual PID1 network delivery, provider settlement or capacity completion.
