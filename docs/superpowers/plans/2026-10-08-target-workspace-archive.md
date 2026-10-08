# Target workspace archive producer Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox syntax for tracking.

**Goal:** Persist original archive admission across failure/reopen and produce terminal attribution only from actual confined source output.

**Architecture:** Original live journal pins completed quiescence and fresh ticket/certificates, stores permanent pending, then later owns one synchronous source stream and durable terminal. Cold structural history cannot reconstruct either owner. Short admission and immutable long session remain distinct.

**Tech Stack:** Go1.25.6/x/sys0.47.0 unchanged; existing protocol/launcher/journal primitives; same pinned restricted native carrier.

**Spec:** docs/superpowers/specs/2026-10-08-target-workspace-archive-design.md

## Global Constraints

- Existing codex/etcd-state-management; no duplicate/nested implementation or second task reviewer; no prior closed OWN reads/replay and no Sentinel reads/edits/stage/revert.
- Source/tests/scripts/spec/plan/report/format edits apply_patch only. No cat/heredoc/gofmt-w source writes or dependency changes.
- Only original fresh journal/quiescence/archive handles; actual pending before source bytes; unchanged closed gates; full session certificate coverage; fixed monotonic deadline before pending IO, never extended by duplicate/stream caller.
- Records<=16384, receipt<=12288; exact protected filename/temp contract, existing accounting byte/file/85percent limits; no cold authority or automatic deletion of uncertainty.
- Separate task directories under this plan OWN, selected env only, raw argv/UTC/exits/streams/sourcehashes/snapshots; no whole env/credentials. Native only new selected tests, same restricted image/caps and exact owned cleanup; physical syscall evidence not power-loss proof.
- Each exact-path source commit under fcntlLOCK_EX with expected branch/base/emptyindex/cachedcheck/commit/emptyindex; Root separate acceptance. No push/deploy/provider write/release/production or capacity completion claim.

## Review Focus

- Readable cold/pending/terminal files cannot mint an original producer: cold reopen and copied/foreign handle tests.
- Admission context5s must not become session cutoff or duplicate renewal: immutable monotonic deadline and late pending IO/auth tests.
- Capacity/recovery must count archive/temp bytes and require retained successful quiescence: boundary, missing retained, conflicting temps and corrupt record tests.
- No summary DTO completion or mutex around destination callbacks: actual source positive native and writer/cancellation/panic/blocked-owner tests.
- Close must retain borrowed resources until physical return and signing must fail after cancellation/poison/late persistence: interleaving and per-syscall fault tests, no false termination proof.

### Task 1: Durable original archive admission and cold refusal

**Files:**
- Modify internal/runtime/controlprotocol/task_archive_types.go (three pure public Validate wrappers only)
- Modify internal/runtime/controltarget/journal.go (archive accounting/original owner fields and content count only; Close unchanged)
- Modify internal/runtime/controltarget/journal_accounting.go (archive/temp accounting, retained quiescence lineage and cold refusal)
- Create internal/runtime/controltarget/task_archive_records.go (strict pending/terminal structural record/schema/Validate/temp names)
- Create internal/runtime/controltarget/task_archive_io.go (linux/darwin protected point read/pending/terminal persistence)
- Create internal/runtime/controltarget/task_archive_io_unsupported.go (unsupported point methods fail closed)
- Create internal/runtime/controltarget/task_archive.go (AcceptedWorkspaceArchive original object and Accept/Lookup only)
- Create internal/runtime/controltarget/task_archive_test.go (labeled host structural setup, original/auth/deadline/cold/capacity/recovery tests)
- Create internal/runtime/controltarget/task_archive_fault_test.go (per-persistence/late-auth retained failure tests)
- Create internal/runtime/controltarget/task_archive_native_test.go (actual PID1 completed-quiescence admission positive and focused faults; env-gated)
- Create scripts/test-runtime-workspace-archive.sh (new labeled restricted admission fixture)

**Interfaces:** Value receiver `(c TaskWorkspaceArchiveContext) Validate() error`, same value method for TicketClaims/ReceiptClaims wrapping existing private validators. `Journal.AcceptWorkspaceArchive(ctx,e p.TaskWorkspaceArchiveEvidence,q *AcceptedUserQuiescence,receipt []byte) (*AcceptedWorkspaceArchive,error)`; `Journal.LookupWorkspaceArchive(ctx,expected p.TaskWorkspaceArchiveContext,ticketDigest string) (*TaskWorkspaceArchiveRecord,error)`. Record fields exactly Spec; self/journal/record/deadline/prior pointers private, no public completion or stream method yet. Task2 consumes these original fields and persistence helpers.

- [ ] Step 1 RED: compiled reject admission stub plus genuinely authenticated ticket and original structural host prerequisite; positive assertion fails for unavailable admission, not compiler/setup error. Retain exact snapshot/raw failure.
- [ ] Step 2 implement admission/records/accounting/authentication/cold refusal; independent corrupt/changed context/receipt/cert tests and capacity/retained/temp cases; meaningful per-syscall persistence and late auth failures retain uncertainty.
- [ ] Step 3 GREEN: once frozen affected host race `go test -race -count=1 -v ./internal/runtime/controltarget ./internal/runtime/controlprotocol`; vet same; Linuxarm64CGO0 compile controltarget and native new admission-only carrier with actual completed quiescence. No old native replay; classify host fixture versus physical proof.
- [ ] Step 4 self-review/exact11-path source commit; concise task-1 report/manifest in this OWN; sole independent task review, fixes/rulings and Root tracked acceptance; proceed Task2 without approval pause.

### Task 2: Actual original source producer and terminal persistence

**Files:**
- Modify internal/runtime/controltarget/task_archive.go (one-shot/cancel/done lifecycle fields, immutable deadline getter)
- Modify internal/runtime/controltarget/journal.go (Close cancels and waits physical archive owner without j.mu, preserves non-archive behavior)
- Create internal/runtime/controltarget/task_archive_stream.go (actual source stream, terminal persist/sign and original historical resign)
- Create internal/runtime/controltarget/task_archive_stream_test.go (host invalid-original/summary-injection refusal/close/persistence boundaries)
- Modify internal/runtime/controltarget/task_archive_native_test.go (only new actual source/receipt/fault/ownership native test, no admission replay)
- Modify scripts/test-runtime-workspace-archive.sh (explicit producer selection and test-only actual-user helper mount if required; admission mode preserved)

**Interfaces:** `Journal.StreamWorkspaceArchive(ctx,a *AcceptedWorkspaceArchive,source *launcher.WorkspaceArchiveSource,dst io.Writer,key ed25519.PrivateKey) ([]byte,error)` derives summary internally. `Journal.SignWorkspaceArchiveReceipt(ctx,a *AcceptedWorkspaceArchive,o *launcher.UserNamespaceQuiescenceObservation,key ed25519.PrivateKey) ([]byte,error)` original successful historical resign only. `(a *AcceptedWorkspaceArchive) Deadline() (time.Time,error)` returns sealed original deadline, no renewal/authority. No arbitrary-summary completion setter. Actual source is fixed root and immutable birth-user-selected by future trusted Supervisor.

- [ ] Step 1 RED: compiled reject stream stub; new actual PID1 source fixture and accepted pending, positive actual TAR+signed receipt assertion fails for unavailable stream. Exact raw/snapshot retained.
- [ ] Step 2 implement synchronous source/terminal/sign/cancel/Close ownership contract; callback lookup occurs with j.mu released, one-shot before first bytes; use real source summary, preserve pending/poison on all failures.
- [ ] Step 3 GREEN: only new producer native test, private-user actual content/receipt hashes and restored credentials; writer/context/panic failures, copied/foreign owners, blocked-writer Close retains owner until return, terminal IO/late-cert/session failures, historical resign/cold refusal. Once affected controltarget host race/vet and Linuxarm64CGO0 compile after source freeze; no Task1 native replay.
- [ ] Step 4 coherent exact6-path source commit, separate task-2 report/manifest, sole independent task/unit review and Root tracked acceptance. This plan completes target producer only; next Supervisor/backend/receiver integration remains explicit, whole migration review later.
