# Authenticated target execution — whole-unit final review

Reviewed original range **4c5c356..30269f76951de716688a13c82128b5a3aa5834d0**. Requirements: **PASS for this new unit**. Findings: **Critical 0, Important 0, Minor 2**. The two Minors concern test maintenance. No new blocking implementation or integration defect was established.

## Strengths

- Authority remains attached to the original objects. Fresh PID1 birth and activation, exact management identity, original backend trust, original operation/effect fences, signed descriptor and durable acceptance are separate checks. Neither a certificate, journal record, diagnostic query nor local drain can manufacture a new Prepared capability or business End.
- Delivery preserves the important ordering across all four tasks: fresh original authorization; actual pinned mutual TLS; another original authorization/trust check; one finite request; durable accepted record; ACK; then bounded execution and streaming. ACK releases the original capability mutex before the long completion wait. The destination cannot enlarge backend roots, and the configured transport issuer must match the original selected issuer certificate bytes.
- Renewal uncertainty is handled conservatively. One exact pending signed deadline is retained before bytes are sent. An old accepted receipt cannot clear it; only a fresh accepted response from the same live owner, after durable renewal and sole-monitor acknowledgment, can confirm the pending deadline. History cannot restart, revive or reconcile execution authority.
- The monitor boundary is physical rather than modeled as an ordinary unconfined child. The production path prepares management credentials, confines the monitor, seals descriptors, applies explicit signed user credentials/options, checks the original absolute deadline immediately before Start, and uses one user wait owner with exact root status and descendant drainage. IPC time does not create another authority interval.
- Resource ownership is explicit. Output queues, input sizes, connections, active operations and timers are bounded. The delivery reader finishes even without Wait; arbitrary caller output writers remain in the caller. The shutdown coordinator now seals registration, cancels/closes transport and execution, joins borrowers, and only then destroys the journal and signer. Concurrent Close callers share completion and errors; failed execution join retains resources.
- The tests include actual Linux execution, real protected files, real TLS, original public metadata provisioning and meaningful behavioral REDs. Historical setup failures, native failures, warnings, skips and source changes remain attributed instead of being folded into a clean final total.

## Issues

### Critical — none

### Important — none

The prior Task 3 late-preparation deadline and mutable-record race findings are addressed in the final code. The prior Task 4 destroy-before-handler-join finding is also addressed. I checked their integration into the complete unit rather than accepting the scoped reviewers' verdicts alone.

### Minor

**M1 / P3 — Bound the test's Accept synchronization waits.** [authority_clock_test.go:474](/Users/dysodeng/project/go/cloud/sandbox/internal/runtime/controlprotocol/authority_clock_test.go:474), also line 481. Both bare `<-listener.accepted` receives can wait until the package timeout if ServeSignedClock exits early or stops accepting. The test's deferred cancellation/join cannot run while blocked there, and the failure loses its useful local diagnostic. Use a bounded select that reports premature server completion or a local timeout, while preserving the existing cleanup owner. This is a test failure-diagnosis/latency issue, not evidence that the production connection deadline is broken.

**M2 / P3 — Format the frame regression test.** [stream_test.go:12](/Users/dysodeng/project/go/cloud/sandbox/internal/runtime/controlrunner/stream_test.go:12). The compressed, nongofmt function puts multiple setup/assertion branches on single lines, making this security-relevant length/malformed-input regression harder to inspect and future failures harder to locate. Apply gofmt to this file; no behavioral redesign or new test is needed.

**Deferred warning item disposition — no additional source finding.** I inspected the retained 004/005/006 summaries: 42 structured etcd warnings are the two same-port warnings and one simple-token warning in each of 14 deliberately isolated single-member projects. They do not demonstrate an authenticated target defect or production security certification. Keep them visible and retain the production etcd mTLS/RBAC obligation. The single `docker stop --time 10` deprecation is in Root's ordinary-stop evidence, successfully exited 0; updating future orchestration to `--timeout` is optional maintenance. Neither warning class should be suppressed or represented as warning-free verification.

## Plan alignment and integration assessment

The final implementation meets the amended four-task unit contract. R19's passive idle accept path does not add an idle refresh actor. R23's exact pending-renewal reconciliation is implemented by the live owner, not journal lookup. R24/R28/R30 preserve production metadata validation while providing a named single-member fixture and independently checked executable custody. R35's underlying optional half-close is invoked only after the finite TLS request and TLS CloseWrite, retaining response reads. R37's tested lifetime coordinator is actually consumed by Serve/Close.

The activation/bootstrap path stays bounded and closed until installation; fixed protected configuration does not permit an arbitrary user-selected authority or payload. The completed request EOF is required before acceptance. The journal's one-use live registration is not reconstructed by reopen, retry or history. The monotonic monitor deadline, post-preparation start check, root readiness identity and final wait/drain attribution compose with accepted/renew/terminal transitions without promoting diagnostics into authority.

No harmful plan deviation was found. The exact full-Serve monitor-kill and paused-history interleavings remain the amended evidence limits described below; they are not claimed as tested equivalents. This review accepts the new library/binary unit for continued integration, not the overall native-etcd migration or deployment.

Named outside-diff checks were limited to concrete changed-call-site risks:

- `internal/storage/state/etcd/exec_effect.go:110–194`, `exec_effect_validation.go:28–151`, `operation_admission.go:264–275`, `operation_lifecycle.go:16–94`, and `client.go:310–317`: original control/operation/issuer comparisons, exact stored effect, controlled-time verification after RPC, original parent/lost/deadline checks and original-Lease renewal. The start envelope's value/Lease/CreateRevision triple remains the certified contract; Renew additionally checks ModRevision. No changed supported writer bypass was found.
- `internal/runtime/controlprotocol/management_runtime.go:1–103` and `management_issuer.go:1–140`: exact runtime context, pinned TrustBinding, role-specific signature domains, fresh intervals, and delegate separation from every pinned root. The new destination verification uses these original checks rather than its caller's enlarged root set.
- `internal/runtime/controltarget/journal_files_unix.go:1–410`: pinned nofollow descriptors, exact ownership/modes/single links, exclusive lock, bounded reads, original write/fsync/rename/directory-fsync sequence and closed reopening. The new live state is layered on that persistence boundary; uncertain bytes remain and no automatic recovery authorizes execution.

The permanent predecessor final-review/verification documents for monitor confinement/drain, kernel boundary, passive journal, exec effect and command authority were consulted for these named contracts, actual earlier gates and individual limits. They remain certified baseline evidence, not a claim that I reread their entire old implementation or replayed their suites. Historical closed findings remain closed: typed-nil clock validation, generic Stage issuer-family reservation, actual-Grant copying regression and predecessor script completion/failure handling.

## Source coverage and evidence

I read the entire supplied **15,083-line, 649,286-byte packaged diff through EOF**, including its 66-commit history, 119 changed files, specification/plan, all product/test/script changes and cross-task seams. A clipped documentation interval was recovered in bounded reads before claiming complete coverage. I also read all four complete implementer reports, all task reviews and scoped fix reviews, progress/rulings, and both current CannotVerify-disposition files. The original whole-unit range, not a final-commit or Task-4-only range, is the basis of this verdict.

This is the R36 inherited review seat. My older kernel authorship precedes 4c5c356; I authored none of the reviewed range. I used no nested reviewer, test/suite execution, Docker operation or Git mutation. Only this report was written. Two initial navigation searches guessed nonexistent paths; they were corrected to `controltarget` and `controlprotocol/authority_clock_test.go`, with no source or evidence changes. Large predecessor-document displays were limited to the relevant retained sections; no complete old-corpus EOF claim is made.

Retained evidence consumed includes the task reports and Root's Task 2/Task 3 proof indexes, all three Task 4 candidate summaries, final repository audit and the added focused fix-evidence index. I read the actual stored shutdown behavioral RED, final five-test race output and complete repository test output. I independently hashed **all 186 named source files in each of the five final test/vet/build/freeze/diff records** against current files: zero mismatches. The shutdown race record's only current mismatch is the later native-selector script; production coordinator and final tests match. Current HEAD is exactly 30269f76951de716688a13c82128b5a3aa5834d0, a documentation-only successor to the recorded final source gate. Root custody/source/native-result audits are retained evidence, not assertions that this reviewer personally launched those fixtures or read every old raw event.

- **Task 1:** actual host signed-clock/TLS and race checks plus Linux cross-compilation are recorded. Missing API/fixture compile errors and the initial hanging clock-test setup remain separate from genuine behavioral REDs. Those checks do not establish production time-source honesty or native Linux race coverage.
- **Task 2:** final real protected-journal native gate records **94 PASS, 20 top-level, 0 FAIL, 0 SKIP**, with 56 frozen sources and binary `d31a7235f2971906fa7eceb9f3b6f4424980a3a7feac8e071031dd03ec8e5947`. Fault matrices cover actual filesystem ordering/uncertainty. The first capability-name fixture-check failure was pre-execution; original outcomes and cleanup remain retained. Protocol-only earlier race source differences are explicitly scoped, not claimed as final journal race verification.
- **Task 3:** the current fix gate records **12 PASS, 0 FAIL, 0 SKIP**, 123 frozen sources, actual production monitor/PTY/binary stdin/renewal expiry, and all exact cleanup. The late-start sentinel behavioral RED and mutable-record Darwin race RED are distinct from compile/setup failures. Earlier actual post-spawn monitor loss, stopped monitor and output backpressure exit-70 cases remain separately attributed; an unjoined stopped monitor was not called joined.
- **Task 4:** candidate 004 records **58 PASS / 11 projects**, candidate 005 **2 PASS / 2 projects**, and candidate 006 **1 PASS / 1 project**: coherent total **61 PASS / 14 projects / 0 FAIL / 0 SKIP**, excluding initializer tests and earlier candidates. Their source inventories are respectively 686/688/691. Root records 761/141/71 successful raw commands, 110/20/10 successful exact removals, and 22/4/2 empty own inventories. Candidate 004's stream-loss and start-failure targets actually exit 70; ordinary targets exit 0.
- Candidate 001's toolchain failure, 002's initial read-only-root extraction rejection and later historical happy case, and 003's real mutation-suite failure are preserved. In 003, a Put did not restore the immutable runtime-index revision; six fresh isolated index/issuer mutation projects correctly fixed the fixture design without weakening validation. Earlier partial passes are not added to the coherent total.
- Candidate 005 proves the real immutable bridge plus original Prepared path and direct-Unix regression after the half-close change. The observed direct ACK was about 9.69 ms while completion took about 527 ms; this demonstrates the intended ordering for that fixture, not a latency SLO.
- Candidate 006 is ordinary external SIGTERM/stop of a real active Serve PID1: target/backend/initializer exit 0, backend reports unknown EOF and joined local delivery work; no fabricated terminal receipt. The approximately 67.9 ms stop is one observation, not isolation/fleet timing. It complements the host real-resource history/signing barrier and active-stream coordinator regressions.
- The focused shutdown index retains 19 commands: 15 exit 0 and four historical nonzero records, of which three are fixture setup failures and one is the actual destroy-before-join behavioral RED. The final race output has five passing tests, including resource retention on unjoined execution; the earlier four-test GREEN is not falsely presented as covering that later test.
- Final retained `go test ./...`, `go vet ./...`, `go build ./...`, freeze verification and diff check all exit 0. These are Darwin arm64/Go 1.25.6 host checks, with cached/platform-limited tests; Linux native gates are CGO0 arm64. No fresh per-test full-repository skip census or Linux race claim follows from them.

The finite PID1 measurements keep observation cost: candidate 005 direct bootstrap/idle/active/completed RSS is 6696/9372/9568/11736 kB, FD counts 9/12/16/12, with a finite diagnostic request at observation time. Candidate 006's active snapshot reports 9528 kB, eight threads and 16 FDs. Backend bridge/relay and metadata costs belong to their respective fixture cgroups. These measurements do not establish a stable memory ceiling, unchanged production-image overhead, minimum user quota or large-N capacity.

## Recommendations

Address M1 and M2 as one small test-maintenance slice with focused verification; neither requires repeating the unchanged native suites. Preserve all failed runs, exact source/evidence associations, warning disclosures, R27/R37 distinctions and individual boundaries when producing permanent artifacts. Continue the approved migration work after this unit gate; do not advertise production native-only execution, remote settlement or Redis removal from this intermediate result.

## Declined to judge and individually retained boundaries

Each line below identifies a considered behavior and the reason it is not certified by this verdict. Mandatory downstream items remain requirements, not waived findings. Repeated historical entries are consolidated by behavior; the permanent predecessor lists retain their original identities and rulings.

1. **Production Manager/API/SDK/backend selection:** this unit deliberately does not switch existing Redis-backed production wiring; end-to-end native-only integration remains mandatory.
2. **Legacy/raw runtime bypass removal:** Docker/Kubernetes and other production invocation paths require a later complete call-graph conversion; the fixed new serve/bridge path does not certify them.
3. **Runtime image/configuration deployment:** native custody proves the frozen fixture ELFs, not production image installation, filesystem provisioning or rollout compatibility.
4. **Production independent birth/publication attestation:** fresh target birth and a real fixture mapping are implemented; operational attesters must still verify actual runtime UID, boot, gate and security contract independently.
5. **Production Root/delegate/clock key custody:** separate cryptographic roles are enforced where specified, but test initialization does not establish operational secret separation or issuance policy.
6. **Controlled-time truthfulness:** signatures authenticate the clock source, not the truth of its UTC/uncertainty; production synchronization and honest uncertainty remain trusted prerequisites.
7. **Context-safe foreign providers:** Clock, signer, Dial and custom connection methods must honor their contracts; arbitrary hostile or indefinitely blocking in-process implementations cannot generally be preempted.
8. **Caller output writers/readers:** local reader ownership is bounded, but an arbitrary caller io.Writer or foreign blocking IO implementation must return; Close cannot join or interrupt unrelated caller work.
9. **Production activation UUID uniqueness/rotation/revocation:** current fresh installation pins one issuer/activation and closes on loss; a complete operational rotation/revocation/nonreuse lifecycle is not delivered.
10. **Physical Create/Prepare deduplication and crash recovery:** the fixture uses original public metadata APIs, but this execution unit does not implement every production creation/recovery protocol.
11. **Physical mount-once/FUSE retry exclusion:** plain-mode execution cannot prove actual FUSE mount consumption or exclusion of physical remount retries.
12. **FUSE/network/mounter topology:** different production mount/network layouts and transient SETPCAP deployment with mounters require their own physical acceptance; netnone plain-mode evidence is narrower.
13. **Remote runtimeGone/dual drain:** local ECHILD, unknown and PID1 namespace exit do not establish all remote runtime identity or both-drain observations.
14. **CloseData/CloseAll, exclusive barriers and legal reopen:** current local closure does not implement the complete production barrier/reopen protocol.
15. **Remote-write quiescence/final synchronization:** descendant termination and local receipts cannot establish that external writes have settled.
16. **Business End/owner release:** terminal/query/Close/cancellation/isolation are not authority to settle or release ownership; the evidence-gated remote lifecycle remains mandatory.
17. **TTL/destroy/task authority:** accepted execution renewal is implemented, but full task-machine TTL extension, destroy sequencing and other command purposes remain separate required protocols.
18. **Warm pool/slots/indexes/recovery accounting:** fixed execution costs do not implement global pool/quota coordination.
19. **Upload/extract/workspace mutation adapters:** exact execution descriptor enforcement does not certify these other effect boundaries.
20. **Partition scheduler/watch/shared-renewal queues:** no idle actor is added here, but the required production scheduler, compaction recovery and bounded shared machinery remain downstream.
21. **Collector/readiness/autosync and hung FUSE probes:** finite PID1 inspection does not implement or certify those physical probe services.
22. **Journal/receipt/snapshot and issuer-registry GC:** local capacity refusal is implemented; safe global retention, rotation growth, unknown backpressure and deletion protocols remain required. Age alone cannot authorize deletion.
23. **Disaster restore/quarantine:** exact restore binding rejects mismatches; isolating old processes, external writers and cloned credentials is a separate operational obligation.
24. **Old-version drain and complete Redis/Sentinel removal:** still required by the migration and not delivered or approved by this unit.
25. **Production etcd mTLS/RBAC/prefix/bootstrap isolation:** the named insecure isolated fixture and its 42 warnings are not deployment-security evidence.
26. **Three-member failover/partition behavior:** new native delivery gates use one actual member; they do not replay or extend the predecessor quorum/failover certification.
27. **Large-N capacity/QPS/p99/recovery SLOs:** finite 64 MiB fixtures and snapshots cannot establish 10k/100k/1M Pod/FUSE behavior or a Redis improvement claim.
28. **Original production-image overhead/minimum quota:** observed fixture RSS/FD/thread/disk accounting includes observer/runtime costs and does not certify a stable ceiling or preserved application headroom for every image/language.
29. **Multi-cell semantics:** phase 6 remains outside this phases-1–5 unit.
30. **Native amd64 execution:** cross-build/vet is available; actual current kernel/process execution is arm64 only.
31. **Native Linux race:** retained race runs are Darwin host tests; CGO0 Linux native execution is a separate kind of evidence.
32. **Foreign/manual/CGO threads and broader platform matrix:** the supported contract is Go-owned CGO0 threads on the stated Linux architectures/toolchain; other Go/kernel/LSM/ABI combinations are not certified.
33. **Exact full-Serve post-spawn monitor SIGKILL:** R27 retains real Task 3 monitor-loss evidence and new full-Serve stream-loss/start-failure coverage separately; that precise wrapper-level kill remains unforced and requires later operator/runtime tooling.
34. **Exact native full-Serve history-Lookup/signing pause:** R37 supplies a real-resource private coordinator regression, actual source consumption and native ordinary shutdown, not a forced full-Serve paused-history interleaving.
35. **Exact native closure before readiness publication:** controlled model tests and naturally short executions support ordering; every precise native scheduling point has not been forced.
36. **OS ForkExec/kernel IO/STW/scheduler/daemon stalls:** the final pre-Start check and bounded owned cleanup cannot make an uninterruptible syscall or process-wide scheduling stall preemptible, nor guarantee an atomic UTC instant across external effects.
37. **Kernel thread-disappearance/retry exhaustion and later-thread setter divergence:** predecessor bounded inspection/source/fatal-path evidence is retained; every precise native interleaving is not forced anew.
38. **Missing/strict seccomp facilities, later TSYNC and filter identity:** supported installation is checked; filter counts are diagnostic, and every old kernel/strict-mode/future-filter combination is not dynamically certified.
39. **Kernel resource/ABI edge matrix:** 4096-process/thread/filter exhaustion, real mountinfo ceiling, x32/foreign ABI, cgroup-v1 breadth and every LSM/permission/IO failure are not established by controlled parser/state tests.
40. **Numeric PID reuse/non-SIGCHLD adoption edge:** inherited stale-FD ESRCH and clone/adoption proofs retain their exact scope; numeric reuse and an omitted-WALL post-adoption native failure were not forced.
41. **Partial kernel mutation rollback or securebits-divergence survival:** intentionally not promised; no boundary is returned, partial changes may remain, and differing all-thread results can terminate the process.
42. **Perpetual validity of a cached kernel snapshot:** it remains a copied diagnostic; actual current validation is required, and malicious trusted code changing credentials is outside the isolation model.
43. **Test helper fsID/signal-mask restore failure:** inherited successful restoration/source behavior does not prove forced restore denial or authorize raw production per-thread privilege manipulation.
44. **Trusted-root concurrency/topology mutation:** protected ancestor/ACL/mount policy, root-only writers and trusted FD-opening behavior remain deployment contracts; hostile same-management-UID/root code is not isolated in-process.
45. **Power loss/hardware fsync truth:** actual journal syscall/fault ordering and process outcomes do not prove firmware persistence or crashes at every machine instruction.
46. **Automatic recovery/history adoption:** missing manifests, malformed temps, changed binding/boot and creator loss remain closed/unknown; no repair, replay or replacement capability is supplied.
47. **Always-available close at full disk/file limits:** closure itself can require real filesystem capacity; no unknown evidence is deleted to manufacture success.
48. **Physical absence as remote authority:** fixed-layout ENOENT plus current fixture diagnostics establishes only the specified local no-acceptance observation; absence alone is neither authenticated remote absence nor Start/End authority.
49. **Unforced server/crypto operational branches:** exact default-16 saturation, entropy/listener-deadline failures, actual resumed-handshake negotiation and every cancellation interleaving are not collectively certified; configured prohibitions and reviewed bounded paths remain the implemented contract.
50. **Current CHOWN-native ownership variants:** the fixed four-capability fixture lacks CHOWN; inherited actual journal ownership evidence remains distinct from current protected-file gates, with no extra capability added merely to broaden a claim.
51. **Malicious authorized raw etcd writers/coherent forged histories:** application capabilities and retained revisions do not make a Byzantine store safe; production writer isolation remains mandatory.
52. **Standalone Load and historical tickets:** diagnostic loaders cannot detect every coherent pre-discovery recreation without an original revision, and intentionally do not grant fresh execution authority from expired opaque history.
53. **Legacy decoder/publication certificate compatibility:** unchanged creation-claim strictness and birth-publication delegate/root equality retain their certified compatibility/key-policy boundary; the new management-role verifier does not silently retrofit every old certificate family.
54. **Start triples versus renewal quadruples:** same-value in-place operation rewrites preserving original Lease/CreateRevision require excluded raw-writer authority; no claim is made that start detects exactly the same rewrite set as Renew.
55. **Generic Stage business-history mutation and metadata cleanup:** supported mutation can invalidate effect history; retained checks reject it. Stage Revoke/Begin cleanup does not establish external termination or safe GC.
56. **Provider rotation during uncertain registration:** immutable exact-signer retention and retrying unresolved identity remain trusted provider obligations, not automatic rotation reconciliation.
57. **Capability/journal mutex preemption and conservative windows:** waiters recheck state after acquisition; blocked holders/syscalls cannot universally be interrupted. Slow observations/signing can consume the fixed window and reject otherwise desirable work.
58. **Universal fixture cleanup/daemon responsiveness:** exact successful removals and empty inventories certify these runs, not success after every future Docker/OS failure or uninterruptible attach process.
59. **Old harness modernization and historical missing transcripts:** prior early-Fatal helper maintenance and specifically unavailable predecessor raw host/static transcripts retain their permanent recorded limits; newer evidence does not retroactively create them.
60. **Dependency/image vulnerability or supply-chain audit:** no such certification was requested or performed; this unit introduces no new dependency or image.
61. **Historical whole-branch replay and evidence custody:** I reviewed the full new diff and named unchanged risks, consumed retained Root audits and performed the stated hash comparisons; I did not repeat old native suites, every historical command or the entire prior branch review.
62. **Controller final bookkeeping/archive and unrelated edits:** permanent retention, individual dispositions and final checkbox completion belong to Root after this report; unrelated Sentinel/configuration/ignored files were excluded and untouched.

Previously deferred birth generation, authenticated physical delivery, live accepted-only renewal, exact payload/FD handling, sole local root wait/drain, bounded stream ownership and current shutdown ownership are now implemented and supported within this unit's stated topology. They are not left as wholesale unknowns merely because earlier prerequisite reports called them future work. Their production deployment and remote-settlement portions remain separately listed above.

## Assessment

**Ready to merge? Yes — for this authenticated target execution unit into the continuing feature branch. C0 / I0 / M2.** The complete reviewed change satisfies the amended unit contract, and source-associated host/native evidence supports its essential physical and authority boundaries. M1/M2 are nonblocking test-maintenance fixes; this verdict does not approve production deployment or declare the complete native-etcd migration finished.


# Sole final fix scoped re-review

- **M1 — Bound the test's Accept synchronization waits — ADDRESSED.** [authority_clock_test.go:470](/Users/dysodeng/project/go/cloud/sandbox/internal/runtime/controlprotocol/authority_clock_test.go:470) introduces an owned one-second timer and a select over acceptance, premature server completion and timeout. Both original waits call it at lines 487 and 494, with first/second diagnostics. The timer is stopped on return. Closing `done` at line 459 allows the existing deferred cancellation/join at lines 462–469 to observe completion even when the diagnostic receive already consumed the buffered server error. Existing connection-close defers and connection-limit/request-deadline assertions remain intact; no worker or production hook was added.
- **M2 — Format the frame regression test — ADDRESSED.** [stream_test.go:12](/Users/dysodeng/project/go/cloud/sandbox/internal/runtime/controlrunner/stream_test.go:12) is formatted throughout. The complete diff preserves all binary bytes, length boundaries, malformed inputs, EOF/write checks and assertion behavior; only formatting changes this file.

### Checks and evidence

- Read the complete supplied brief, fix report, Root audit and one-commit packaged diff `30269f76951de716688a13c82128b5a3aa5834d0..9f2ed3f5c9dac2d3c42e85c62a0f9c85b84f57db` through EOF: exactly two test files, 47 insertions and 15 deletions. Checked current line references only within the changed tests. No second whole-unit review, Git command, suite/native/Docker replay, nested reviewer or tracked mutation was performed.
- Parsed all three raw maintenance JSON records through EOF and read their full command, environment, timing, stdout/stderr and exit fields. `go test ./internal/runtime/controlprotocol ./internal/runtime/controlrunner -run '^(TestSignedClockConnectionLimitAndDeadline|TestMonitorFrames)$' -count=1 -v` records exit 0: the named clock test PASS in 5.01s and frame test PASS in 0.00s; stderr empty. Scoped package vet and two-file diff check both record exit 0 with empty stdout/stderr. Environment is Darwin arm64, Go 1.25.6, CGO1; these are host checks, not Linux race or native evidence.
- Independently hashed each raw record and all 186 recorded source entries against current files. All three raw hashes match `root-final-fix-evidence-audit.json`; each source inventory has zero mismatches. This corroborates the covering results without rerunning them.
- Parsed the production-inheritance record: it attributes unchanged 315 baseline non-test Go files and exact baseline-gofmt equality for the frame test. The complete fix diff independently contains no production/script change. Candidate006 remains its original consumed 691-source/ELF freeze; its current all-source match is explicitly false because these two tests changed. No new physical execution or retroactive current-binary claim is made, and the prior whole-unit boundaries remain unchanged.

### New Breakage in the Fix Diff

- **None — C0 / I0 / M0.** The new select removes the two unbounded synchronization waits, preserves cleanup ownership after either error-consumption path, and adds no detached activity; frame-test behavior is preserved.

### Out-of-Scope Observations

- **None.** No unrelated implementation was reviewed or new out-of-scope issue recorded.

### Verdict

- **Fix round: All findings addressed, no new Critical/Important breakage.** M1 ADDRESSED; M2 ADDRESSED; open findings 0; new breakage C0/I0/M0. This is the sole scoped final fix re-review, not renewed whole-unit or deployment approval.
