# Workspace archive read identity Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox syntax for tracking.

**Goal:** Access actual nonzero-user private workspace files without adding bypass capabilities or changing PID1 management identity outside a narrow verified filesystem-open scope.

**Architecture:** Original source carries private user access policy; original kernel mutex plus locked OS thread protects raw filesystem-only identity setup/open/restore. Silent refusals are read back, and unverified restoration terminates PID1. Existing root-mode archive and its bounds remain intact.

**Tech Stack:** Go1.25.6, x/sys0.47.0 unchanged, Linuxarm64/amd64 CGO0; existing pinned native carrier, no dependency change.

**Spec:** docs/superpowers/specs/2026-10-08-workspace-archive-read-identity-design.md

## Global Constraints

- Existing codex/etcd-state-management checkout, no duplicate agent/worktree or unrelated Sentinel reads/edits/stage/revert.
- All code/tests/scripts/spec/plan/report and formatting edits apply_patch only; no cat/heredoc/gofmt-w source writes.
- PID1 real/effective/saved IDs0 and caps0xe0 preserved; filesystem identity scopes serialize original kernel validation, supplementary groups clear at open and restore exactly. No public credential callback, no provider/process spawn in production source. Restoration failure actual exit70.
- Only new focused native identity tests; old accepted native scenarios not replayed. Native carrier pullnever/networknone/readonly/NNP/same caps; exact owned cleanup and actual terminal evidence.
- Raw command/env allowlist/exits/timestamps/output/hashes only in fresh plan OWN; never capture whole process environment or credentials. Preserve failed attempts/RED snapshots; no force-add OWN.
- Coherent small source commit under OWN fcntl LOCK_EX spanning empty-index/exact stage/cachedcheck/commit/empty-index; Root separately commits docs acceptance. No production/auth/effects/release/capacity completion claims.

## Review Focus

- Silent failed setfsuid/setfsgid cannot fall through to root open; test real entry refusal and readback.
- Restoration failure must exit70 before FD or bytes escape; test real seccomp refusal, not only fake callbacks.
- Supplementary root groups must not bypass intended-user permissions; test group-only file denial and restoration.
- No writer/runtime validation inside transient scope or credential inheritance through new Go threads; test writer-visible restored all-thread kernel contract.
- Invalid UID, copied/foreign handles, cancellation and IO errors preserve identity/source consumption/zero-summary and descriptor cleanup; test native, no stale boolean authority.

### Task 1: User access scopes for original archive source

**Files:**
- Modify internal/runtime/launcher/workspace_archive_types.go (private read identity and pure validation only)
- Modify internal/runtime/launcher/workspace_archive_linux.go (shared original constructor plus scoped root/child/probe/validation opens; preserve root-mode functions/behavior)
- Modify internal/runtime/launcher/workspace_archive_unsupported.go (new unsupported API only)
- Create internal/runtime/launcher/workspace_archive_identity_linux.go (raw scope/setup/readback/restore/fatal and readonly open adapters)
- Create internal/runtime/launcher/workspace_archive_identity_test.go (pure host policy/unsupported validation)
- Create internal/runtime/launcher/workspace_archive_identity_native_test.go (actual native access/refusal/restoration/fatal tests)
- Create internal/runtime/launcher/testdata/archiveuser/main.go (fixed test helper prepare/clean, actual user credentials)
- Create scripts/test-workspace-archive-identity.sh (new focused labeled fixture with normal/entry-refusal/fatal-restore cases)

**Interfaces:** consumes original KernelBoundary.mu/self/PID/seal and original census API, accepted archive root/child/FD/bounds/summary. Produces OpenWorkspaceArchiveAsUser(ctx,observation,uid,gid), no signing or authority token. Private raw read scope opens only the fixed readonly paths and never exposes arbitrary effect callbacks.

- [x] Step 1 RED: add native private-file positive assertion, user helper and focused carrier with compilable all-reject AsUser stub; capture real1000-owned700/600 fixture metadata and actual expected failure plus source snapshot.
- [x] Step 2 implementation: complete Spec with narrowly scoped raw Linux credentials, safe runtime/ABI guard, supplementary group clearing, all readbacks/restoration, no writer under scope, fixed readonly open protections and fatal70 on unverified restoration. Add all specified negative and syscall-refusal cases.
- [x] Step 3 GREEN: new focused normal, entry-refusal and fatal-restore fixtures with native real statuses/cleanup; affected host launcher race once, vet, Linuxarm64 CGO0 compile and helper build. Capture all actual errors/diagnostics, distinguish deliberate70 from GoPASS and exclude old native scenarios.
- [x] Step 4 self-review, exact eight-path source commit, complete OWN author report/manifest; sole independent task/unit review, fixes if blocking, Root individual limit rulings and separate tracked acceptance. Explicitly preserve task/gate/producer/stream/effects and capacity gaps.

Accepted source `60a16e90fd23fdfe64a9a8edaf0284b7117893c5`; independent review Approved, C0/I0/M0. See `docs/superpowers/reports/2026-10-08-workspace-archive-read-identity-verification.md` for actual evidence and retained integration/platform/capacity limits. Completion is this physical access unit only.
