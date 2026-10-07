# Implementation plan — task-authenticated user quiescence

The predecessor CloseData unit is accepted at3f1e269. Section9/10 select decisions and correct independent preflight findings; dispatch requires exact filled brief. All proposed files/selectors below are scoped future work, not existing evidence. This plan depends on `docs/superpowers/specs/2026-10-07-task-user-quiescence-design.md` and the master destroy/operation/workspace/remote-settlement constraints.

## Global constraints and preflight decisions

One durable USERS barrier and one immutable attempt per task; finite lifetime-owned coordinator; management TLS/journal/key survive successful quiescence. Exact task/claim/runtime/generation/data-gate/restore attribution. Earlier claimant's CloseData history is a prerequisite, never a reconstructed Prepared. New higher epoch does not supersede unknown work. No effect retry after bytes, fabricated local_terminal, token deletion/revocation, successful End, FUSE/remote settlement or owner release.

Eligible topology: actual PID1 plus registered monitors/users only, mounter outside PID namespace. Fresh4096-bounded original bootstrap-pinned kernel/proc/namespace/cgroup seal validation and non-reaping __WALL|WNOWAIT ECHILD after original waits join. No UID exemptions or new capabilities. Unknown child/process/namespace/cold state fails closed.

Preserve16conn/5s request budget and all predecessor canonical wire formats/limits. Proposed NEW quiesce ticket8192,accepted/terminal receipts12288,target record16384,etcd record32768; root must approve independently. Native64operations/64KiB record/256KiB mutation and target256MiB/70%/85%/65536 limits do not change. New workers only for active finite task, no per-idle leases/goroutines/timers/Watch. Every owned worker/callback/pipe has one join/close owner and fixed budget. No product fixture mode, raw credential/capability getter or generic sign callback.

Selected requirements before Task1 dispatch (sections9/10 control):

1. **D1 absolute lifetime:** proposed t0+31s includes all stop delivery, existing monitor cleanup, joins, namespace proof and terminal persistence/signing. Approve earliest-deadline integration with PID1 isolation, or reject async acceptance if no bounded lifetime can be established. No new31s per shutdown caller. Existing30s monitor cleanup remains; late cancellation may yield legitimate aggregate unknown. Unpreemptible OS calls never become success.
2. **D2 receipt CAS:** recommended complete63 shape + exact original CloseData receipt ModRevision =64 charged operations; require native proof of mutation sensitivity. 63 without this needs an explicit no-GC/retention and historical-proof ruling; it is not default. Four extra receipt comparisons =>67 and must fail pre-Grant. A link transaction alone is not a remedy. If64 cannot be approved, redesign/decompose before author work.
3. **Protocol and durable barrier:** approve names/types/limits, canonical ordered execution-set digest, accepted/terminal state separation, pending record as durable USERS barrier (or explicit second file), original interval versus post-expiry cleanup. Decide whether newer claim revision must be >= historical CloseData claim revision.
4. **Recovery/local prerequisite:** terminal historical target proof plus a fresh exact claim may create a NEW local dual-drain prerequisite, never original delivery authority. Unknown attempt remains query-only. Define prerequisite's later revalidation requirements, not a bearer token.
5. **Integration topology and evidence:** identify actual approved3member-to-production-target transport topology. Combined test must run real Backend against real metadata and actual protected PID1, no substitute fake client/target. Root owns custody/fixture limits/source freeze and fault decisions.

## Task 1 — typed authority, durable USERS barrier and atomic signer

**Depends on:** current whole-unit acceptance and all protocol decisions above. **Scope:** new controlprotocol and controltarget quiescence files plus narrow existing root scanner/accounting integration. No supervisor stop implementation or etcd effect yet.

Proposed files:

- `internal/runtime/controlprotocol/task_quiesce_types.go`, `task_quiesce_ticket.go`, `task_quiesce_receipt.go` and corresponding focused tests.
- `internal/runtime/controltarget/task_quiesce.go`, `task_quiesce_records.go`, `task_quiesce_io.go`, `task_quiesce_test.go`, `task_quiesce_native_test.go`; narrow existing journal.go private fields/contentFilesLocked, journal_types.go and journal_accounting.go; exact lines resolved in filled brief, not blanket package ownership.

Implement exact `TaskUserQuiescenceContext`/ticket/accepted/terminal claims and Sign/Verify APIs from draft sections2–3. Distinct domains/purposes; copied evidence; zero partial evidence; current versus historical context checks; issuer key/binding/window checks. Add fixed strict pending/terminal journal record and irreversible USERS closure accounting. Task1 implements opaque accepted pending handle/barrier and fixed accepted signer only. Task2 owns terminal producer integration under section9 trusted supervisor/private-owner and fresh opaque kernel observation contract. No generic complete setter or fabricated proof in Task1.

Meaningful tests-first: independent raw Ed25519 domain fixtures; valid independent cert mismatches; exact bound schemas/aliases; accepted is not terminal; earlier claimant CloseData tuple permitted, wrong task/birth forbidden; after-ticket-expiry query with fresh runtime cert. Journal actual file/fsync/rename/dir-sync fault boundaries for pending/accepted and capacity peak; terminal IO/poison/cold signer tests belong Task2; wrong/duplicate attempt; poison/cold cannot sign; no old exec/gate bytes mutated. Pure compile failures classified honestly.

Selectors: `^TestTaskQuiesce(Ticket|Receipt|Journal|Capacity|History)`; Root protected Linux journal selector `^TestTaskQuiesceJournalNative$`. Targeted affected-package race/vet/build/diff only. Native journal proof is not whole PID1 drain proof. Commit coherent tested slices and return full raw/source report for independent task review.

## Task 2 — stop original registrations and prove local namespace empty

**Depends on:** Task1 accepted private proof/signer contract; D1 approved. **Scope:** supervisor lifecycle coordinator and new launcher PID1 observation; no metadata drain/capability adoption.

Proposed files:

- `internal/runtime/controlrunner/supervisor_quiesce.go`, `supervisor_quiesce_test.go`, `supervisor_quiesce_native_test.go`.
- New target `task_quiesce_terminal.go`/`task_quiesce_terminal_test.go` and corresponding native/IO tests plus narrow Task1 private handle/record integration.
- Narrow changed portions of `supervisor_exec.go`, `supervisor_types.go`, `supervisor_failure.go`, `supervisor_transport_lifetime.go` and monitor control ownership only where necessary.
- narrow `internal/runtime/launcher/kernel_types.go`/`kernel_linux.go` original immutable pre-admission seal capture, `pid1_quiescence_seal_linux.go`, `internal/runtime/launcher/pid1_quiescence_types.go`, `pid1_quiescence_linux.go`, `pid1_quiescence_unsupported.go`, focused model/native tests. No blanket kernel redesign.

Implement one supervisor-owned accepted coordinator; its immutable execution set includes accepted-before-spawn and Start-in-flight and every registration not yet owner-joined at the barrier, bounded64. Retirement and ownerDone close linearize under s.mu only after watchdog/callback/pipe/sole-Wait completion; normal and failure paths covered; release s.mu before awaiting captured owners. Register/join task ownership before ACK availability, preserve journal/key until task+transport borrowers joined. Close user admission under existing mutex and arbitrate beginStartLocked. Never-spawned disposition requires original start owner joined and no actual Start committed; otherwise await exact Start outcome or fail unknown. Retain sole monitor cmd.Wait and user monitor Wait4 owners; use original control channel/stop semantics, no competing output reader. Follow all stages through terminal result, EOF, ownerDone, waitDone before considering an entry complete.

After all owned entries complete, actual original KernelBoundary performs fixed4096-limited proc census, namespace/cgroup/contract validation and non-reaping ECHILD. Return no proof for unknown child, overflow, wrong namespace or incomplete scan. Task2 implements fixed-purpose terminal persistence/signing under live journal mutex after its trusted private-owner proof and fresh opaque original-seal observation; timeout/loss/poison never terminal. Existing output-loss/monitor-loss isolation remains distinct, with exact inherited function equality reported where claimed.

Tests: `^TestTaskQuiesce(RegisteredBeforeSpawn|StartInFlight|SoleWait|Namespace|Shutdown)` covers deterministic private coordinator interleavings, no replacement Start, terminal-before-join refusal, stop/renew races, duplicated close/query/owner arbitration, callback lifetime and absolute deadline. Actual Linux selector `^TestTaskQuiesceNative(Local|DoubleFork|UnknownChild|MonitorLoss)$` covers real cancel, setsid/doublefork, actual ECHILD/census and unchanged quotas. No broad repeated kernel suites. Root decides physical injection for precise monitor loss; never substitute a model and call it production physical proof. Report unknown fallback versus genuine clean quiescence separately. Independent review before Task4 uses it.

## Task 3 — original-claim metadata effect and bounded operation pages

**Depends on:** Task1 protocol contract and D2 ruling. Can be authored separately from Task2 with exact disjoint source ownership after Root scheduling. **Scope:** etcd task quiesce intent/preparation/history and read-only operations observation.

Proposed files:

- `internal/storage/state/etcd/task_quiesce_authority.go`, `task_quiesce_effect.go`, `task_quiesce_records.go`, `task_quiesce_read.go`, `task_quiesce_validation.go`, focused tests.
- `operation_drain_read.go`, `operation_drain_read_test.go`; narrow `client.go`, `task_claim.go`, `task_keys.go` optional provider/private draft/fixed keys only.

Exact APIs: `PrepareTaskQuiescence`, `LoadTaskQuiescence`, `ObserveTaskOperationsPage`; immutable original send capability named `PreparedTaskUserQuiescence`, not later `PreparedTaskQuiescence`. Provider optional separate typed interface preserves existing TaskCommandIssuer callers; constructor calls neither signer nor clock. Pin first certificate before uncertain registration. Original full claim/fences/native Stage arbitration; fixed command/deadline never widened on Renew; history cannot generate new effect. Derive original CloseData proof with strict common-birth committed receipt; accept older claimant attribution only with identical permanent tuple. Recheck original claim/context pre/post every signer/RPC.

If D2 approves64 charge, compare exact prior receipt ModRevision in addition to all63 existing/proposed comparisons; literal wire independently asserted (49 business comparisons+1write+14reserve=64 charge;57 literal comparisons+2success writes+4Else reads=63 nodes). Existing clientv3.Cmp MOD oneof validation/copy supports the standalone exact positive revision fence, without a new comparison primitive. Keep global cluster/restore and all original63 fences. Add a65-charge case that fails before any actual Grant. Actual native mutations must include receipt delete, same-byte rewrite, valid alternate content, Lease attach, same-byte delete+recreate and global restore change after pre-read and before commit. No fake server revisions or weaker comparisons. Test pre-Grant operation/byte limits observe zero real Grant calls. If budget fails, stop for Root design ruling.

Operation pages: private self/origin/claim cursor,16records/80KiB owned total; exact fixed prefix/end and increasing keys. Current linearizable Range per page with original full fence comparisons and nonempty branches. Validate actual response shape/headers/cluster/key/envelope/order/limit and original pre-destroy record tuple. No complete-empty evidence from pages or token absence alone; no writes/revokes/End. A final fixed limit1 barrier helper is private for Task4 composition. Unknown or stale cursor cannot reconstruct a claim.

Host selectors `^TestTaskQuiesceMetadata` and `^TestTaskOperationDrainPages`; actual three-member `^TestTaskQuiesceNative(Metadata|Pages|DelayedClaim|ReceiptFence)$`. Include concurrent token disappearance, page-boundary mutations, restore/fence loss, malformed real replies, unknown native commit/resolve and delayed original Txn after original claim revoke/expiry/new claim. Root selects disjoint same-quota batches if needed; retain failed gates/warnings and no performance claim. Review complete task scope before downstream composition.

## Task 4 — authenticated finite transport and dual-drain prerequisite

**Depends on:** reviewed Tasks1–3. **Scope:** fixed new request dispatch, backend delivery/query and local conjunction; management resources remain alive.

Proposed files:

- `internal/runtime/controltransport/task_quiesce_types.go`, `task_quiesce_frame.go`, `task_quiesce_session.go`, focused tests; preserve existing wire schemas.
- `internal/runtime/controlrunner/supervisor_quiesce_transport.go`, narrow `supervisor_transport.go` routing.
- `internal/storage/state/etcd/task_quiesce_delivery.go`, `task_dual_drain.go`, focused authority/fault/TLS tests.

Expose `DeliverTaskQuiescence`, `QueryTaskQuiescence`, `PrepareAfterDualDrain` with Destination-authenticated preparation and finite diagnostic delivery result per spec. One real finite mTLS request with EOF,16conn/5s, exact activated peer/independent backend roots/current runtime cert. Recheck original send capability/fences/intent/deadline before dial and after handshake immediately before bytes. Query-only backend requires configured trust+clock, not signer. Accepted ACK signifies durable owned work, not drain. Lost ACK preserves same reference and unknown; no auto resend, no long-lived handle or remote cancel.

Compose terminal signed target evidence with exact native immutable quiesce intent/common-birth receipt, current original claim and NEW full-prefix limit1 empty Range. Count read-only comparison and response budget explicitly. Error/nonempty/late cancellation returns nil prerequisite. Returned `PreparedTaskQuiescence` is original-current-claim-bound, opaque and private; a later sync/flush consumer must revalidate, not trust an old bool/revision. New claimant may consume historical terminal proof only for its new local prerequisite after full fresh fencing, never reconstruct original effect send. Keep conservative checkpoint states unchanged.

Selectors `^TestTaskQuiesceTransport` and `^TestTaskDualDrain`: genuine TLS/EOF/peer/current-clock negatives, false root same tuple, oversize/truncation, ACK loss then exact query, shutdown with owned worker, terminal certificate expiry, fixed deadline not extended by query, token-empty/local-unknown and local-success/token-nonempty, cold runtime, copied/foreign capability, lost claim, receipt mutation between validation/final barrier, unknown outcome no replacement. Genuine assertion RED, targeted race/vet/build/diff, then independent review.

## Task 5 — mandatory combined physical chain and final unit publication

**Depends on:** reviewed Tasks1–4. This task adds only necessary combined test helper/source and verification reports; no permission to widen implementation by fixture convenience.

Proposed combined source: `internal/storage/state/etcd/task_dual_drain_native_test.go` and a narrowly owned helper file only if required for actual production target transport. Root specifies exact environment/selectors/ELF custody and separate metadata/backend/target topology before freeze. Backend uses real public Begin/PrepareDestroy/Claim/Prepare/Deliver/Query APIs and actual three-member etcd; target is real protected production PID1 and launcher under original resources. No in-memory substitute, hidden production fixture bypass or extra target fault child.

Anchored combined selector: `^TestTaskDualDrainNative(Active|LateArrival|Unknown)$`.

- Active: original real admitted execution crosses destroying and CloseData; real USERS pending/accepted barrier, original stop/join and namespace proof; metadata tokens drain through legitimate cleanup/Lease expiration; final actual prefix empty transaction yields opaque prerequisite. Management TLS query still works. Root verifies actual UID/Boot/PID1/process/cgroup and complete signed attribution. Owner/control/index and remote intents remain unchanged.
- LateArrival: original operation token expires before delayed original user request arrives; target rejects it after durable closure. A registered-before-spawn command is resolved by the original start/stop owner, not dropped from the set. Explain any deterministic host seam versus physically forced interleaving; combined required public execution path still cannot be replaced by those component tests.
- Unknown: actual constrained failure (lost connection, pending/cold/expired claim or unjoined owner as approved by Root harness) produces no fake terminal/prerequisite. Token-zero cannot cover local unknown; local-success cannot cover remaining tokens. Any target isolation exit has its real reason/code/joins recorded separately from test PASS.

Measure finite activated-idle and one-active-quiesce target self PID1 RSS/FD/thread/process/cgroup snapshot with declared diagnostic overhead; metadata/backend costs separately, within original quota/security. No long-run/N/fleet/upgrade/RBAC claim. Exact isolated fixture warnings/errors stay visible. Freeze copied/current/Git source+compiler+ELFs; every Docker action/custody/security/cleanup is Root-owned with complete raw evidence. No independent worker launch or unchanged accepted native/kernel suite replay.

After successful combined chain and required changed-seam evidence: one full new-unit original-range review by independent reviewer, all findings/CannotVerify explicitly dispositioned by Root, source-frozen final relevant checks once (not reflexive whole-repository replay), publish final report with failed/partial runs and limitations. Only then may Root accept this unit and dispatch separate final sync/flush/remote-settlement work.

## Shared execution/report contract

Every author gets exact original BASE and owned file list, binding requirements first, no sibling/closed OWN, no unrelated Sentinel, no dependency installation, no nested author/reviewer, no production cutover/push/merge. Use tests-first where meaningful, retain missing-symbol compile as compile failure rather than behavior RED. Prompt coherent commits, explicit owned staging under the shared current-unit commit lock when parallel ownership is used. Root alone dispatches review/freeze/native fixtures and resolves architecture ambiguities before implementation guessing.

Per-task report includes actual interfaces/files, commands/full stdout/stderr/exit, meaningful RED→GREEN, relevant current/copied/Git/compiler hashes, original-range ownership, native/skipped distinction, absolute time/record/RPC/resource budgets, fault outcomes and unforced limitations. Native output classification must retain expected client faults and server errors without “pristine” claims. No new source edits during Root freeze; failure requires explicit release/new candidate.

## Binding supersession and gates

Specification section9 supersedes draft D1/D2/API/proof alternatives above. Task1 pending/barrier/accepted only; Task2 owns terminal integration and tests under actual kernel proof, not arbitrary completion flags. No QuiescenceHandle; completed live-owner set excludes fully joined history. Original bootstrap seal and same-lock ownerDone/retirement are required section10 corrections. PrepareTaskQuiescence and PrepareAfterDualDrain take Destination and independently authenticate prerequisite receipts. Root preflight reviewer must assess all supersessions, budget and process/callback lifetime before code.

- [x] Independent preflight spec/plan review and individual Root dispositions (C0/I2/M2/CV10; I1/I2 and M1/M2 corrected in spec9/10 and task extraction; scoped confirmation before dispatch).
- [x] Task1 original-range author + independent review (C0/I0/M1; each CV14 dispositioned; actual Linux journal gate passed).
- [x] Task2 concrete proof interface preflight, original-range implementation + scoped fix review (runtime component accepted; downstream/physical limits individually retained in runtime-review report).
- [x] Task3 original-claim metadata/pages original-range implementation + scoped fix review (metadata component accepted; combined physical chain and deployment limits retained in metadata-review report).
- [ ] Task4 transport/dual-drain original-range implementation + review.
- [ ] Task5 combined physical gate, original-range review; one full-unit review and tracked acceptance.

Whole migration remains active after this unit. Keep small verified commits, retain every raw failure and current source attribution. Root alone owns fixtures; no old accepted native/kernel replay.
