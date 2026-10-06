# Exec effect 单元最终审查与裁决

本单元已通过三个独立任务 gate、完整原生 etcd fault/race、全仓 test/vet/build/diff 和一次独立集成审查。最终产品1af14b2；正式 spec510fca7；最终审查/回归冻结397880b。源代码没有发现需要最终修复波的问题。

本次只完成执行意图前置层，不代表整体 phases1–5、生产切换或 Redis 移除完成。继续可信目标端日志、实际 launcher/物理证据及其后的任务、调度、collector、GC、部署与切换工作。未执行 push/merge/deploy；无关 Sentinel 文档改动保留。

## 独立集成审查完整原文

# Native exec effect prerequisite — final integrated review

Review target: `a00baa0e310bcc1fbc199403485c9b887b348e7d..397880b839d07173fc6711f74eb3ba92af1f200a`, 17 commits, 19 changed files. HEAD was independently checked as `397880b839d07173fc6711f74eb3ba92af1f200a`. Applied the requested `requesting-code-review/code-reviewer.md` criteria. This report is the sole review artifact written; no product, index, HEAD, branch, fixture, credential, or unrelated Sentinel mutation; no subagents and no test reruns.

## Assessment

**PASS / Approved for prerequisite continuation. Critical 0, Important 0, Minor 0.** The complete new unit delta and named unchanged integration paths support the current spec and plan. No confirmed product, test, or current-unit plan/spec defect requires a final fix wave. Continue to the authorized target/effect and runtime integration work with all mandatory later requirements below retained.

**Overall phases 1–5 / global merge or deployment readiness: NOT COMPLETE / NOT APPROVED by this review.** This unit writes and validates metadata and returns a private draft handle. It neither starts nor ends a physical process; production Manager/config still use Redis, and migration, target enforcement, lifecycle, safe GC, bounded orchestration, deployment isolation and real-scale acceptance remain required. Passing this prerequisite is not the final global source/call-graph or phase acceptance gate.

## Strengths

- `internal/storage/state/etcd/exec_effect.go:21` serializes draft mutation with the same capability mutex used by Renew/Cancel. The fixed draft deadline at `:47` never follows a later renewal. Existing outcome/reference diagnostics survive early retry errors (`:23`), while authorization is checked afresh before every returned Prepared (`:102`).
- `internal/storage/state/etcd/exec_effect_validation.go:71` samples the monotonic anchor after the trusted observation and bounds fixed claims by the original remaining deadline, issuer interval, 30-second wire limit, and business expiry. `:108` signs the retained exact issuer digest; copied wire is verified against original private expected context and actual descriptor, including exact fixed time claims (`:123`). Provider signatures alone cannot choose the command context.
- `internal/storage/state/etcd/exec_effect.go:152` saves the exact record body and complete Stage reference before Grant. An uncertain Begin with no private Stage resolves only that attempt (`:60`); an uncertain Commit retains the same Stage, ticket and command. Terminal cleanup is separately reported and cannot overwrite committed/aborted history (`:91`).
- `internal/storage/state/etcd/exec_effect_read.go:53` owns discovery bytes and first revision across the second RPC. The coherent read ties effect, exact receipt and original issuer revision together; no clock/provider/Lease call or capability reconstruction occurs. Record decoding uses local replacement after strict bounded validation, and public results own ticket bytes.
- `internal/storage/state/etcd/stage_response.go:15` validates all point envelopes before interpreting business evidence. Successful Stage replies also require exact operation shape. The native Delete0 exception is confined to a positive outer/outer-minus-one nested revision; actual deletes, puts and ranges remain strict (`:47`). The actual Grant-copy regression now exercises the boundary previously missing from M2.
- The tests use real server CAS before damaging replies, cover retained identities after unknown outcomes, test every typed codec field independently, and expose real fixed RPC/Lease costs. Current new fault hooks are synchronous; no new fault worker can outlive its test. The parent callback test checks that registered callbacks are removed on return.

## Findings

### Critical

None.

### Important

None.

### Minor

None. Existing and newly considered limitations are individually ruled below; these are not unreported findings or claims of global completion.

## Source coverage and integration checks

Read `integration-context.md` in full first, including the literal predecessor certification and all 42 retained boundaries. Read the requested reviewer instructions and current exact unit spec and plan in full. Read every line of `review-a00baa0..397880b.diff`: **3,696 / 3,696 lines, 180,137 bytes, no unread intervals**. Inclusive bounded intervals were `1–180`, `181–560`, `561–1000`, `1001–1440`, `1441–1880`, `1881–2320`, `2321–2770`, `2771–3220`, `3221–3696`. An initial 1–650 tool response was truncated; the subsequent listed intervals recovered its entirety before EOF was claimed. Independently compared the package's complete diff body to `git diff --no-ext-diff --unified=10 BASE HEAD`; they are byte-identical.

All new source, tests and changed documentation are therefore covered. Also read current full `stage.go`, `receipt.go`, `mutation.go`, `stage_attempt.go`, `operation_admission.go`, `operation_lifecycle.go`, `operation_read.go`, `operation_records.go`, `operation_resolve.go`, `command_authority.go`, `publication_authority.go`, `command_issuer.go`, `command_issuer_records.go`, `fence_check.go`, and `controlprotocol/exec_start.go`. Read the affected strict codec helper in `runtime_preparation_records.go` through EOF, the immutable KV predicate in `runtime_dispatch_read.go`, snapshot UTF-8/compact/digest helpers in `domain_records.go`, and request/base-comparison helpers in `client.go`.

These unchanged checks establish: original admission owns its five control fences and immutable guard/token/receipt state; Renew/Cancel share the same mutex and preserve original Lease semantics; the new private draft cannot revive lost authority; generic Stage copy/reservation rules remain applicable; strict new receipt parsing matches the unchanged producer wire and immutable permanent receipt key; unknown arbitration and terminal guard cleanup stay separate; issuer/clock configuration and bounded observation semantics remain trusted dependencies; final fence reads are default-linearizable on both branches.

Spec comparison budgets were checked directly: 10 control + 9 operation envelope + 3 issuer + 1 absent effect = 23 business comparisons; one write plus reserved 14 gives Stage budget 38. Final base 4 + control 10 + operation 9 + issuer 3 = 26 comparisons. Exec's three operation comparisons deliberately differ from Renew/Resolve's four; the consequence and disposition are explicit in #43 below. No accidental reuse of the four-comparison helper or budget understatement was found.

Historical baseline certification is reused exactly as provided: original-base package `b99b823..0e2a67e` had complete same-seat coverage; `0e2a67e..67129ab` had the independently approved scoped fixes. The larger `b99b823..397880b` context package was **not freshly read end-to-end** in this seat. This report makes no new whole-branch EOF claim and does not replace the eventual global review. The predecessor's old coverage limitation #40 was already satisfied; the current new-delta coverage gate is satisfied independently here.

## Verification and cost evidence

Inspected Root's `unit-gates.md`, full nonverbose `unit-repo.log`, inventory summaries, warning-context ledger, and extracted relevant results directly from `unit-native.log`. Independently counted the actual native log: **267 top-level PASS, 2,115 PASS nodes, 0 FAIL, 0 SKIP, no DATA RACE**; Root's context records 1,933 leaves and the actual package result is **126.144s**. The warning ledger sums to **42 expected fault warnings**, with no new ExecEffect test warning contexts; this was not a warning-free full fault run. Root records 38 LeaseNotFound, 2 cancellation and 2 NOSPACE warnings.

The all-repository log reports passing packages, native etcd **108.943s**; nonverbose output does not prove individual full-repository skip counts. Vet/build log files are empty and Root's execution ledger records exit 0; diff-check also records exit 0. This reviewer did not rerun these suites or infer command exit codes merely from empty logs. Task gates and predecessor reviews were context, not substitutes for the independent source review. No unresolved concrete doubt required an additional focused test.

Actual native cost lines confirm warm-registry Prepare at idle 0 and synthetic idle 1000: **7 Txns / 15 points / 3 Puts / 1 Grant / 0 KeepAlive / 1 Revoke**, Stage 38 and post-fence 26. Committed retry: **3 Txns / 9 points, zero Put/Grant/KeepAlive/Revoke**. Typical effect/ticket/operation/descriptor bytes were respectively `2710/1207/810/314` and `2714/1212/809/314`; wire interval was 30s, with the conservative current-time usable portion shorter. Long-context test recorded operation 4096, ticket 4096, effect 7744; the alternate maximal opaque-runtime encoding case recorded effect 8126 under the 16384 limit. Actual Stage request TTL 1s received native TTL 2s and remained supported.

These counts show bounded per-demand work and no new idle-N scan/watch/per-sandbox timer, goroutine or Lease. They do not establish latency/QPS SLOs or real Pod/FUSE scale. Permanent effect+receipt writes grow with exec attempts, registry history with issuer rotations, and retained descriptors consume memory while capabilities/prepared handles remain reachable. No GC, admission budget or unknown-backpressure implementation is inferred from a successful small fixture.

## Individually retained and set-aside behaviors

The following IDs preserve every predecessor disposition and explicitly state current advances. “Mandatory later” is uncompleted authorized work, not a waiver.

1. **Physical Create/Prepare deduplication/recovery — mandatory later.** Metadata intent and Stage arbitration do not demonstrate once-only physical invocation or crash recovery at the target.
2. **Truthful birth/ready observations and issuance — mandatory later.** Root-authenticated data is insufficient to prove actual target gate/mount observations.
3. **Physical mount-once and FUSE retry exclusion — mandatory later.** No physical mount implementation is added by this unit.
4. **Durable effect intent and current original operation/task authority — advanced, remainder mandatory.** This unit supplies permanent exec-start intent and opaque private original-operation draft with current checks. Physical effect consumption and independent task authority are still required.
5. **Target Begin/End, streams/descendants and terminal receipts — mandatory later.** Stage terminal metadata and cleanup do not establish physical terminal state.
6. **CloseData/CloseAll, exclusive barriers, both drains and reopen — mandatory later.** Existing token disappearance and the new intent cannot authorize those transitions.
7. **TTL extension, destroy/task machine and safe owner release — mandatory later.** The original operation deadline is enforced here, but business lifecycle and evidence-gated owner release are not implemented.
8. **Certificate install/activation/rotation/revocation and UUID uniqueness — partially advanced, remainder mandatory.** This unit consumes a retained registry identity and digest; no target installation, possession proof or operational certificate lifecycle is delivered.
9. **Production issuance and root/delegate key custody — mandatory later.** Injected provider/test keys do not establish production isolation or issuance policy.
10. **Trusted PID1/launcher, management UID and IPC/proc/FD/key isolation — mandatory later.** No Linux physical enforcement is provided.
11. **Raw/legacy execution bypass removal and descendant containment — mandatory later.** Production execution call paths remain to be migrated and audited.
12. **Production controlled clock and target-time rechecks — advanced locally, mandatory later at runtime.** New metadata checks consume the controlled clock and original remaining deadline. Production clock infrastructure/target checks remain absent; predecessor CA-M1 was fixed and remains closed.
13. **Absolute atomic UTC expiry at commit/external effect — not promised.** Bounded observations and target rechecks are the chosen contract; etcd cannot atomically transact against external physical time.
14. **Warm pool, persistent slots/free indexes and recovery accounting — mandatory later.** Fixed per-demand cost does not implement those protocols.
15. **Docker/upload/extract/workspace mutation adapters — mandatory later.** Digest binding is not actual adapter enforcement.
16. **Partition scheduler, R+1 watches, shared renewal and bounded queues/caches — mandatory later.** No idle work is added here, but native production orchestration and compaction recovery remain required.
17. **Collector/readiness/autosync and hung FUSE probe slots — mandatory later.** Metadata checks do not provide bounded physical probes.
18. **Safe journal/receipt/snapshot GC, history/churn and unknown backpressure — mandatory later.** New effect and receipt records accumulate; unresolved evidence cannot be deleted merely by age.
19. **Manager/API/SDK/config native-only wiring — mandatory later.** The current production integration is not switched end-to-end.
20. **Old-version drain and Redis/Sentinel removal — mandatory later.** These original user goals remain unfulfilled by this prerequisite and passing legacy packages do not complete them.
21. **Production mTLS/auth/RBAC/prefix/authority bootstrap isolation — mandatory later.** The owned local fixture and application validation are insufficient deployment evidence.
22. **Disaster restore epoch and old writer/credential quarantine — mandatory later.** Current epoch comparisons do not quarantine every external writer or cloned authority.
23. **Real 10k/100k/1M Pod/FUSE, QPS/p99 and recovery SLOs — mandatory later.** The synthetic 0/1000 fixed-cost experiment is only a shape check.
24. **Multi-cell routing/cross-cell semantics — phase 6.** Remains outside the authorized phases 1–5, without claiming single-cell code proves it.
25. **Malicious trusted Clock/TLS signer/callback implementations — outside trusted dependency contract.** Context honoring, immutable lifetime and concurrency safety remain required; no in-process hostile-code isolation is promised.
26. **Arbitrary Byzantine envelopes across every old generic native path — not promised.** New Stage/exec evidence is strict, but this review does not retrofit every historical trusted-client path or loosen untrusted signed-wire checks.
27. **Coordinated authority forgery by an authorized raw etcd writer — outside application capability boundary.** Record/revision checks cannot secure a Byzantine store; production writer isolation remains mandatory.
28. **Early-Fatal join modernization for every old fault harness — deferred maintenance.** New exec hooks are synchronous and new parent callbacks are stopped; this does not certify every older harness.
29. **Prior M2 actual-Grant Stage-copy regression — CLOSED in this unit.** `stage_test.go:439` mutates caller comparison Key/RangeEnd/value-oneof and write key/value inside actual Grant, checks original digest/guard and native committed original bytes, and checks absence at the mutated key. No alias product defect was inferred from the historical test gap.
30. **Current dependency/image vulnerability/provenance certification — not performed.** No new dependency/image is introduced here; no online advisory or supply-chain audit was carried out.
31. **Unrelated Sentinel edits and out-of-range controller checkboxes — excluded/preserved.** No unrelated file contents or credentials were accessed; controller must record its own final process state without staging unrelated work.
32. **Start deadline bounded by original remaining monotonic Lease — producer requirement now implemented; target consumer remains mandatory.** The retained draft deadline, post-observation anchor, caller/parent/lost checks and fresh verification close this metadata consumer prerequisite. Later transport must recheck original fences/time and target authority.
33. **Accepted-only renewal and other command purposes — separate mandatory protocols.** Existing metadata Renew and an exec-start intent cannot stand for target acceptance, renew tickets, task or file authority.
34. **Target payload reconstruction and credential/env/path/default policies — partially advanced, target enforcement mandatory.** The producer freezes and verifies the actual descriptor; the real adapter must resolve final execution options and the target must execute the same bytes under isolation.
35. **Standalone registry Load detecting coherent deletion/recreation without an original revision — intentionally not promised.** Historical Load still cannot infer an absent prior revision; the new private draft does retain and compare original issuer value/revision/Lease=0 after successful registration.
36. **Permanent global issuer-rotation retention and safe GC — mandatory later.** Rotation-based growth is distinct from idle sandbox N, but still needs retention/reference budgets and a typed safe deletion protocol.
37. **Provider concurrency, immutable lifetime and rotation availability — trusted operator contract, consumed here.** Once registration succeeds, signing retains the exact certificate/digest even if the provider's offered certificate rotates. Provider must retain that signer; an unknown registration remains no authorization to change identity or execute. See #47 for the retry boundary inspected here.
38. **Generic Stage writes/deletes to command-issuers — CLOSED predecessor hardening retained.** `mutation.go:103` rejects the exact family and descendants for writes/deletes; comparisons and dedicated typed Register remain usable. This was not silently deferred or weakened by the new producer.
39. **Certificate valid at exact storage commit instant — not an execution-authority promise.** Stored history may outlive validity; this producer authenticates freshly and rechecks after commit before returning Prepared. The future external consumer must check again.
40. **Required source-reading gate — SATISFIED for predecessor and independently for the new delta.** Prior incomplete checkpoints remain historical audit evidence. This seat has no unread new-delta intervals and makes no fresh original-base whole-branch EOF claim.
41. **Legacy creation-claim decoder strictness — retained trusted-storage/corruption-classification boundary.** Prior canonical UUID/duplicate-field/explicit-zero differences remain as certified; new exec/receipt strictness does not claim to repair the old decoder, and no changed integration path was found to promote malformed stored claims into authority.
42. **Birth-publication delegate/root equality — retained existing compatibility and issuer-custody boundary.** New management identity verification does not silently retrofit birth-publication equality checks. Root/delegate operational separation and production key custody remain required.

Newly considered individual boundaries:

43. **Exec three-clause versus Renew four-clause operation envelopes — explicit chosen contract, no new blocker.** `exec_effect.go:127` checks original value/Lease/CreateRevision; `operation_resolve.go:82` additionally compares ModRevision for renewal/resolution. Exec therefore does not detect an in-place same-value rewrite that preserves the original Lease and first revision. Supported Begin/Renew/Cancel do not perform that rewrite; generic Stage Put cannot attach/preserve that Lease, and delete/recreate changes CreateRevision. Creating this difference requires a raw writer that already has excluded authority under #27. This is not a claim that exec has Renew's exact immutable-envelope detection. The documented 38/26 budgets correctly reflect the selected triples; no demonstrated supported-input authorization defect or plan/spec mismatch was found.
44. **Forging/recreating a fully coherent effect+receipt pair outside the typed producers — outside raw-writer boundary, not silently protected.** The loader rejects discovery-to-snapshot changes, rewritten records, missing/mismatched receipts, and original issuer revision replacement. A standalone public reference has no original effect revision. Generic Stage cannot modify the reserved receipt family, so it cannot recreate the complete pair as a supported mutation; coordinated raw forging remains #27. Private producer rechecks its exact original body and issuer fences and never reconstructs a cap from history.
45. **Historical opaque tickets are not freshly cryptographically verified — intentional metadata API.** `LoadExecEffect` validates bounded JSON/digest, linkage and immutable store envelopes, including expired history. Authenticity/current execution checks belong to the private producer and future target; interpreting arbitrary valid stored JSON as live authority would violate this API's documented contract.
46. **Generic Stage mutation of exec business history — allowed trusted mutation surface, not a bypass.** The effect key is business state, so a generic write/delete can invalidate history. It cannot rewrite the protected receipt or issuer family, and body/first-revision/linkage checks reject the damaged entry. Future GC must be a separate safe protocol, not inferred from the existence of generic Delete.
47. **Provider rotation during an unknown registration before an entry is returned — relies on retained provider contract.** `signExecEffect` has no issuer entry to save when Register returns an error; retry calls the provider again. Register documents retrying the same certificate, and #37 requires unresolved identity retention. The current code returns no Prepared or effect authorization in that path; it must not be presented as automatic rotation reconciliation or as an adversarial-provider guarantee. Once an entry exists it is retained even if the next clock/sign call fails.
48. **Stage Revoke reply proof and Begin-failure cleanup context — deliberately unchanged cleanup contract.** `ReleaseStage` still reports metadata cleanup success from the existing Revoke error contract; it does not validate a new terminal proof. Legacy Begin-failure cleanup may use its own bounded background context, explicitly retained by the unit spec. New producer terminal cleanup uses caller+parent+old-deadline context and never adopts a Lease from history. Neither cleanup proves external termination.
49. **Preemptible waiting for the capability mutex — not introduced by this unit.** The spec requires a recheck after waiting, not a new polling lock. The shared mutex serializes Prepare/Renew/Cancel; RPC/provider deadlines and trusted context honoring bound the holder, and the waiter rechecks caller/live state before proceeding. No unbounded idle waiter mechanism or new background worker was added.
50. **Recovery after creator loss or after the original live deadline — metadata-only.** Public Resolve/Load can settle or inspect the original reference but cannot rebuild Prepared or payload. Keeping the frozen one-shot command after unknown/aborted outcomes is intentional; a durable payload queue would need a separate protected store and fresh task authority.
51. **Ticket availability under the conservative time window — intentional cost.** Short remaining leases and slow Observe/sign/Stage work can reject a start, and a 30-second wire interval has roughly 28 seconds nominal future validity before further safety checks. Renew does not widen that frozen interval. No throughput or availability SLA is claimed by the primitive.
52. **Immediate physical delivery from a cached Prepared — not authorized.** The handle has private fields and only Reference is public. Future transport must freshly reauthorize original fences/time/target gate/accepted journal; current Prepared creation is not a reusable public launch permission. A later implementation must preserve this condition.

## Recommendation to controller

Record this zero-finding integrated gate and the new boundary IDs in permanent unit evidence; no extra broad rereview or speculative test wave is justified for this frozen delta. Continue the remaining authorized implementation and retain the final global review/real-scale acceptance gate. Do not mark phases 1–5, production migration, physical lifecycle, or safe history management complete on the strength of this prerequisite.

## 本单元裁决（完整，按发生顺序，含判断错误的代价）

1. Ruling: continue these prerequisite artifacts under the already approved overall architecture and repeated implementation/continue instructions, without another artifact permission handoff — work stays reversible in the feature branch and avoids external effects — cost if wrong reversible interface/provider/record refinement and downstream rework.

2. Ruling: strengthen shared Stage evidence only at its actual new effect producer boundary and preserve native returnedTTL minimum behavior — avoid duplicate arbitration or incorrectly rejecting legitimate server Grant, original operation deadline independently limits exec — cost if wrong compatibility/refactoring work; no execution introduced by metadata hardening.

3. Ruling: permanent16KiB exec record stores ticket/digests/originalOperation/attempt only, not stdin/argv/env/full descriptor — bounded metadata/secret exposure/idleN budget, historical creatorcrash cannot restart userexec — cost if wrong later encrypted offboardpayload/task queue design for durable queued execution.

4. Ruling: retain demand RegisterExecIssuer fixedRPCcost and omit cache this first original-live producer — scope correctness before measured hotpath optimization, no idleN refresh — cost if wrong added perexec latency/QPS cost until later measured fixedglobalmetadata cache refinement.

5. Ruling: this merged unit finalreview reuses permanently certified frozen baseline evidence and fully reviews all newdelta+affected integration, with fullbranch package supplied for context; don't re-read all historic documents or falsely claim fresh fullpackageEOF — baseline source already completely reviewed and scoped fixes independentlyclosed; taskgates remain — cost if wrong missed baseline issue needs supplementary review/rework; no exemption for newsource/security/crosslayer effects, global productioncallgraph review remains required atoverallfinish.

6. Ruling: allow a Stage-only DeleteRange reply-header exception when Deleted==0, PrevKvs empty, matching-or-zero cluster, positive nested revision equal to outer or outer-1; all Put/Delete1/Range and outer headers remain strict — actual owned etcd3.6.15 Delete0 revision457/outer458 and official storeTxnWrite.DeleteRange returns beginRev before any changes, while final receipt Put advances outer revision by1; rejecting every nested!=outer violates explicit legitimate Delete0 contract — cost if wrong a narrowly permissive no-op nested envelope needs refinement, no generalized header weakening or physical authority; native leading-noop/after-Put and malformed old/future/zero/foreign tests required.

7. Ruling: pin the first discovery effect exact value and CreateRevision through the second coherent5point Load snapshot; disappearance/change/samebody recreation returns corruption, no extraRPC — immutable effect and originalIssuerRevision cannot be silently replaced between discovery and receipt/issuer verification — cost if wrong concurrent legitimate replacement would fail historical reads and require contract refinement; no claimed detection of coordinated raw forgery before the first snapshot.

8. Ruling: after known Committed/Aborted, synchronously attempt ReleaseStage only for the originally retained Stage under the existing caller+parent+old deadline context; successful cleanup is remembered and errors remain separate GuardCleanupError; producer-level Unknown retains originalStage/reference without terminal cleanup — retaining every successful StageLease for30s creates roughly lambda_exec×30 transient Lease workset at highQPS, while normal completed prepare no longer needs its metadata guard — cost if wrong one extra normal RevokeRPC/latency and cleanup may fail on expired/canceled context (originalTTL fallback), requiring cost/refinement; no original operation Lease revoke/adoption/background worker/physicalterminal proof. Existing Begin-error originalGrant cleanup remains the sharedStage exception and does not furnish a Stage capability.

9. Ruling: boundary43: retain exec Value/Lease/CreateRevision triples as the explicit38/26 contract, distinct from Renew ModRevision checks — supported typed admission/lifecycle cannot same-value rewrite preserving original Lease/CRev; raw privileged writer is separately excluded — cost if wrong future supported writers may require ModRevision and revised budgets/consumer tests.

10. Ruling: boundary44: retain historical discovery/coherent-pair boundary without claiming raw store Byzantine protection — loader pins discovered body/CRev and exact receipt/original issuer; genericStage cannot rewrite reserved receipt — cost if wrong stronger audit/revision provenance and writer isolation may need refinement.

11. Ruling: boundary45: keep opaque historical ticket loading structural and deadline-independent — Load never produces capability; fresh crypto/time belongs to original producer/target — cost if wrong a future caller confusing history with live authority requires API/consumer rework.

12. Ruling: boundary46: allow genericStage to invalidate exec business history but never infer safety/terminal from its deletion — protected receipt/issuer and immutable linkage make damage fail closed; typed safeGC remains required — cost if wrong future supported business mutation or GC protocol may need family reservation and compatibility changes.

13. Ruling: boundary47: retain trusted provider same-certificate retry contract before uncertain registration returns a pinned entry — unknown passive registry write produces neither effect authorization nor Prepared; returned original entry is thereafter retained — cost if wrong orphan certificate growth/rotation availability may require explicit provider pinning and registration retry state.

14. Ruling: boundary48: retain existing ReleaseStage error-based metadata cleanup and bounded Begin-failure exception — no external terminal claim or Lease adoption is introduced, new terminal cleanup uses original bounded context — cost if wrong cleanup proof/accounting may need stronger response validation; original guard can remain untilTTL.

15. Ruling: boundary49: retain original mutex serialization with post-wait live/caller checks — same lock coordinates Renew/Cancel; holders are bounded by trusted dependency context contract, no idle waiter worker added — cost if wrong contention latency/queue admission or cancelable-lock refinement may be needed.

16. Ruling: boundary50: keep creator-loss/deadline recovery metadata-only without reminting execution — opaque original cap and payload are intentionally unrecoverable from history; unknown cannot silently replay — cost if wrong durable queued execution needs protected offboard payload and fresh task authority.

17. Ruling: boundary51: retain conservative fixed ticket window and short-remaining refusal — original monotonic deadline and issuer/business limits require safety margin; Renew cannot extend already signed start — cost if wrong latency/start availability may require measured pipeline optimization or a separately authenticated accepted-only protocol.

18. Ruling: boundary52: require cachedPrepared fresh original fences/time and target acceptance checks at delivery — private prepared handle has diagnostic Reference only and is not a transferable launch permission — cost if wrong new transport/target must implement these checks; missed integration requires rework and cannot ship.
