# Original registered workspace sync source Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use subagent-driven-development with a fresh sole author and sole independent review.

**Goal:** Supply bounded original verified archive entry/body IO under current native ownership, with physical consumer shutdown and retained quota.

**Architecture:** Register one source owner on the retained private spool, reuse original claim deadline publication and a shared current-native transaction helper, and expose cursor/body only to private future adapters. Parent Close routes cancel/join actual source work before cleanup.

**Tech Stack:** Go1.25.6, archive/tar, original os.File SectionReader, existing native etcd/protobuf.

**Spec:** `docs/superpowers/specs/2026-10-08-workspace-sync-source-design.md`.

## Global Constraints

codex/etcd-state-management, merged base7bca73a. All source/test/report/format edits apply_patch, no cat/heredoc/python writes or gofmt-w. Only eleven paths below. No CLOSED OWN reads/repeated tasks/nested agents/Sentinel/dependencies/credentials/globalHTTP/trust/cloud/deploy/push/merge/Redis changes. New selector only, no accepted selector replay. Existing64nodes/64KiBrecord/256KiBmutation and Jobs/disk/index limits unchanged;32768byte scratch buffers/1MiB TAR metadata/path4096. No idle-N actors/maps/watches. Exact source commit under OWN fcntl LOCK_EX with pre/post empty index and exact staged paths. No arbitrary callback/summary/reader promotion to provider authority.

## Review Focus

- Close/cancel while an original read is physically held: no unlink/refund/client shutdown or new IO until registered work and monitor join.
- Legitimately renewed claim versus expired old archive session: source uses latest current claim and fixed history, neither abort valid renewed source nor revive archive-send authority.
- Wrong plan/copied handle/foreign Backend or altered file: refuse before bytes/descriptors, retain conservative uncertain cleanup, no replacement source.
- Real PAX offset/empty/large files/exclusion lookalikes and incomplete body: exact bounded bytes/hash/EOF, no hidden wholefile buffers or deletion-completion promotion.
- Restore/identical command ABA/new operations/failed native query/renewal: invalidate active source, shared helper keeps complete native freshness and exact caps.

## Task 1: Original consumer, private cursor/body and shutdown integration

**Files:** Create same directory `internal/storage/state/etcd/task_workspace_sync_source_types.go`, `task_workspace_sync_source.go`, `task_workspace_sync_source_guard.go`, `task_workspace_sync_source_cursor.go`, `task_workspace_sync_source_body.go`, `task_workspace_sync_source_test.go`, `task_workspace_sync_source_native_test.go`. Modify `task_archive_receive_types.go` for registered consumer binding; `task_archive_spool.go` for all-consumer cancellation/physical join; `task_archive_delivery_guard.go` solely extract shared native helper; `task_archive_delivery_native_test.go` solely factor existing fixture through optional canonical final-sync-plan builder, retaining existing default behavior. No claim/client/limits/transport changes without concrete Root ruling.

**Consumes:** ReceivedTaskWorkspaceArchive and exact private witness; TaskWorkspaceSyncPlan.MatchesAttempt; current original archive command/draft/task/claim pins; retained archiveDelivery atomic deadline/wake; accepted spool identity/index/close; existing actual framed native delivery fixture and plan constructor. Read accepted definitions, not closed OWN.

**Produces:** Public PrepareTaskWorkspaceSyncSource and original source.Close; private next/openBody, sealed entry and body reader.Close, registered consumer lifecycle; one shared private native freshness helper used by both receiving and consuming. No native provider dispatch/intent/settlement or new public IO capability.

- [ ] Capture meaningful compiled RED for original source rejection/admission/physical retention with callable scaffold, preserve raw output; no undefined-symbol failure.
- [ ] Implement original-only canonical admission and source registration before IO, immutable body/history attribution, independent consumer ctx and exact bounded identity/size/SHA checks. No attempt consumed on pure mismatch.
- [ ] Implement shared native helper extraction, source local/fresh fences, serial native monitor/independent latest-deadline expiry, and all-close physical joins using existing Renew/Release spool-close path. No claim mutex across IO/joins or historical-window extension.
- [ ] Implement bounded private sequential cursor/PAX offsets/current descriptor/one-shot body; exact logical hash/bytes/EOF/exclusions and incomplete-body refusal. No size-driven allocation, index expansion or public Reader.
- [ ] Add new host/native behavior tests for five focuses, real TLS/native canonical-plan fixture plus controlled original-file read completion barriers. Record test seam and clock limits truthfully; reuse existing fixture code without old selectors. Cover both default fixture/refusal and canonical fixture after factoring.
- [ ] Run final `go test -race -count=1 ./internal/storage/state/etcd -run '^TestTaskWorkspaceSyncSource' -v`, `bash scripts/test-etcd-state.sh -run '^TestTaskWorkspaceSyncSource' -v`, package vet and Linuxarm64CGO0 compile. Inspect gofmt diff/apply_patch and exact diff check; self-review, raw evidence/current+Git source hashes, small exact source commit promptly. Sole task review then Root tracked acceptance; advance to per-wire native provider intent/settlement.
