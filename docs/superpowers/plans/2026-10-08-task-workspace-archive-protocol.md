# Task workspace archive protocol Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox syntax for tracking.

**Goal:** Define independently authenticated fixed archive task attribution, a bounded session deadline, terminal summary and standalone wire framing for the approved final-sync path.

**Architecture:** Reuse strict protocol helpers and historical task lineage; separate short start window from fixed session deadline, and isolate archive framing from existing execution transport. No live producer, routing or remote effect is reconstructed from decoded bytes.

**Tech Stack:** Go1.25.6, existing dependencies unchanged; affected controlprotocol/controltransport host race and vet, no native fixtures.

**Spec:** docs/superpowers/specs/2026-10-08-task-workspace-archive-protocol-design.md

## Global Constraints

- Existing codex/etcd-state-management checkout. No duplicate implementation/reviewer, no unrelated Sentinel reads/edits/stage/revert, no prior closed OWN reads or accepted native scenario replay.
- All source/tests/scripts/spec/plan/report and formatting edits apply_patch only; no cat/heredoc/gofmt-w source writes.
- Fixed task_workspace_archive purpose and independent expected context; start<=30s, NotAfter<SessionNotAfter<=NotBefore+1h, entire issuer/runtime certificate window bound. Historical completion verifies current runtime, never grants a new send.
- Context equality/order, domains, JSON/wire/frame limits exactly as Spec; no caller UID/GID/path, no backend signing, new trust role, journal/producer, router/Session or Supervisor change in this unit.
- Only fresh plan OWN evidence; allowlisted Go/platform env, raw argv/UTC/exits/streams/source hashes. Never capture whole process environment or credentials; retain failed RED attempts and compilable exact stub snapshot.
- Source small commit under OWN fcntl LOCK_EX spanning expected base/branch, empty index, exact eight paths, cachedcheck, commit and empty index. Root separately commits tracked acceptance; no push/deploy/Redis removal or production/capacity completion claim.

## Review Focus

- Confusing signature attribution with actual producer/join/TAR bytes or safe release: comments/tests must retain boundaries.
- Session deadline may outlive start ticket but must not outlive certificate or be renewed by caller: exact independent expected times and certificate coverage tests.
- Context lineage can be syntactically valid with changed task/runtime/claim/epoch: field mutations and revision/equality edge tests must reject.
- Receipt substitution and summary overflow, invalid alignment or zero count: domain/purpose/key/cert and uint64 boundary tests.
- Chunk codec does not own stream ordering/EOF/cumulative limits: request real EOF and bounded one-frame binary tests; existing routing/event constants unchanged.

### Task 1: Signed archive attribution and standalone bounded frames

**Files:**
- Create internal/runtime/controlprotocol/task_archive_types.go (public context/claims/evidence, private pure validation and schemas)
- Create internal/runtime/controlprotocol/task_archive_ticket.go (typed current issuer signing/verification)
- Create internal/runtime/controlprotocol/task_archive_receipt.go (typed terminal signing/historical exact runtime verification)
- Create internal/runtime/controlprotocol/task_archive_ticket_test.go (independent signatures, lineage/window/cert/malformed/copy tests)
- Create internal/runtime/controlprotocol/task_archive_receipt_test.go (summary/time/context/domain/runtime/historical tests)
- Create internal/runtime/controlprotocol/task_archive_strict_test.go (strict nested JSON cases)
- Create internal/runtime/controltransport/task_archive_frame.go (envelope, separate one-request/one-response-frame codecs)
- Create internal/runtime/controltransport/task_archive_frame_test.go (canonical EOF/binary/boundaries/truncation/IO tests)

**Interfaces:** `SignTaskWorkspaceArchiveTicket(key,issuer,claims) ([]byte,error)`; `(*ManagementVerifier).VerifyTaskWorkspaceArchiveTicket(wire,issuerWire,expectedContext,now) (TaskWorkspaceArchiveEvidence,error)`; `SignTaskWorkspaceArchiveReceipt(key,claims) ([]byte,error)`; `(*ManagementVerifier).VerifyTaskWorkspaceArchiveReceipt(wire,runtimeCertificate,expectedContext,ticketDigest,notBefore,notAfter,sessionNotAfter,now) (TaskWorkspaceArchiveReceiptEvidence,error)`. Types/field names from Spec. Evidence exposes cloned Wire, Context, three time getters, ticket Digest; receipt additionally State/TicketDigest/ArchiveBytes/EntryCount/SHA256 getters. Codec: `WriteTaskWorkspaceArchiveRequest(w,e) error`, `ReadTaskWorkspaceArchiveRequest(r) (TaskWorkspaceArchiveEnvelope,error)`, `WriteTaskWorkspaceArchiveFrame(w,kind,data) error`, `ReadTaskWorkspaceArchiveFrame(r) (byte,[]byte,error)`. Public ArchiveData/ArchiveComplete constants as Spec. Reuse existing pure validation/schema/private key helpers and test fixtures; no existing-source edits needed.

- [ ] Step 1 RED: add real signed-ticket positive test with independently signed exact claims and compilable all-reject constructor stubs; capture actual assertion failure, exact snapshot and hashes. Missing symbol/compiler failure is not meaningful RED.
- [ ] Step 2 implement Spec and focused tests: ticket/receipt validation and authentication, fixed session deadline, immutable evidence, nested strict schemas, standalone canonical EOF and bounded binary frames. Run focused new tests while implementing.
- [ ] Step 3 GREEN: pinned Go test -race -count=1 -v ./internal/runtime/controlprotocol ./internal/runtime/controltransport once after source freeze; vet same packages; gofmt-equivalence diff through patch only, git diff --check. Capture all actual warnings/failures and source hashes. No native replay or performance/platform claims.
- [ ] Step 4 self-review, exact eight-path coherent source commit and concise OWN author report/manifest; sole independent task/unit review, fix blocking issues, Root individual retained-limit rulings and separate tracked acceptance. Overall authenticated archive transport remains unimplemented until the following integration units pass.
