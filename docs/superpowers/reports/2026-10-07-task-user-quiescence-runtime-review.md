# Runtime user-quiescence component acceptance

Root accepts Task2's bounded runtime component after original-range review and scoped fix-round1: original BASE4fe9c60, baseline32f8c9e, fix BASE11c4b67, fixed runtime commits4d42440/6061026, immutable new-native candidate3dd3f6f. Original review C0/I2/M0; scoped review approves spec and quality C0/I0/M0 with13 unresolved evidence/physical limits retained individually. This is not whole-unit, authenticated transport/combined-chain, production migration, FUSE/remote/End/release or capacity acceptance.

Root consumed both full reviews through EOF, original/full appended author report, six original and six new native raw outputs and independently audited amended source/archive/compiler/security/ELF/daemon custody/cleanup. Tracked runtime-native-verification report separates original4positive top/17sub+2UNKNOWN from new1positive top/0sub+5UNKNOWN. No baseline gate was replayed or represented as exact-current amended code. Host race and Linux CGO0 remain distinct. Root-host I1 regression follows exact production branch; I2 controlled production transitions now have real PID1 execution.

Root dispositions: original CV1–6 resolved only for their explicitly attributed supported component fixtures. CV7/8 retained adversarial physical-census/replacement coverage limits; CV9 retained OS scheduler/uninterruptible-call limitation. CV10 assigned mandatory Task4 real TLS/routing/borrow/shutdown; CV11 assigned mandatory Task5 same physical three-member metadata→production PID1→dual-drain. CV12 requires independent Task3 acceptance and is not granted by runtime review. CV13 retained power-loss/filesystem limitation. CV14 requires measured large-existing-N/active/FUSE capacity in full migration. CV15 requires production/FUSE/remote/End/safe release work. CV16 preserves inherited source/no replay and no Linux race claim. New CV17/18/19 retain exact clock-mutex versus fsync, original context Start versus blocked syscall, and controlled failureOnce versus unmodified cooperative-sweep distinctions below. No CannotVerify item is silently dropped; later gates remain mandatory.

Task1 deferred minor M1 is closed by unchanged task_quiesce_boundary_test.go independently canonical exactly64 mixed disposition test and the original Task2 reviewer source verdict; no Task1 suite replay. There is no unaddressed Critical/Important runtime finding. Source freeze is released only for downstream narrowly scoped transport integration; no existing kernel/owner/journal contract may be weakened.

## Original independent review

# Task 2 review

## Verdicts

**Spec compliance: Needs fixes. Code quality: Needs fixes. C0 / I2 / M0.**

This is the Task 2 gate for original BASE `4fe9c60128a7e4c3b7aae8c1cef2f480d19fc4ef` through frozen HEAD `32f8c9ea51255b094dd72891fcd604c92b0222a5`. It is not a Task 3, whole-unit, or production-readiness verdict. Native evidence still pending at this review cutoff is explicitly separated below.

## Scope and method

Read the task-reviewer rubric, the complete 200-line Task 2 brief and complete 134-line author report. Read the unfiltered original-range `review-4fe9c60..32f8c9e.diff` through EOF: 215,104 bytes, 5,348 lines, 11 commits. A truncated output within the final neighbor-test slice was recovered by a bounded read. Reviewed all 26 Task 2 owned files; neighboring etcd and Root documentation changes were integration context, not an independent Task 3 approval. No Git, tests, build, Docker, installation, source edit, or nested reviewer was used.

Focused context checks were limited to cut-off lifecycle functions: `internal/runtime/controlrunner/supervisor_exec.go:152–234` (whether a successful Start can return an error before installing its sole Wait), `supervisor_exec.go:278–340` (result/terminal ownership), and `supervisor_failure.go:130–190` (what the isolation fallback may block on). The one unchanged seam was `supervisor_activation.go:67–85`, to check whether setting closed before CloseAdmission prevents gate closure; it does not. No broader predecessor recertification was performed.

## Strengths

- `internal/runtime/controlrunner/supervisor_quiesce.go:18–41` moves retirement behind watchdog, pipe and sole-Wait joins, then closes ownerDone and removes the exact original registration under s.mu. This addresses the omitted retiring-owner interval without an unbounded historical map.
- `internal/runtime/controlrunner/supervisor_quiesce.go:43–173` binds one private attempt to the permanent USERS barrier and original frozen registrations, arms its independent deadline before journal acceptance, and separates request cancellation from lifetime-owned cleanup. The stop fanout uses one aggregate opportunity rather than 64 fresh per-entry seconds.
- `internal/runtime/controltarget/task_quiesce_terminal.go:1–197` keeps completion and signing fixed-purpose: original live handles and records, exact local receipts, independently derived counts/digest, protected terminal replacement, fresh opaque observation and certificate checks. The DTO is not treated as cryptographic proof that the Supervisor joined owners.
- `internal/runtime/launcher/pid1_quiescence_linux.go:1–88` and `pid1_quiescence_seal_linux.go:1–215` use the bootstrap seal, bounded pinned proc census and non-reaping ECHILD check. Observation stores no exported FD or replacement authority. Unsupported topology fails closed.
- `internal/runtime/controlprotocol/task_quiesce_boundary_test.go:1` adds the requested independently canonical 64-entry mixed positive digest boundary without changing existing protocol production formats.

## Findings

### Critical

None.

### Important

**I1 — Standalone Close can replace an expired original deadline after its success-owner join crosses that deadline.** `internal/runtime/controlrunner/supervisor_failure.go:76–79`, `supervisor_quiesce_deadline.go:67–85`, and `supervisor_quiesce.go:269–294`.

`isolationDeadlineOwner.succeed` first marks state 2, then waits for done, and can return false because that join finished after the absolute deadline. The standalone stopExecutions failure branch then only starts isolate. Because armIsolationDeadline treats state 2 as stopped, isolate can allocate a new owner with its newly calculated remaining interval (normally 30 seconds once active is empty). The original deadline is thus lost precisely on an unsuccessful completion path; subsequent RecordUnknown/CloseAdmission work can block while the new interval runs. This violates brief lines 138–144: failure must preserve the earliest registered deadline. The quiescence worker's fail path already rearms its original deadline, but standalone Close does not.

Minimum resolution: retain/rearm the original absolute bound before entering this failure path, or make unsuccessful success-owner join leave an expired enforceable owner rather than a replaceable success state. Add a deterministic regression delaying deadline-owner completion after state 2 until beyond the original bound; establish that the fallback cannot receive a fresh interval. Merely checking succeed returns false does not verify the Supervisor path.

**I2 — Required coordinator interleavings are not established by the submitted focused tests.** `internal/runtime/controlrunner/supervisor_quiesce_deadline_test.go:1–64`, `supervisor_quiesce_test.go:10–176`, `supervisor_quiesce_native_test.go:1–176`; binding `task-2-brief.md:176` and `:198`.

The deadline tests exercise primitive shortening/success/expiry. Lifecycle tests manually construct registration states and channels; the Start-in-flight case checks the barrier decision rather than completion of the same original Start owner. The native cases exercise positive quiescence and two physical failure outcomes. These do not exercise the required already-running isolation plus earlier deadline, expiry while pending IO/sign is blocked, repeated Close preserving the same deadline, or a journal terminal that became readable before coordinator failure and must remain unsignable on later query. In particular, checking an already-set failure flag before acceptance cannot establish the post-persistence historical-query rule. I1 is an uncovered integration path of this kind.

Minimum resolution: add focused deterministic tests of those actual coordinator transitions, including same-owner in-flight Start completion and success/callback-owner joining; use controlled existing seams rather than a new framework. Include a failed-readable-terminal query regression distinct from successful historical query beyond 31 seconds. Retain actual behavior RED where a defect is exposed and distinguish setup/API compilation failures. This is missing explicitly required proof for the new concurrency contract, not a request to broaden unrelated accepted suites. Root-owned physical fixtures remain separate.

### Minor

None identified in this task scope.

## Evidence assessment

Reviewed retained `task-2-evidence/coordinator-final-focused.json`: exit 0, clean stderr, four package PASS summaries under race for runner/target/protocol/launcher. The command's historical HEAD is 23fa5cc; its 26 owned source hashes before/after match the final frozen candidate, so that HEAD difference is not a Task 2 source mismatch. Output is not per-test JSON/verbose evidence: it does not justify invented subtest or skip counts, and Darwin execution does not execute Linux-only native tests.

`start-barrier-behavior-red.json` and `terminal-key-behavior-red.json` are actual behavioral failures. `deadline-api-red.json`, `retirement-api-red.json`, and `terminal-api-red.json` are missing-API/helper compilation failures, not behavioral RED. Final retained vet/build/gofmt/diff commands exit 0 with empty error output. `final-source-freeze.json` describes the 26 owned files; `frozen-inherited-equality.json` records unchanged monitor, monitor-drain, transport-lifetime and old exec protocol paths plus compiler identities. These are scoped inheritance evidence, not fresh runtime tests of unchanged code.

The author report's Linux ELF compilation remains compilation evidence. Root reported unsuccessful fixture preparations caused by toolchain selection and Docker inspect representation/custody checks before target start, with empty cleanup inventories; these are preparation failures, not product RED or native coverage. The reported Task 3 metadata batch A is not Task 2 native proof. No completed Task 2 Root native audit was supplied at this report cutoff, so no native PASS is inferred.

## Individually scoped CannotVerify / declined-to-judge items

1. **CV1 — Actual protected PID1 Local selector and successful historical query beyond 31 seconds.** Linux-only runner source is present; completed Root execution, terminal outcome and custody/cleanup evidence are pending. Host package PASS is insufficient.
2. **CV2 — Actual DoubleFork descendant cleanup and final ECHILD.** Requires the Root Linux selector with actual descendant topology and final result; source review alone does not prove syscall behavior.
3. **CV3 — Actual UnknownChild isolation.** Must separately establish the fixed marker, exact exit 70, non-OOM/non-running final state and cleanup; it cannot be counted as an ordinary exit-0 positive test.
4. **CV4 — Actual MonitorLoss isolation.** Requires its own actual failure-outcome attestation, original Wait ownership and exact exit 70; CV3 does not substitute.
5. **CV5 — Linux terminal journal IO/fault and post-clock-poison behavior.** Native source covers these paths, but its actual protected observation and persistence/signing runs are pending; portable tests cannot mint real physical absence.
6. **CV6 — Actual bootstrap-seal/census/ECHILD syscall compatibility under the specified quota and capability profile.** Root must establish this from its executed launcher/target selectors, including bootstrap success; cross-compilation is insufficient.
7. **CV7 — Real namespace/proc/cgroup replacement attacks.** The seal sensitivity test changes private saved seal components, which can prove mismatch rejection but not actual mount/namespace/cgroup replacement. Do not advertise it as live replacement coverage.
8. **CV8 — Actual overflow, disappearance/reuse and partial-read census fault behavior.** Bounded fail-closed code is inspectable; submitted physical fixtures do not establish every kernel race/error branch. A clean empty namespace does not prove these adversarial paths.
9. **CV9 — Deadline execution during a frozen scheduler or uninterruptible kernel operation.** User-space timer ownership cannot prove such execution; retain the brief's explicit operating-system limitation rather than an unconditional wall-clock guarantee.
10. **CV10 — Request transport integration and the unchanged 16-connection/5-second cap.** Relevant transport bytes are inherited, but new end-to-end dispatch/borrow behavior belongs to following task work and is not established by direct private coordinator calls.
11. **CV11 — Mandatory combined three-member metadata → protected production PID1 → dual-drain chain.** Not performed by this review and not replaceable by separate component results. This remains an explicit whole-unit gate even if all Task 2 selectors pass.
12. **CV12 — Neighbor Task 3 metadata protocol/effects.** Read as context in the unfiltered package; no independent Task 3 correctness, fence, reconciliation or integration approval is issued here.
13. **CV13 — Power-loss durability and all filesystem implementations.** Fault-oriented journal checks do not simulate every crash/power-loss/storage implementation. The existing supported durability contract is inherited, not newly certified here.
14. **CV14 — Large-N resource/performance behavior.** Bounded 64-owner logic and a pure 64-entry digest test are not measurements of fleet-scale idle overhead, CPU, latency or memory. No performance claim is approved.
15. **CV15 — FUSE/remote effects, End, release and production selection.** Local user absence does not establish remote settlement, file flush, safe owner release or completed production migration; these are explicitly outside this task.
16. **CV16 — Accepted predecessor suites and Linux race coverage.** No predecessor suite was replayed. Retained source equality supports inheritance only; any CGO0 native run must not be described as Linux race-detector coverage.

## Separate ancillary fixture amendment confirmation

**Scoped utility verdict: Approved; new C0/I0/M0.** Read `root-toolchain-fixture-fix.diff` completely. Its two scripts consistently use the absolute installed Go 1.25.6 executable with GOTOOLCHAIN=local across env/list/build/test2json, retaining isolated SRC and sanitized environment. The local inspector now expects Docker's canonical CAP_* representation and checks the native volume separately from the exact HostConfig.Tmpfs journal/tmp option strings. Those amendments correct representation/toolchain assertions without widening create-time privileges, networking or quotas.

This is inspection of the amendment only, not another full harness review or proof that a fixture ran. The reported preparation failures and cleanup remain retained evidence; final native success/custody/outcome still requires Root's actual audit. No ancillary product conclusion is implied.

## Assessment

The bounded ownership, immutable attempt and fixed-purpose authority structure is coherent, but the unsuccessful deadline-owner join path must retain its original bound, and the expressly required coordinator interleavings need direct evidence. Root should disposition I1/I2 and the separately numbered native/integration limitations before accepting this task; no accepted whole-suite replay is requested.


## Scoped fix-round independent review

**I1 — Standalone Close can replace an expired original deadline after its success-owner join crosses that deadline. — ADDRESSED.** `internal/runtime/controlrunner/supervisor_failure.go:78–90` routes the actual standalone Close decision through `finishStopExecutions`; its failed-success/join branch synchronously registers `originalDeadline` before starting isolation or returning UNKNOWN. `supervisor_quiesce.go:273–296` either shortens the existing enforceable owner or replaces a stopped owner with that same original absolute bound. An asynchronous isolation invocation cannot grant another interval after this registration. `supervisor_quiesce_deadline_test.go:69–130` enters that actual branch, holds the original state-2 owner past its deadline, verifies the branch cannot return before deadline arbitration, then checks the preserved bound and callback join. Retained behavior RED genuinely reports both original defects; GREEN and final focused host race exit 0. The test's safe private callback/failureOnce control establishes this host branch, not physical PID1 shutdown.

**I2 — Required coordinator interleavings are not established by the submitted focused tests. — ADDRESSED within the stated controlled-seam evidence limits.** `internal/runtime/controlrunner/supervisor_exec.go:196–241` extracts the exact original committed Start path into a private helper, retaining the same cmd.Start, watchdog, sole cmd.Wait, pipe and runExecution ownership. `supervisor_quiesce_interleaving_native_test.go:264–382` captures the committed registration with no monitor PID, refuses premature attempt/owner completion, releases that same Start, and checks original Wait/result/watchdog/owner/timer joins plus repeated successful Close. The actual amended-source native selector passes with registered1/local1 and fresh original census/non-reaping ECHILD. The other new cases establish already-running isolation plus an earlier deadline (`:129–162`), expiry under actual pending/signing journal-mutex contention (`:164–208`), readable terminal followed by original coordinator failure and error/nilwire query despite a fresh empty census (`:209–246`), and two real failed Close calls retaining the original attempt/deadline/key while joining the same Start owner (`:384–446`). Root executed all six selectors; five negatives reached their exact markers and actual PID1 exit 70. The prior successful historical query at real 32.023178723s remains separately attributed baseline evidence. This closes the missing controlled coordinator-transition proof; it does not claim stalled-kernel-fsync, blocked-spawn-syscall or unmodified immediate cooperative-isolation coverage.

## Fix-diff verdict

**Spec compliance: Approved for this scoped fix round. Code quality: Approved for this scoped fix round. C0 / I0 / M0 / CV13 retained unresolved.** Both original Important findings are addressed; no new Critical, Important or Minor breakage was identified in the runtime fix. Six original CV items now have supplied component evidence; original CV7–CV16 and three additional explicit physical-test limits remain individually recorded below. CVs are evidence/acceptance limits, not silently converted to passing tests.

**New breakage in the fix diff: None.** The Start extraction preserves the original lock release, sole Wait installation before result ownership, Start-error cancellation/watchdog join and closure of the same six pipe descriptors. The Close extraction adds synchronous preservation of the original deadline without adding an exported hook, new registration, second Wait, replacement output reader or new cleanup budget. No wire, kernel seal, journal capacity or production transport change is introduced by these two runtime commits.

**Out-of-scope observations: None newly identified.** Neighboring etcd reservation/issuer changes and specification/report documentation in the unfiltered package were read as context only. This report does not approve Task3, recertify the accepted 26-file baseline, or replace the later complete-unit review.

## Scope and checks consumed

**Scope check:** Read the entire task-2 brief, original task-2 review including the full I1/I2 findings, complete author report with its appended fix checkpoint and Root resume appendix, and binding design specification. Read supplied `review-fix1-11c4b67..3dd3f6f.diff` through EOF: 1,242 lines, 12 files. Fix BASE `11c4b67be5c2653cb57e1b82aa5a7cca5011fbba`; immutable source HEAD `3dd3f6f848e7ad884334de55b81637301320440d`; runtime commits `4d424406c5f632a2f3684b61d50117336228e11a` and `6061026e0ce665764739482504403fdf6a4840d6`. Later `13828c3` is Root's documentation-only actual-evidence appendix, not a changed product candidate. Followed the scoped re-review prompt; did not regenerate a Git diff, run tests/builds, launch Docker, install dependencies, change product/index/Git state or dispatch another reviewer. Only this review artifact was written.

**Narrow source-context check:** Read the original helper's caller/transfer and Close/deadline-owner arbitration, coordinator final-success/query gates, and exact journal authentication clock call site needed to interpret the new tests. This was confined to verifying the findings and fix behavior, not a second whole-baseline review. In particular, the pending clock pause occurs during original Journal authentication while its mutex is held; it is not a pause inside a kernel fsync.

**Retained host behavior check:** Consumed exact argv, complete stdout/stderr, exit, before/after source and compiler fields in `task-2-evidence/fix1-i1-behavior-red.json`, `fix1-i1-green.json`, `fix1-new-seams-race.json`, `fix1-final-linux-compile.json`, `fix1-final-vet.json`, `fix1-linux-vet.json`, `fix1-final-build.json`, `fix1-checkpoint-gofmt.json`, `fix1-checkpoint-diff.json` and the pause source freeze. The RED is behavioral exit1 with the two actual regression messages, not an API compilation failure. GREEN is exit0/1.578s; the final focused race command is exit0/3.080s with clean stderr. Host/Linux vet, host build, Linux arm64 CGO0 test compilation, gofmt and diff checks exit0 with empty stdout/stderr. No per-test counts are inferred from the host package summary.

**Host attribution check:** Independently compared all 27 owned current hashes to the pause freeze and final focused race source map: zero mismatches. All inspected before/after source maps are unchanged within each run; retained Go1.25.6 executable/compiler/linker hashes match current installed bytes. Historical HEAD labels precede commits but source attribution matches the reviewed owned candidate. Those source supersets are not actual compiled-file manifests, and cross-compilation is not native execution.

**New native outcome check:** Consumed complete `root-fix1-native-runs.json`, enriched `root-fix1-pid1-native-audit.json`, the tracked runtime verification appendix and each of the six raw `024.stdout`/`024.stderr` files through EOF. Wrapper exit0 means evidence/cleanup completion; actual attach and Docker target exits are separately 0 for CommittedStart and 70 for the other five. Exact outcomes: one positive top-level PASS/zero subtests; five UNKNOWN_ISOLATED results with no Go PASS; zero FAIL/SKIP; no timeouts, OOM or remaining running target. All five stderr streams contain the expected PID1/gate_closed/exit70 isolation diagnostic. Their markers precede `expectQuiesceIsolation`, whose missed-deadline fallback exits71 without cleanup, so later cleanup cannot masquerade as the expected deadline outcome.

**New native source/custody check:** Independently verified every retained raw stdout/stderr/stdin hash for all 35 commands per gate (210 command records), each six-run actual 115-file repository manifest against retained archive and current bytes, compiler hashes and the three retained ELF hashes; zero mismatches. Each source manifest identifies Linux arm64/CGO0/Go1.25.6 and the same source archive SHA256 `87c45a2860f22cc544ff5fcdfc30149e5c452afa236da8080a5a477e7f9fac33`. Root's enriched audit additionally verifies daemon tar ownership/mode/bytes, actual image/attestation/runtime ID+UID/security/tmpfs/mount contract and exact negative diagnostics. Independently read actual target pre/final inspect: 64MiB, .5CPU,128PIDs, readonly root, netnone, NNP, original KILL/SETUID/SETGID/SETPCAP bootstrap capabilities; negatives exited70/non-OOM/nonrunning. All six before/after container/volume/network inventories are empty. Linux CGO0 evidence has no race detector.

| New selector | Project suffix | Actual outcome | Source evidence |
| --- | --- | --- | --- |
| CommittedStart | ad097c4d063a | exit0, PASS1top/0sub | `supervisor_quiesce_interleaving_native_test.go:343–382` |
| EarlierIsolation | 7012a505d759 | exit70, UNKNOWN | `supervisor_quiesce_interleaving_native_test.go:129–162` |
| PendingDeadline | 0d79007fded4 | exit70, UNKNOWN | `supervisor_quiesce_interleaving_native_test.go:164–207` |
| SigningDeadline | 49f750224ba5 | exit70, UNKNOWN | `supervisor_quiesce_interleaving_native_test.go:164–208` |
| FailedTerminalQuery | 284f0b4bb580 | exit70, UNKNOWN | `supervisor_quiesce_interleaving_native_test.go:209–246` |
| RepeatedClose | 77854a3ccf02 | exit70, UNKNOWN | `supervisor_quiesce_interleaving_native_test.go:384–446` |

**Baseline evidence check:** Consumed complete `root-pid1-native-audit.json`, the tracked baseline runtime verification report and all six retained original `024.stdout`/`024.stderr` streams with their attach argv/exit/timeout/hash. The original source attribution remains 32f8/11c4: four positive top-level/17 subtest PASS and two actual exit70 UNKNOWN outcomes. No old selector was replayed and no baseline binary is represented as an exact-current amended binary. Target/launcher and historical-query source are inherited context; the new amended-source CommittedStart independently exercises the extracted original Start helper and current successful coordinator.

## Individual CannotVerify dispositions

**CV1 — Actual protected PID1 Local selector and successful historical query beyond31s: component evidence supplied; original pending item resolved within its baseline attribution.** `root-pid1-native-audit.json`, project `task-quiesce-local-9d93b94431fd`, raw `024.stdout` shows genuine success, original census/ECHILD and query at32.023178723s. This is retained baseline execution, not a replay on amended bytes. New project `ad097c4d063a` establishes current helper/coordinator success. Combined transport remains CV10/CV11.

**CV2 — Actual DoubleFork descendant cleanup and final ECHILD: component evidence supplied; original pending item resolved.** Original project `task-quiesce-local-8ea39868560c` raw `024.stdout` establishes DOUBLE_READY plus registered1/local1 terminal and fresh original census/ECHILD. Marker alone is not claimed to prove barrier-time liveness. Original attribution remains explicit.

**CV3 — Actual UnknownChild isolation: component evidence supplied; original pending item resolved.** Original project `task-quiesce-local-f65cc8f6143b`, raw `024.stdout` fixed marker and `024.stderr` user_quiescence_unknown diagnostic, actual exit70/non-OOM/nonrunning and cleanup in the baseline Root report. This is UNKNOWN isolation, not positive quiescence.

**CV4 — Actual MonitorLoss isolation: component evidence supplied; original pending item resolved.** Original project `task-quiesce-local-bdeef49c233b`, raw marker and monitor_lost unknown/no-terminal diagnostic, actual exit70 and original owner/wait observations. It is distinct from UnknownChild and retains original source attribution.

**CV5 — Linux terminal journal IO/fault and post-clock-poison behavior: component evidence supplied; original pending item resolved for the tested faults.** Original project `task-quiesce-local-180f8f271030` passes success plus six fault subtests; readable terminal plus fresh empty namespace is unsignable after dir-sync/post-durable-clock failure. New project `284f0b4bb580` additionally establishes actual Supervisor refusal after failed original coordinator. No stalled kernel fsync or power-loss claim follows; CV13/CV17 remain.

**CV6 — Actual bootstrap-seal/census/ECHILD syscall compatibility under quota/capability contract: component evidence supplied; original pending item resolved for the supported fixture.** Original project `task-quiesce-local-9be09879d413` and new positive `ad097c4d063a` report actual PID1 original boundary/ECHILD. Root security/custody audit and actual new inspections retain the stated quotas and contract. This is Linux arm64 cgroup-v2 fixture compatibility, not all architectures/kernel/seccomp/LSM configurations.

**CV7 — Real namespace/proc/cgroup replacement attacks: retained CannotVerify.** Baseline saved-seal perturbations prove sensitivity against unchanged kernel, not actual replacement. `root-pid1-native-audit.json` Seal selector and its raw output explicitly preserve this limit. No new replacement fixture was introduced.

**CV8 — Actual overflow, disappearance/reuse and partial-read census faults: retained CannotVerify.** Clean empty census and seal mismatch sensitivity do not physically exercise every bounded-scan kernel race/error branch. No new gate closes this item.

**CV9 — Deadline execution during frozen scheduler/uninterruptible kernel operation: retained CannotVerify/explicit OS limit.** Neither host race nor short native expiry guarantees a user-space callback executes during scheduler/kernel suspension. Binding design sections4/9 retain this limit; exit70 is unknown, not all-joined proof.

**CV10 — Request transport integration and unchanged16connection/5second cap: retained CannotVerify in Task2.** Direct private coordinator tests do not establish future activated TLS dispatch/borrow integration. Task4 owns that end-to-end evidence; inherited transport bytes do not close it.

**CV11 — Mandatory combined three-member metadata→protected production PID1→dual-drain chain: retained CannotVerify and mandatory later gate.** No new selector composes that chain. Task5 must supply actual combined evidence; separate component results cannot substitute or authorize the next phase.

**CV12 — Neighbor Task3 metadata protocol/effects: retained declined-to-judge.** Neighbor diffs were context only. No independent reservation, issuer, fence, reconciliation or integration verdict is issued by this runtime review.

**CV13 — Power-loss durability/all filesystems: retained CannotVerify.** Tested fault returns and tmpfs protected journal do not simulate every crash/storage implementation. Inherited durability is not newly certified.

**CV14 — Large-N resource/performance behavior: retained CannotVerify.**64-owner bounds/digest and small component runs do not measure fleet idle overhead, CPU, latency, memory or capacity. No measurement claim is approved.

**CV15 — FUSE/remote effects, End, release and production selection: retained out-of-scope/declined-to-judge.** Local sender absence is not flush, settlement, release or migration. Neither new nor baseline component evidence grants those outcomes.

**CV16 — Accepted predecessor suites and Linux race coverage: retained CannotVerify.** No predecessor suite was replayed. Host Darwin race is separate; all current native binaries are Linux CGO0 and have no race detector. Generated wrappers/stdlib/module-cache supply chain is also not independently certified by these manifest checks.

**CV17 — Expiry during an actually stalled kernel fsync: retained new physical-test limit.** `supervisor_quiesce_interleaving_native_test.go:173–204` blocks the configured AuthorityClock while the real journal mutex is held. This establishes lock-independent expiry in real authentication/signing ownership. It does not physically stall a filesystem syscall, even when terminal bytes are readable. The fix report and Root audit correctly preserve that distinction.

**CV18 — Start blocked inside the actual spawn syscall: retained new physical-test limit.** `supervisor_quiesce_interleaving_native_test.go:249–261,298–340` gates exec.Cmd context preflight inside the same original helper before kernel spawn. The actual release/sole-Wait/join result is established; a stuck kernel Start syscall is not physically injected.

**CV19 — Unmodified immediate cooperative isolation in failed-terminal/repeated-Close windows: retained new physical-test limit.** `supervisor_quiesce_interleaving_native_test.go:215,390` consumes failureOnce to preserve an observation window. These tests establish query refusal, timer identity/no extension, key retention and actual independent original-timer exit70 after controlled shortening. They do not prove the unmodified immediate cooperative sweep in those exact windows. EarlierIsolation does enter the actual cooperative owner without consuming failureOnce; baseline monitor-loss/unknown-child remain separate original outcomes. The new cases assert the original31s timer identity before earlier expiry and do not independently wait31s.

**Fix round: All findings addressed, no new Critical/Important breakage.** Root may accept this Task2 fix gate with the individually retained evidence limits; this verdict grants no Task3, Task4/Task5 combined, production, remote settlement or release approval.
