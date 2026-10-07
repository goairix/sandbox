# Task1 independent implementation review

Spec compliance: **Approved for the bounded Task1 implementation**. Code quality: **Approved with one nonblocking Minor**. C0 / I0 / M1. The actual Linux journal gate and cleanup evidence are now included. Task1 is ready for acceptance; this is not whole-unit or production-readiness approval.

## Scope and method

Reviewed original BASE `ea63f942a959e427a203c1423d4743d36a513d9f` through `f772c356454bacb44eb21068ddeacdd29060bbaf`: the complete unfiltered 2,303-line / 106,224-byte package `review-ea63f94..f772c35.diff`, SHA256 `9ee649e16f2ef730d15b7ec13c78cda66405f5ffdcedd6da9346a00f5a75194e`, once in contiguous chunks through EOF. All 19 owned files and the Root producer-interface report neighbor were included. Read the original Task1 brief and author report through EOF, the task-reviewer and code-reviewer rubrics, and binding spec sections 9/10. The accepted sequencing overrides the earlier draft terminal-production checklist: Task1 produces pending/accepted ownership and pure terminal cryptography; Task2 supplies trusted physical completion.

Focused unchanged reads addressed concrete inherited-helper risks only: `journal.go:96-115` for context/poison guards; `journal_task_close.go:100-154` for post-IO authentication, original installation/birth binding and installed issuer pinning; `journal_task_close_receipt.go:63-78` for fresh runtime-certificate/window/context checks shared by the new signer. No Git, tests, build, Docker, product mutation, nested reviewers, old OWN or unrelated files were used. Only this review artifact was written.

## Strengths and contract assessment

- `controlprotocol/task_quiesce_types.go` validates both complete contexts, the immutable task/installation tuple, historical digests and intent revision, with the specified equal-claim and later-claim distinction. The bounded execution digest uses the existing `ExecStartContext`, sorted unique command IDs, explicit dispositions and literal `[]` for the empty set.
- The ticket, accepted receipt and terminal receipt have distinct actual-NUL signature domains, recursive strict schemas, their own 8192/12288 limits, zero evidence on errors and copied normalized wire. Terminal cryptography establishes attribution, not physical completion. Existing protocol formats are unchanged.
- `controltarget/task_quiesce.go:20-136` independently authenticates the original durable CloseData record and signed receipt, the new ticket, installed issuer and fresh runtime identity. It authenticates again after pending IO. The barrier is set before persistence; uncertain persistence or late authentication poisons the journal and returns no handle. Exact retries can recover only the same original live in-memory handle.
- `controltarget/task_quiesce_receipt.go:13-55` holds the journal mutex through handle/record/install/key validation and fresh pre/post-sign checks. It exposes neither a callback signer nor a terminal setter, does not retain the key, and excludes cold/copied/foreign/poisoned handles. Its documented ACK limitation correctly leaves registered finite coordinator ownership to Task2.
- Capacity preflight charges current bytes plus actual pending bytes plus the largest valid terminal replacement and two extra slots. The 32/32 split gives maximal two-digit disposition counts for a 64-member terminal record. The new 16384-byte limit is confined to quiescence records; scan/accounting retains canonical temporaries and requires the original terminal CloseData linkage. Cold history remains structural and cannot recreate the accepted handle.
- Tests exercise actual journal IO with before/after failure and cancellation hooks, late authorization, fixed-record conflicts, retained temporaries, copied handles, original records/gate preservation and the signer/Close mutex lifetime. There are no new idle actors, dependencies or production wiring in this slice.

## Findings

### Critical

None.

### Important

None.

### Minor

**M1 — Add the positive full execution-set boundary.** `internal/runtime/controlprotocol/task_quiesce_types_test.go:10-43` tests empty, one/two valid entries and rejection of 65, duplicates, ordering and invalid dispositions, but never accepts exactly 64 sorted unique entries. The implementation's bound is correct on inspection; a positive 64-member mixed-disposition case with an independently assembled canonical hash would protect the promised full usable capacity when the helper evolves. Nonblocking; add with the next relevant test slice. No production change or suite replay is needed to accept this Task1 slice.

## Evidence assessment

Read `root-task-1-author-evidence-audit.json` and independently parsed the retained final race stdout through all 3,764 JSON rows: **17 top-level + 922 subtest PASS**, two packages, no FAIL/SKIP, empty stderr, no race/panic/warning diagnostics found. This is Darwin arm64 race evidence, not Linux execution. The Root audit ties 85 repository inputs (83 Go files plus go.mod/go.sum), three compiler binaries and 25 evidence hashes to the frozen candidate/copies/current checkout. Final vet/build/diff checks exit 0; the Windows build and Linux ELF compilation are compile-only evidence.

Inspected the complete retained stdout/stderr for `protocol-tests-first.json`, `digest-tests-first.json`, `journal-tests-first.json` and `journal-auth-race.json`. The first three fail on missing APIs at compilation. The fourth fails because the historical changed-worker fixture cannot encode its invalid record, before exercising the intended signer check; the final test uses a valid changed worker. None is a genuine compiling behavioral RED. The report accurately discloses this limitation. The first oversized metadata rendering during this review was truncated; a subsequent selective read retrieved all four failure streams completely, without re-running commands.

Root's initial author-evidence reader excluded go.mod/go.sum incorrectly, then corrected its whitelist; that audit setup error does not change product results. The first native preparation attempt copied the ELF with UID501/GID20 instead of required root ownership and was stopped before native execution. Root reports all 21 preparation commands completed and all three inventories were cleaned. It is fixture setup failure, not behavioral RED or native coverage.

## CannotVerify / declined-to-judge

Each item is a retained boundary, not an assertion that the behavior works:

1. **Trusted physical terminal production:** this Task1 has no `CompleteUserQuiescence` or journal terminal signer; validating each original execution handle/receipt and producing a durable terminal record is mandatory Task2 work.
2. **Absolute deadline and coordinator lifetime:** t0 before IO, earliest independent t0+31s isolation, <=1s stop fanout, callback joining and shutdown resource ownership are absent from this slice and must be verified with Task2's real coordinator.
3. **Atomic USERS/start and full owner retirement:** the new journal flag does not by itself implement the supervisor start lock, complete bounded frozen registration set or same-lock ownerDone retirement. Task2 must demonstrate accepted-before-spawn, Start-in-flight and retiring-owner cases before an ACK can escape.
4. **Kernel absence and continuity:** no new original bootstrap seal, namespace/cgroup/proc revalidation, complete census or ECHILD observation is implemented or proved here; a journal test cannot establish these Task2 properties.
5. **Native metadata fences:** the 64-charge reservation, exact positive original CloseData receipt MOD fence, permanent intent/claim deadlines and 65-charge rejection belong to the later metadata task and are not inferred from typed ticket validation.
6. **Authenticated public delivery/reconciliation:** Destination-owned Prepare/Deliver/Query and internally verified historical receipt flows are later tasks; these Journal APIs do not establish transport authorization, finite delivery ownership or original-versus-new claim reconciliation.
7. **Dual-drain metadata proof:** the final 52-comparison plus two-range bounded transaction is not implemented or tested in Task1.
8. **Combined protected-target chain:** the mandatory actual metadata-to-protected-PID1 fixture remains future work. Separate protocol and journal results cannot replace it; the auxiliary namespace/Unix target transport topology remains to be executed without widening product metadata endpoint validation.
9. **Physical crash/power-loss durability:** the IO hooks and cold reopen cases prove fail-closed behavior around actual API operations, not every abrupt kernel/storage crash or power-loss outcome. No such broader guarantee is certified.
10. **Large-N and production overhead:** no large-N throughput, latency, idle-resource or deployed-image overhead measurement is established by this finite protocol/journal suite.
11. **Complete migration and external effects:** FUSE/data drain, remote settlement/termination, End/owner release and production Redis replacement are outside this bounded Task1 review and remain downstream obligations; pending USERS or pure terminal signatures imply none of them.
12. **Test-first behavioral sensitivity:** there is no retained genuine compiling behavioral RED for this Task1. Final behavior and negative cases were reviewed, but I do not claim demonstrated preimplementation failure or mutation-test sensitivity.

13. **Physical maximum-capacity exercise:** byte/slot boundary tests adjust accounting inputs; the native gate does not physically populate 65,536 files or consume the full 256 MiB. Arithmetic and boundary behavior were reviewed, but maximum occupancy performance is not established.
14. **Linux race and unsupported-platform execution:** actual Linux execution is CGO0 nonrace; the race evidence is Darwin. The Windows unsupported-path build proves compilation only, not execution on Windows.

## Native evidence appendix

Read `root-fixture-evidence/task-quiesce-journal-5547336051e7/root-independent-audit.json`, the native command record in `records.json`, and the entire 10,325-byte `018.stdout` through its terminal PASS. The frozen candidate's actual Linux journal gate reports **1 top-level + 69 subtest PASS**, 0 FAIL/SKIP, 62 leaves across 10 direct groups, 0.43s test duration. Native record018 exited 0, without timeout or stderr; stdout SHA256 is `bba47935d39b44f12fc4d3d497544f62c3c8ee336a32b8164112adcebe1a1326`. The audit reports all 27 raw commands exit0, no nonempty stderr, no OOM and all three cleanup inventories empty.

Root's exact-Git-archive rebuild payload SHA256 `d9d9e81ca0d4c9f2dcbd9c97dd8510f23d0dc9cd86355bfd80ef277bb0ae6f31` was verified in the daemon archive with UID/GID0 and mode0755. Its ELF is not the author's ELF: rebuild source/debug paths differ. The audited actual compiled set is **62 repository Go files + 3 compiler binaries**, all copied/current/Git bytes matched. The earlier 81-file snapshot included 19 dependency protocol TestGoFiles that were not compiled; the audit preserves and explicitly corrects that classification using original recorded GoList data, without re-running native execution or changing the candidate. Do not cite 81 as the actual compiled set.

The fixture retained 64 MiB, 0.5 CPU, PID128, read-only root, NNP, network-none and drop-ALL; the journal was root0700 and payload read-only/root-owned. This closes the Task1 actual Linux journal-IO gate. It does not close any of CV1–14, imply physical PID1 quiescence, or upgrade the author's earlier compile-only report into an earlier native run.

## Final assessment

**Spec compliance: Approved. Code quality: Approved with nonblocking M1. Ready to accept Task1: Yes.** The implementation stays within pending/accepted authority, retains the required immutable attribution and fail-closed journal behavior, and has both focused host race and actual Linux IO evidence. Parent must record dispositions for M1 and each CV before proceeding; Task2 must complete and verify the explicitly deferred physical producer and coordinator contract before any accepted ACK is exposed by Supervisor.


## Root acceptance and individual dispositions

Task1 accepted for pending/accepted journal scope, original ea63f94..f772c35; independent spec/quality Approved C0/I0/M1/CV14 and actual Linux gate audited. No source change or replay required.

- M1 — Ruling: defer positive exactly64 sorted unique mixed-disposition independent canonical hash to Task2's new protocol boundary test file; nonblocking correct bound inspected — omission costs regression detection, not current unusable capacity.
- CV1 — Mandatory Task2 original-handle/receipt terminal producer and atomic signer; absent by approved Task1 sequencing, not certified here.
- CV2 — Mandatory Task2 pre-IO t0, independent earliest31s deadline, aggregate1s stop, callback/timer/shutdown joins; journal evidence cannot prove lifetime.
- CV3 — Mandatory Task2 atomic start barrier, complete not-yet-joined set and same-lock ownerDone retirement with deterministic interleavings.
- CV4 — Mandatory Task2 immutable bootstrap namespace/proc/cgroup seal, complete fresh census and nonreaping ECHILD under actual privilege contract.
- CV5 — Mandatory Task3 native64 charge, exact positive prior receipt MOD and65 zero-Grant refusal; protocol validation substitutes none.
- CV6 — Mandatory Tasks3/4 authenticated Destination preparation/delivery/query and exact historical/current claim reconciliation.
- CV7 — Mandatory Task4 final52comparisons+2Range transaction and opaque local conjunction; pages do not establish empty proof.
- CV8 — Mandatory Task5 actual3member backend→protected productionPID1 combined chain; separate component results never substitute.
- CV9 — Retain physical power-loss/kernel/storage crash limitation; hooks and cold reads do not certify abrupt-device durability.
- CV10 — Large existing N, idle overhead, throughput and deployed-image performance remain mandatory migration capacity work; no performance improvement claimed here.
- CV11 — FUSE/finalflush/remote settlement/termination/End/safe release/production Redis replacement remain mandatory downstream, no local receipt implication.
- CV12 — Retain no genuine compiling behavioral RED or mutation sensitivity; missing-symbol and invalid-fixture failures remain honestly classified.
- CV13 — Retain no physical65536file/full256MiB occupancy measurement; arithmetic boundary checks do not establish occupancy performance.
- CV14 — Retain Darwin race versus LinuxCGO0nonrace attribution and Windows compile-only limitation; no unsupported execution claim.

Ruling: accept only independently reviewed bounded Task1 and release its source freeze for approved Task2 ownership — every physical/metadata/production obligation remains explicit — wrong scope interpretation costs false drain or migration acceptance. Root preserves raw review, author evidence and both failed/accepted fixture records in current OWN.
