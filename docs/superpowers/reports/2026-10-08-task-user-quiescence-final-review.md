# Final Task5 and original user-quiescence unit review

Reviewer: the single independent final reviewer, 2026-10-08. Scope is the filled `final-unit-review-brief.md`, read through EOF, and the task/code-review templates supplied in the dispatch. This report is the sole final report; it does not authorize downstream migration or a new implementation.

## Verdicts

| Scope | Spec verdict | Quality verdict | Findings |
| --- | --- | --- | --- |
| Task5 combined fixture and its approved evidence contract | Satisfied within the expressly retained limitations below | Acceptable for the scoped fixture | Critical 0, Important 0, Minor 0 |
| Complete original user-quiescence unit | Needs fixes: the required five-second finite request-handler lifetime is not maintained across pending work/lock acquisition | Needs fixes before unit acceptance | Critical 0, Important 1, Minor 0 |

Task5 has actual corrected Active, LateArrival and Unknown physical assertion passes, attributable protected production PID1 and three-member native metadata, and completed scoped cleanup. These results do not close the new cross-cutting I1 below. The successful Task4 writer-deadline fix remains valid for socket writes; I1 concerns the lifetime of the handler and its borrowed slot, a different boundary.

## Important finding I1 — Pending work can retain finite transport borrowers beyond five seconds

**Location:** `internal/runtime/controlrunner/supervisor_quiesce.go:112–120`, with the lock acquired at line 75; related `supervisor_quiesce_transport.go:13`, `supervisor_quiesce.go:250`, and `supervisor_transport.go:85–103`.

`beginUserQuiescence` installs the original independent `context.Background()` deadline at t0+31s, then calls `AcceptUserQuiescence` synchronously on that context while holding `s.mu`. Its transport caller has not returned. The five-second context and connection deadline in `serveConnection` terminate network operations, but they do not interrupt this synchronous pending-work call or the unbounded mutex acquisitions. The borrowed handler is released only by the defer after `serveConnection` returns; the transport slot follows the same return boundary. Other finite requests can also remain waiting for `s.mu` behind that work after their own deadlines expire.

**Trigger and consequence:** a pending operation that cooperatively completes within its legitimate 31-second coordinator budget, but after the requesting connection's five-second budget, retains that connection's borrower/slot past five seconds. Waiting requests can consume the remaining finite slots. A controlled context-honoring six-second pending-work wait is sufficient to expose the ownership error; neither an indefinitely blocked syscall nor a stopped Go scheduler is necessary. Closing or timing out the socket cannot make a Go mutex acquisition or the independent pending operation return. The independent PID1 isolation fallback bounds a different, longer lifetime and does not satisfy the finite-handler contract.

This violates the binding request-handler requirement, separately from the required continuation of already owned cleanup after caller expiry. It is a liveness/resource-bound defect; I did not find a path that turns this delay into a forged successful receipt. Approved uninterruptible-OS/scheduler limitations remain separate CannotVerify items, not a waiver for the cooperative ownership mismatch.

**Required correction:** give the one original coordinator ownership of pending persistence and its continuation independently of the finite transport borrower. Let the transport wait only within its original five-second context for an accepted/result/error outcome. Make relevant lock/admission waits context-aware or move them behind that independently owned coordinator. Do not cancel accepted cleanup on client timeout, mint a second coordinator, release a slot while an unowned handler continues, or renew the original 31-second deadline.

**Meaningful validation for the correction:** a context-honoring delayed pending operation must outlive the client budget while the finite borrower/slot is released by the original five-second bound; the same original coordinator must remain solely owned and finish or fail under its original 31-second deadline. Include a finite query/second request waiting behind the same lock and joined shutdown. Existing slow/nonreading network tests alone cannot cover this. No new test or fixture was executed by this reviewer.

## Review coverage and evidence method

The whole original range is `3f1e2698ffaa0b46f427291ae66c6c72c1e1b6e2..a7ebee70ae273e2cf473af5a7c6157665745006c`; Task5's original base is `cf58a66051ec1f7b1ea1dc4c8bc7c376c93c9857`. I consumed the supplied unfiltered commit/stat/unified-40 package through EOF: 16,117 lines, 864,348 bytes, SHA256 `71e9922044cfc01ec4a6eb849785585e71432602bbbc17845c68e7ff4e0ea275`. This included protocol, barrier/journal, original execution lifecycle, namespace seal, reservation/Stage/history, authenticated finite delivery/query, native operation drain, opaque dual-drain and Task5 source/tests, plus historical reviews and Root verification reports. Truncated tool deliveries were recovered in bounded portions; the protocol strict-test cutoff was also recovered. Subsequent narrow diff navigation was used to index finding lines and inherited dispositions, not to regenerate or repeat a whole source review.

I read the frozen author report and recovered its initially truncated middle, then read only the newly authorized final append in live `task-5-report.md`. The append's parser failures are read-only evidence-processing mistakes, not fixture implementation failures. Author checks are reported evidence, not tests independently rerun here. Host SKIP is never counted as native PASS.

**One focused unchanged dependency check:** `internal/runtime/controlrunner/supervisor_transport_lifetime.go`, for the named risk that cancellation might independently release a borrowed handler or provide an unseen five-second join. It cancels/closes connections on shutdown and joins borrowers, but `transportBorrow.release` remains caller-driven after handler return. This confirms rather than repairs I1. No other unchanged dependency investigation, closed sibling OWN read, nested reviewer, native replay, build/test, install, Git/index/HEAD mutation or product edit was performed. Only this requested report was written.

I parsed the five exact combined evidence directories named in the brief. Across them, all 893 command records and all 1,786 stdout/stderr streams passed recorded length/SHA256 checks: 476,477,194 aggregate bytes. All recorded commands exited zero and none timed out. This does **not** imply all native tests passed: Docker log/wait commands can exit zero while the contained test exits one. Binary/private custody streams were hashed as bytes and were not printed or treated as text. I inspected contracts, manifests, public protected-file inventories, payload custody metadata, target/backend/relay inspections, results, cleanup inventories and Root audit fields. I also checked the authoritative backend logs, actual frame, native snapshots, warning origins, and Unknown's numbered daemon-fault commands where causal attribution mattered. This is retained-evidence inspection, not evidence regeneration or independent supply-chain certification.

## Physical results and fault attribution

All five projects retain the eight-volume/three-ELF contract, three distinct metadata member identities, 281 repository compilation inputs, three Go tool identities, Linux arm64 CGO=0 attribution, root-owned protected config/custody file modes, and target security attestation. The target's mounts contain payload/control/clock, not metadata sockets or private custody. Its attested security is 64MiB, 0.5 CPU, PID128, network none, read-only rootfs, no-new-privileges, four bootstrap capabilities with all others dropped, and fixed journal/tmp tmpfs. Actual production entrypoint is `/native/sandbox-launcher`; backend/relay run the fixture ELF in separate containers. Attestation JSON hashes match their recorded contract digest. Final container, volume and network inventories are empty for every project.

| Project suffix and source | Retained outcome | Review interpretation |
| --- | --- | --- |
| `0e054d6e3b3b`, `05615b4` | Active test passed; 143 commands; cleanup complete | **Partial only.** Doubled-slash snapshot selected just the nonce, so permanent-record invariance was unproved. The corrected gate does not erase this defect. |
| `14d86783b6d5`, `bb64c11` | Active PASS, 31.17s, one top-level/zero subtests, 143 commands | Valid corrected Active chain. |
| `9dcfe8d5fde0`, `0039d56` | Late FAIL, 0.28s, 120 commands; cleanup complete | Canonical decoded zero stdin was nonnil-empty while original was nil. Failure preceded delayed-input assertions. No Late behavioral proof is taken from it. |
| `2ee34dbbbeb8`, `320e3a4` | Late PASS, 67.30s, one top-level/zero subtests, 143 commands | Valid delayed-original-frame and independently attributed current-certificate-expiry case. |
| `204ab0408523`, `a4caf91` | Unknown PASS, 32.16s, one top-level/zero subtests, 344 commands | Valid sole-send ACK loss, original historical queries, native zero conjunction and separately failed-target refusal. |

The original-range package carries the later frozen candidate and the authoritative evidence associations; no additional actual run at `a7ebee7` is inferred. Earlier component physical tests remain tied to their supplied frozen-source closures rather than relabeled as Task5 executions.

**Corrected Active.** Authoritative backend record 69 has SHA256 `a1a28ec920f1eb581656c3e824fb2e58d510ce31f5e8848b285f12415fc99b4f`. Original UID1000 hold and double-fork owners actually cross destroying/DATA closure while active; their original execution waits and authenticated local receipts contribute registered=2, never-spawned=0, local-terminal=2. The signed execution-set digest is `469c2ab998073a8a16b0b43b3ead431c597c373622287f9a386919fff6bb724b`. The retained real operation token blocks the conjunction until natural original lease expiry; then the public fresh native empty prerequisite succeeds at revision 50 with literal 55-node shape. Independent full KeyValue comparison confirms 50→42 records, 39 byte/create/mod/lease-identical retained records, exactly three unleased additions (reservation birth 43; intent/receipt birth 45), and 11 disappearing records on original creation/operation leases. Twenty required anchors occur in both snapshots. Target remains live; actual active sample is 2 with RSS 12,708KiB, cgroup memory 19,406,848 bytes and 22 PIDs, including measurement overhead. This is one sample, not a fleet budget.

**Late.** Backend record 69 SHA256 `5cd4934489fc47cbfeb4cf7cc4e7abfcd205524718f0d7ee8ecb09571ca1743e` retains the exact original 4,192-byte frame, header length 4,188 and SHA256 `12973ea6463362f53362be41ef1c368350241b3b72c9cb259da492ef1a882375`. After actual original token disappearance, Backend delivery refuses with zero dials/application bytes. The separate delayed packet traverses fresh genuine TLS and EOF, writes all original bytes, and receives real EventError 5 while the current runtime certificate is valid. Pre/post process census and gate remain unchanged; terminal counts are all zero and digest is canonical `[]`. Expired original ticket and closed DATA/USERS gates are both present, so **closure-only causation is not established**. Full snapshots independently confirm 40→38 records, 35 unchanged, only three additions at 33/35/35 and five original leased expiries. At actual UTC `20:55:28.264028503Z`, the original certificate ended at `20:55:26.130571833Z`: fresh TLS fails after one actual dial; public query and prerequisite refuse before dial. The live daemon does not imply successful authentication after expiry.

**Unknown.** Backend record 272 SHA256 `bda01e0a5b5eecbb32748e6465a0eb2337b6ae585ed8d09b48b61c735db77767` first shows actual native zero without local quiescence evidence and nil prerequisite. Its sole authorized send performs actual write EOF/read close once and returns ErrDeliveryUnknown. The test-only faulty Dial adapter deliberately violates healthy read preservation; this is an explicitly approved fault seam, not a production transport recommendation. Unknown alone is not delivery proof. Fresh authenticated same-reference query subsequently returns the authentic terminal. Queries at `21:06:44.263471344Z` and `21:06:45.284533511Z` preserve the original `21:06:12.174415386Z–21:06:42.174415386Z` interval and receipt hash `aa9e2aea34bb9e3638fb81ba9fe39f3913dc33dae231b4345574cda7ce741208`. No resend/re-sign/replacement/new preparation extends that authority. Public conjunction succeeds at empty revision 32, reservation 28 and intent/receipt 30. Full snapshots independently confirm 35→36 records, 33 unchanged, those three additions and two original creation-lease expiries.

Unknown's later Root commands 263–270 kill the exact already-quiesced target with SIGKILL, observe exit137/non-OOM, restart the **same** container, observe exit1/non-OOM and `sandbox-launcher: invalid supervisor configuration`, then signal the backend. Actual Id/Config/HostConfig/Mounts/Image match across before/killed/cold inspections. The stale socket refusal occurs before new Bootstrap/Boot/key, and each postfailure public query/prerequisite performs one real refused dial under the still-current original certificate ending `21:41:14.022896219Z`. This is failed-target refusal, not fresh-Boot recovery, local-pending isolation, or proof that killing live users constituted a drain. No isolation70 is expected or counted as success here.

**Warnings and bounded bridge evidence.** I matched classified warnings to their numbered raw streams. Each valid fixture has six mixed HTTP/gRPC warnings, three data-directory-0755 warnings and three simple-token warnings. Corrected Active additionally has two schema/term detection WARNs and one `version/monitor.go:120` storage-version ERROR at startup; typed totals are **14 WARN + 1 ERROR**, despite the legacy aggregate `server_warning_count=15` field. Actual member/quorum checks subsequently pass, but neither pristine logs nor successful storage recovery is inferred. Late and Unknown have 12 WARN/0 ERROR; all three have zero classified client WARN/ERROR/FATAL/PANIC. These are bounded fixture conditions, not production-security acceptance.

Bridge join records show zero retained pairs and three listeners with complete error counts. Corrected Active backend/relay totals are 50/52 (34/36 omitted detail); Late 50/53 (34/37 omitted); Unknown 46/54 (30/38 omitted). Retained EOF/partner-close outcomes are distinct from retained first reset/broken-pipe failures. The omitted error details cannot be individually classified; total counts and completed joins do not justify classifying all of them as benign. Initial partial Active has its own 48/52 totals and is not substituted for corrected evidence.

## Whole-unit source assessment

The protocol uses distinct ticket/accepted/terminal purpose domains, strict typed linkage of current and historical DATA closure, positive birth ordering and exact equal-claim identity. Bounded schema/wire limits and canonical sorted execution-set hashing are covered by positive/negative boundary source tests, including exact-limit inputs and empty `[]`; diagnostic history does not mint live capability.

The USERS barrier is installed under the same admission lock as the frozen original-owner set. Start commitment, the sole original `Wait`, callback/watchdog/pipe completion, `ownerDone`, retirement and terminal evidence preserve original ownership. Never-spawned attribution is restricted to the original pre-commit path. The independent t0+31s timer is armed before pending I/O, with no new user stop epoch, and terminal promotion requires successful original ownership and current physical observation. Failure-readable terminal bytes are not promoted into live proof. The journal reserves bounded transition capacity, uses strict pending/terminal linkage and durability sequencing, and fails closed after poison/cold state. These properties do not remove I1's borrowed-handler problem.

The original production PID1 kernel object captures namespace/proc/cgroup seal state before user admission. Fresh bounded PID1-only census and nonreaping Waitid ECHILD are required, with seal checks surrounding observation. This supplies local namespace absence on the tested supported Linux setup; it does not imply remote side-effect settlement or all-kernel adversarial robustness.

The task-scoped unleased immutable reservation precedes provider Certificate/Sign, Grant and Stage allocation. The original uncertain reservation can reconcile by read only; a later claimant cannot adopt a durable predecessor, and the delayed absent predecessor packet retains obsolete claim comparisons. Reserved Stage commit carries the positive reservation MOD fence; history checks reservation association and birth, both original intents and common-birth committed receipts. Full serialized pre-Grant accounting includes begin/commit/resolve branches and response envelopes; the actual commit shape is 58 comparisons + 2 writes + 4 Else reads = 64. Generic Stage behavior retains its separate compatibility attribution.

Fresh authenticated query verifies independent Backend trust/current runtime certificate and original historical interval without reconstructing send authority. Delivery consumes the one original send attempt before bytes. The opaque local dual-drain prerequisite combines exact terminal evidence with a new full-prefix linearizable empty observation and original-current claim/history pins, then rechecks terminal/history/liveness. Final empty observation is 53 comparisons + two one-result Range branches = 55. It grants no automatic future flush/End/release authority. The closed unit's prior reservation and issuer-fence defects are addressed in this original range; I found no new Critical/Important issue in these metadata boundaries.

## Individual inherited CannotVerify dispositions

“Resolved for scope” below means supported by the stated source and retained evidence, not universally verified. “Retain” names an actual limit. “Downstream” is not acceptance of the excluded consumer.

### Preflight and producer-interface obligations

| Original item | Final disposition |
| --- | --- |
| Preflight CV1 supported Linux Waitid | Resolved for tested Linux arm64 production PID1 by component and combined physical absence paths; other kernels/platforms remain unverified. |
| Preflight CV2 original seal | Resolved for original bootstrap seal plus repeated current checks in the tested configuration; physical replacement attacks remain runtime CV7. |
| Preflight CV3 absolute OS deadline | Retain: scheduler freeze/uninterruptible OS execution cannot be proved by these tests. Ordinary finite-handler ownership is separately **I1**, not folded into this limit. |
| Preflight CV4 original-owner races | Source and accepted Task2 deterministic/native ownership evidence address the selected interleavings; exact forced public timing remains Task4 CV6. |
| Preflight CV5 concrete interface | Resolved by actual opaque producer/transport/Backend APIs in this range; no public boolean substitutes for evidence. |
| Preflight CV6 native CAS | Resolved for scoped reservation, Stage, delayed/tamper/history/native-empty behavior by accepted Task3 evidence and actual Task5 conjunction. |
| Preflight CV7 combined physical chain | Resolved by the three corrected physical Task5 cases, with their distinct conditions. |
| Preflight CV8 I/O/cold/capacity | Fault-boundary/cold fail-closed semantics have component evidence; physical full-capacity and power-loss universality remain journal CV9/CV13. |
| Preflight CV9 N/security/provider preemption | Retain individually: no fleet budget; fixture security is one attested configuration; dependency contracts do not prove forced preemption of an uncooperative provider. |
| Preflight CV10 downstream work | Declined: flush/remote/End/release/migration not implemented or requested here. |
| Producer CV1 actual Task1 reconciliation | Resolved by the completed journal/protocol and concrete caller linkage. |
| Producer CV2 lock/owner linkage | Frozen-set and retirement source/component evidence support linkage; finite transport lock waiting has newly identified I1. |
| Producer CV3 original finite deadline | Coordinator source/component evidence supports no reset; OS limits remain; transport borrower duration fails I1. |
| Producer CV4 seal implementation | Resolved for tested original-seal/fresh-observation path; adversarial physical coverage remains limited. |
| Producer CV5 terminal faults/history | Resolved for enumerated fault/readability/poison/history cases; not power-loss proof. |
| Producer CV6 combined chain | Resolved by corrected Task5 actual public chain. |
| Producer CV7 excluded N/downstream | Retain N measurement gap; decline downstream acceptance. |

### Journal review obligations

| Original item | Final disposition |
| --- | --- |
| CV1 physical terminal producer | Resolved for supported PID1 through component and actual combined terminal evidence. |
| CV2 original absolute deadline | Resolved at coordinator/source/component level with retained runtime CV9; I1 separately blocks finite transport compliance. |
| CV3 barrier/retirement atomicity | Resolved for reviewed same-lock registration/freeze and owner join/removal; accepted deterministic interleavings retain component attribution. |
| CV4 seal | Resolved for tested original seal/current observation; runtime CV7/CV8 remain. |
| CV5 metadata integration | Resolved by reservation/history/issuer fences and native Stage/empty evidence. |
| CV6 transport integration | Authentication/EOF/deadline-clamped writes addressed; **not fully resolved because I1 leaves handlers/slots over budget**. |
| CV7 final native conjunction | Resolved by current 55-node transaction (the superseded 52-node expectation is not used), authentic local proof and actual public prerequisite. |
| CV8 combined integration | Resolved by corrected Task5 chain, not the first partial Active. |
| CV9 power loss/filesystem universality | Retain: enumerated I/O hooks and native tmpfs executions are not power-cut or all-filesystem durability testing. |
| CV10 N overhead | Retain: Active supplies one measured active=2 sample only. |
| CV11 downstream semantics | Declined: no remote/FUSE/End/release certification. |
| CV12 genuine Task1 behavioral RED | Retain historical process-evidence limitation; later passes do not invent a missing compiling behavioral RED. |
| CV13 physical capacity edges | Retain: no physical 65,536-entry/full-256MiB execution is supplied; arithmetic/boundary models do not establish that footprint. |
| CV14 platform/race attribution | Retain: Darwin race, Linux CGO0 execution and Windows compilation have different evidentiary scopes; none is Linux native race proof. |

### Runtime review obligations

| Original item | Final disposition |
| --- | --- |
| CV1 actual local/historical query | Resolved by accepted Local component case and fresh Task5 original receipt queries; no post-expiry effect-send capability follows. |
| CV2 actual double-fork join | Resolved for accepted component case and Active's genuine original double-fork owner/leaf across closure. |
| CV3 unknown-child refusal/isolation | Accepted component physical attribution remains; Task5 does not replace it with its post-terminal SIGKILL. |
| CV4 monitor-loss refusal/isolation | Accepted component physical attribution remains; Task5 does not reproduce monitor loss. |
| CV5 terminal I/O faults | Enumerated accepted seven component fault cases remain valid; combined normal terminal is additional, not replacement fault evidence. |
| CV6 physical seal/cgroup | Accepted cgroup-v2/original PID1 component attribution is supported by current combined attestations and observation; other setups not proven. |
| CV7 live namespace/proc/cgroup replacement attacks | Retain: private saved-seal sensitivity checks are not physical replacement attacks. |
| CV8 overflow/PID reuse/disappearance/partial reads | Retain: strict bounded source checks exist, but the full adversarial physical matrix is not supplied. |
| CV9 frozen scheduler/uninterruptible OS deadline | Retain as an execution-model limit. I1 is a different cooperative-path defect. |
| CV10 integrated 16-slot/five-second finite transport | Socket bounds and component saturation/deadline tests do not establish handler release through independent pending work. **I1 remains open.** |
| CV11 combined same-runtime proof | Resolved by corrected Active/Late/Unknown source and retained production-chain evidence. |
| CV12 neighbor metadata review | Resolved for this original unit's reservation, issuer, Stage/history and fresh empty boundaries, reviewed here with accepted native evidence. |
| CV13 power-loss/all-filesystem behavior | Retain; normal tmpfs/fault injection cannot prove this. |
| CV14 large-existing-sandbox performance | Retain; one active/idle fixture measurement is not N/capacity evidence. |
| CV15 FUSE/remote/End/release/production | Declined as outside this unit. |
| CV16 predecessor replay/platform/supply chain | No predecessor replay performed or inferred. Retain Linux race gap and independent generated-wrapper/module-cache/stdlib/toolchain supply-chain limits. |
| CV17 clock/mutex pause versus stalled fsync | Retain distinction: controlled pause establishes timer logic, not preemption of stalled fsync. I1's correction needs its own cooperative borrower-lifetime regression. |
| CV18 controlled original Start versus blocked kernel Start | Retain distinction: deterministic original-owner Start gating is not proof of interrupting a blocked kernel syscall. |
| CV19 controlled failed-terminal/Close versus natural isolation | Retain attribution: failureOnce-controlled cases prove their stated ownership path; actual earlier isolation cases remain separate. Task5 selected post-terminal daemon failure adds neither local-pending isolation70 nor unmodified cooperative-isolation proof. |

### Metadata and transport review obligations

| Original item | Final disposition |
| --- | --- |
| Metadata CV1 actual metadata behavior/warnings/cleanup | Resolved for accepted Task3 amended paths by supplied native audits, with baseline warning attribution retained. New Task5 actual snapshots/three-member checks provide integration evidence. |
| Metadata CV2 combined chain | Resolved by corrected Task5 physical chain. |
| Metadata contract CV1 serialized pre-Grant budget/generic compatibility | Resolved for reviewed complete request/reply accounting and accepted covering host/native checks; current code preserves the reserved seam. |
| Metadata contract CV2 uncertainty/cold/delayed/tamper | Resolved for the enumerated native cases and strict immutable reservation/history source. No cold send adoption is inferred. |
| Task4 CV1 public metadata/PID1/dual-drain chain | Resolved by corrected Task5 actual public interfaces on production PID1. |
| Task4 CV2 real production ACK loss/same original query | Resolved by Unknown's approved real EOF/read-close fault followed by independently authenticated original terminal; ErrDeliveryUnknown alone remains insufficient. |
| Task4 CV3 live owner/coordinator/shutdown joins | Active resolves applicable real user quiescence joins. Retain daemon teardown while users are still live: Unknown deliberately faults after terminal, and existing component shutdown evidence remains separate. I1 blocks finite-handler ownership compliance. |
| Task4 CV4 actual current-certificate expiry and cold/failed refusal | Expiry resolved by Late; failed-target refusal resolved by Unknown under current certificate. Retain successful fresh-different-Boot cold recovery, absent from selected stale-socket case. |
| Task4 CV5 original deadline unchanged by repeated queries | Resolved for original retained ticket/draft/receipt through actual after-expiry queries and source coordinator identity. This does not resolve I1's per-request handler duration. |
| Task4 CV6 delayed input/interleavings/record invariance | Actual original delayed frame and complete unchanged records are supported; closure-only rejection cause remains unisolated. Active proves real live owners crossing closure; exact forced public before-spawn/Start-in-flight timing remains component-only. |
| Task4 CV7 future consumer revalidation | Downstream: future flush/sync must freshly validate its original-current claim, exact immutable pins, runtime evidence and full-prefix zero. The opaque prerequisite is not approval of that future operation. |
| Task4 CV8 Linux race/N/migration/supply chain | Retain Linux native-race and fleet-capacity gaps; decline production/Redis migration acceptance; retain uncaptured supply-chain scope. |

## Remaining CannotVerify, separated from source defects

The tables above preserve each inherited item. The following Task5-specific residuals are not new source findings:

1. Exact forced public registered-before-spawn and Start-in-flight timing: retained deterministic Task2 attribution, not a physical combined claim.
2. Closure-only causation for the Late external packet: ticket expiry and durable DATA/USERS gates are confounded; actual bytes, absence, TLS and refusal are separately established.
3. Successful fresh-different-Boot cold recovery: selected stale-socket restart refuses before new Boot/key.
4. Local-pending claimant-loss/isolation70 across the full combined chain: Unknown has zero users and later faults only after authentic terminal.
5. Daemon shutdown with live original users: Active proves quiescence joins, while daemon teardown/fault happens after joins.
6. Every omitted bridge error origin: the bounded 16-detail retention preserves counts but not all per-error classifications.
7. Recovery interpretation of Active's startup storage-schema ERROR: later quorum/replication success does not establish persistent-schema repair or pristine service health.
8. Fleet/N/capacity/performance: one active=2 sample and idle/terminal samples do not establish production overhead limits.
9. Linux native race execution and all supported kernel/filesystem combinations: supplied host race and Linux CGO0 gates have separate scope.
10. Independent supply-chain certification of generated wrappers, standard library, module cache and complete toolchain provenance: manifests cover recorded inputs/tool hashes, not those broader claims.
11. Universal wall-clock preemption, all power-loss states and physically full journal capacity: bounded source/fault cases and ordinary fixture runs are insufficient. This limitation must not absorb I1's correctable cooperative ownership defect.

## Declined-to-judge items

Each item below is explicitly outside the requested acceptance, not a passing verdict and not an alleged defect in this unit:

1. Final sync/flush and FUSE handoff: no consuming implementation or fresh final-boundary proof is requested.
2. Remote side-effect settlement: `users_quiesced` is local process evidence only.
3. Business `End`, destructive cleanup and `safeRelease`: no consumer contract is implemented/accepted here.
4. Scheduler integration and whole migration: the fixture/public capability chain is narrower than product rollout.
5. Production deployment/security readiness: isolated fixture quotas, plain metadata loopback/bridge setup and warnings are not a deployment approval.
6. Restore/recovery rollout: strict old-Boot refusal and historical diagnostic reads do not certify operational restoration.
7. Large-existing-sandbox N/capacity acceptance: requires its own authorized measurements and budgets.
8. Redis removal or complete production migration: no evidence in this unit justifies that decision.

The next necessary action is the one scoped I1 correction and its meaningful changed-seam validation. Preserve the valid physical Task5 evidence and the explicit residual limits; do not relabel the full unit accepted until that finite-handler ownership defect is resolved.


## Root disposition at original review checkpoint

Root consumed the complete 191-line sole reviewer report and independently checked I1 against the current synchronous dispatch, pending acceptance, query, signing, Serve borrower/slot, shutdown and binding spec. I1 is **accepted and open**; it is correctable cooperative ownership delay, not an approved OS-limit waiver. One scoped runtime fix is dispatched at BASE a7ebee70ae273e2cf473af5a7c6157665745006c; the original runtime author did not survive the upgrade, so one fresh author substitutes. Pending/completion ownership and finite signers are the named changed seams. No duplicate whole-unit reviewer, new downstream implementation or accepted physical replay is dispatched. Unit acceptance stays pending the fix, meaningful changed-seam validation and one scoped re-review.

Task5 spec and quality are accepted for the exact fixture/evidence scope described here. This does not make the whole unit accepted. Each inherited table above remains part of this checkpoint: resolved rows are limited to their listed source/component/physical attribution; retained rows remain explicit limitations; I1 rows are open; downstream/declined rows remain mandatory migration work where applicable. Root does not turn a retained limitation into evidence or erase a failed/partial run.

| Task5 residual | Root decision and follow-up |
| --- | --- |
| CV1 exact public before-spawn/Start timing | Retain accepted Task2 component attribution; do not claim physically forced public timing. No product timing hook is required for this fixture acceptance. |
| CV2 closure-only rejection cause | Retain both expiry and closed-gate causes; authentic original packet refusal/unchanged census is accepted only at that measured strength. |
| CV3 successful fresh-Boot recovery | Not closed by stale-socket refusal. Mandatory deployment/recovery phase must validate fresh runtime identity and inability to adopt old effect authority. |
| CV4 full combined local-pending claim loss/isolation | Remains unverified in the selected Unknown case; original component isolation evidence stays separately attributed. Never treat zero metadata tokens as local completion. |
| CV5 live-user daemon shutdown | Remains unverified by post-terminal daemon fault. Changed-seam I1 validation must exercise its original owned coordinator and shutdown join, without relabeling old Active as live-daemon teardown. |
| CV6 omitted bridge details | Retain counts and joined-owner facts, with all omitted origins unknown. No all-errors-benign claim. |
| CV7 startup schema/storage ERROR recovery | Retain actual14WARN+1ERROR and subsequent quorum facts separately. Deployment/recovery validation must assess persistent storage health; this checkpoint is not recovery acceptance. |
| CV8 existing-N capacity/performance | Mandatory dedicated later large-existing-sandbox benchmarks and resource budgets. Active=2 sample is diagnostic only; no fleet improvement claim. |
| CV9 Linux native race/kernel/filesystem breadth | Retain Linux CGO0 execution versus host race distinction and tested configuration boundary. No all-platform/native-race claim. |
| CV10 wider supply chain | Retain exact recorded repository/module/tool/ELF/daemon custody attribution; generated wrappers, stdlib and module cache are not independently certified. |
| CV11 universal preemption/power loss/full journal | Retain as stated execution/durability/capacity limits. This does not waive I1; no universal bound or physical full-capacity claim. |

All eight declined items have explicit downstream status: (1) final sync/flush/FUSE handoff, (2) remote settlement, (3) business End/destructive cleanup/safeRelease, (4) production scheduler integration, (5) deployment and production security, (6) operational restore/recovery, (7) existing-N capacity, and (8) Redis removal/whole production migration are **not accepted here and remain required** by the approved master migration. The current next-interface inventory is read-only preparation; future consumers must freshly validate original claimant/pins/evidence and full-prefix zero. Uncertain remote effects must retain pending ownership even after process exit. First generation retains existing object layout.

No new product commit or native run is claimed by this review-record commit. The three corrected physical cases, both earlier failed/partial outcomes, prior warnings, source freezes and cleanup remain those recorded in the combined native verification report. Whole migration and its heartbeat remain active.
