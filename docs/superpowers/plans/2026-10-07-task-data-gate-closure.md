# Task data-gate closure implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Deliver one original-task-claim-authorized CloseData and retain an authenticated exact-runtime durable closure receipt.

**Architecture:** Typed Ed25519 task command and local closed receipt, original-claim native Stage intent, one-way journal closure, exact activated mTLS delivery. This closes admission only; later drain and safe-release protocols consume separate evidence.

**Tech Stack:** Existing Go1.25, etcd3.6, strict codecs, Ed25519 and protected journal IO; no dependencies.

**Spec:** `docs/superpowers/specs/2026-10-07-task-data-gate-closure-design.md`.

## Global Constraints

- Branch codex/etcd-state-management. OWN `.superpowers/sdd/2026-10-07-task-data-gate-closure/` only. Never read/touch/stage unrelated Sentinel document or closed predecessor OWN. Small verified commits promptly; no push/merge/deploy.
- No idle-N actors; no owner release/control/index deletion/physical End, CloseAll, reopen, remote settlement or production selection changes.
- Native64ops/64KiBrecord/256KiBmutation unchanged. Ticket4096, receipt8192, close intent16384, local record8192. Existing journal256MiB/70%/85%/65536 unchanged.
- Original task claim/guard compares value/Lease/Create/Mod, local send-time deadline irreversible; no replacement Lease/adoption. Typed close draft deadline fixed despite renewal; claimCreate>destroyingControlBirth. Metadata Stage remains original server-guard protocol, not cancellation as external withdrawal.
- Strict recursive exact wire schema and owned immutable evidence. Target installed restore, exact runtime UID/Boot, generation/data gate and activated issuer remain pinned. Cleanup independent of business expiry; live issuer/runtime certificate required.
- Real etcd/Linux gates coordinated by Root on exact frozen source with isolated owned fixtures; workers never launch Docker, read real env/secrets or mutate external services. Explicitly separate host/static/skipped/compositional evidence.

## Review Focus

1. Expired business TTL must not block legitimate close, while ticket/claim/certificate expiration must block new effects (Tasks1/3/4).
2. Pending write, ambiguous fsync or cancellation cannot yield data_closed proof; late command accepted before close remains for later drain (Tasks2/4).
3. Close cannot disable authenticated historical queries or reopen start/renew; process shutdown ownership still joins borrowers (Task4).
4. New claim/reference cannot overwrite unknown intent, adopt old prepared ticket or replace first target attribution (Tasks2/3/4).
5. Same-byte key recreation, alias mutation or wrong signature purpose cannot cross identity/fencing; budgets include actual Stage reserve (Tasks1/3).

### Task 1: Typed CloseData ticket and local closed receipt

**Files:** Create `internal/runtime/controlprotocol/task_close_types.go`, `task_close.go`, `task_close_receipt.go`, `task_close_test.go`, `task_close_receipt_test.go`. Reuse unchanged codec, issuer/runtime certificate helpers; do not change existing wire formats.

**Interfaces:** Produce all TaskCloseDataContext/TicketClaims/Evidence and TaskDataClosedReceiptClaims/Evidence, sign/verify APIs and getters specified in the spec. Context fields and fixed domains/limits exactly as spec. No live runtime/etcd capability.

- [x] **Step1:** Write `TestTaskCloseDataTicket`, `TestTaskCloseDataTicketRejects`, `TestTaskCloseDataTicketStrict`, `TestTaskCloseDataTicketCopies`, `TestTaskDataClosedReceipt`, `TestTaskDataClosedReceiptRejects`, `TestTaskDataClosedReceiptStrict`, `TestTaskDataClosedReceiptCopies`. Independently raw-sign valid and invalid claims/domain; all context fields one-at-a-time, wrong roles/roots/delegates/key copies, ID/hash/revision and time boundaries, strict recursive malformed Unicode/schema, exact byte limits, normalization/copy independence. Receipt expired ticket/fresh runtime certificate positive, expired cert/wrong boot/wrong digest/window negative; no false drain fields. Record actual tests-first result; compilation failure is not behavioral RED.
- [x] **Step2:** Implement exact pure protocol signatures/getters and shared schema helpers, no exported generic authentication callback. Sign/verify zero outputs on errors and do not mutate inputs.
- [x] **Step3:** Run `go test -race ./internal/runtime/controlprotocol -run '^TestTask(CloseDataTicket|DataClosedReceipt)' -count=1 -v`, `go vet ./internal/runtime/controlprotocol`, `git diff --check`; preserve full command/stdout/stderr/exit and hashes of relevant source/compiler. Run affected package suite once when changed shared helper warrants it; do not replay kernel/native/etcd suites.
- [x] **Step4:** Self-review, promptly commit coherent verified slices, write full report, original-task-range independent review. Root accepts both spec/quality verdicts and individual evidence limits before next task.

### Task 2: Durable one-way runtime journal CloseData

**Files:** Create `internal/runtime/controltarget/journal_task_close.go`, `journal_task_close_codec.go`, `journal_task_close_test.go`, `journal_task_close_fault_test.go`; narrowly modify `journal.go`, `journal_accounting.go`, Unix/unsupported IO helpers for fixed close file and accounting.

**Interfaces:** Consume Task1 evidence; produce `TaskDataCloseRecord{Version uint32; State string; Context controlprotocol.TaskCloseDataContext; TicketDigest string; NotBefore,NotAfter time.Time}` and Journal.CloseData/LookupDataClose exactly as spec. Own fixed file data-close.json; no signed receipt production in journal.

- [x] **Step1:** Pin actual syscall pending->gate->closed ordering, ambiguous write/file-sync/rename/dir-sync at each phase, clock/cancel before/after IO, origin/cold/birth/issuer/restore mismatch, identical replay/different claim conflict, immutable caller copies, hard capacity/85% behavior, cold pending/closed and preserved temp accounting. No closed proof from initial/local CloseGate alone.
- [x] **Step2:** Implement strict fixed record codec, original ticket reauthentication, one-way persistence and exact point lookup; cold scan remains bounded128names/256buckets and diagnostics only. Existing formats/exec records unchanged.
- [x] **Step3:** Focused race/vet/diff; propose named changed-seam Linux journal selector/freeze to Root for real protected IO, preserve full evidence. No Docker launch by worker.
- [x] **Step4:** Small verified commits/report/independent review. Task3 may execute in parallel in disjoint etcd files after Task1 acceptance; no Task4 until both accepted.

### Task 3: Original-claim-fenced permanent CloseData intent

**Files:** Create `internal/storage/state/etcd/task_close_authority.go`, `task_close_effect.go`, `task_close_effect_types.go`, `task_close_effect_records.go`, `task_close_effect_read.go`, `task_close_effect_validation.go` and focused corresponding tests; narrowly extend client.go/task_claim.go/task_keys.go and shared private issuer registration.

**Interfaces:** Consume Task1 claims/evidence and original TaskClaim. Produce optional TaskCommandIssuer/Options.TaskIssuer; PrepareTaskCloseDataResult{Outcome,Reference,Prepared,GuardCleanupError}, TaskCloseDataReference{Task TaskReference; CommandID string; Stage StageReference}, TaskCloseDataRecord/Entry, opaque PreparedTaskCloseData and PrepareTaskCloseData/LoadTaskCloseData as spec. Load takes TaskReference to recover the original close command after worker restart; it does not require caller-retained command ID or reconstruct a private capability. Preserve Task1/2 metadata interfaces and ExecIssuer contracts.

- [x] **Step1:** Tests before implementation for constructor zero calls/typed nil/trust, exact immutable issuer registration, task context from original fences, expired business expiry allowed, fixed draft/UUID/deadline, intent maxbytes/recursive schema, actual59ops and overbudget pre-Grant failure, all original fence changes, unknown registration/Begin/commit and original abort competition, post-sign/postcommit expiry/cancel, copied/foreign handles, newclaim/unknownintent refusal.
- [x] **Step2:** Implement original sealed draft and native immutable intent+receipt. Share only the registration mechanism via private helper, maintain Exec registration behavior. Reauthorize at each specified boundary, explicit nonempty linearizable fence check, no overwritten history/auto replay or external action.
- [x] **Step3:** Focused host race/vet/build/diff and named actual owned-three-member selectors proposed to Root; no prior accepted-suite replay. Source/RPC/outcome/warnings preserved and no false native claim.
- [x] **Step4:** Prompt small commits, complete report/original-range independent review. Coordinate ownership with Task2; no shared-file edits by two authors.

### Task 4: Authenticated close delivery and query after closure

**Files:** Create `internal/runtime/controltransport/task_close_frame.go`, `internal/runtime/controlrunner/supervisor_task_close.go`, `internal/storage/state/etcd/task_close_delivery.go` and focused tests; narrowly modify frame dispatch, supervisor_transport.go and destination/client seams only as needed while preserving old exec bytes.

**Interfaces:** Consume accepted Task1/2/3. Produce one-shot `(*Backend).DeliverTaskCloseData(ctx context.Context,p *PreparedTaskCloseData,destination *controltransport.Destination)(TaskCloseDataDeliveryResult,error)` and diagnostic `(*Backend).QueryTaskCloseData(ctx context.Context,ref TaskCloseDataReference,destination *controltransport.Destination)(TaskCloseDataDeliveryResult,error)`. Result `{Receipt *controlprotocol.TaskDataClosedReceiptEvidence}`; nil receipt + error means unknown. Reuse existing Destination's exact runtime verification and original owned bounded connection contract; diagnostic query never close dispatch.

- [ ] **Step1:** Actual activated mTLS and EOF/frame rejection, wrong-peer/boot/gate/claim/digest, stale prepared loss before/after handshake, postcommit known history/no live dispatch, late start rejected while pre-close accepted remains, renew rejected and exact exec receipt query works after close, ambiguous disk/sign/send unknown, concurrent shutdown/query borrower join. Native selector must include real protected Linux target closure and retained authenticated query, no synthetic physical End.
- [ ] **Step2:** Implement separate task DTO/frame parsing with strict bounds, Supervisor admission mutex closure/signing, and split identity-query from start/renew admission. Keep16conn/5s and original IO ownership; no automatic send retry, idle actor, monitor discard or side effects from history.
- [ ] **Step3:** Covering host race/vet/build/diff and Root-owned frozen real Linux/etcd delivery gates, classify failures/warnings and inherited source scope honestly.
- [ ] **Step4:** Small verified commits, original-range independent review, report all declined behaviors individually. Then one complete new-unit review and one collective fix wave/scoped re-review if required, preserve rulings/evidence. Continue mandatory CloseAll/dual drain/remote settlement/exact termination/safe release and production migration, no overall completion.
