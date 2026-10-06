# Native issuer registry 最终审查与验证

当前源冻结 `67129ab46f34a8a086c6fd2218a64d6221f51b2c`，从 `feat/workspace-fuse-mount` 的原始基线 `b99b823d69de6baa860eb95d47bd269599b03f42` 开始；工作分支 `codex/etcd-state-management`。

本单元实现 operator DI issuer、严格不可变证书 record 与永久注册 CAS／固定点历史查询。所有空闲 sandbox 均不增加续租、watch 或后台轮询；注册仍有每次需求的固定 RPC 成本，尚不能证明整体性能 SLO。生产 Manager 仍依赖 Redis，整体 phases1–5 未完成。

原始基线→0e2a67e 的包 58 commits、131 files、25124 lines、1348151 bytes，由同一 whole-review seat 完成全部真实覆盖。六次 continuation 只补读未覆盖段，累计行数 8734、11107、15433、19796、24078、25124；最终未读0，C0/I0/M1。没有把旧报告或测试通过当作未读源码的替代。

ONE fix wave 分成两笔验证即提交：d82e276 拒绝 typed-nil channel Clock；67129ab 在 Grant 前禁止 generic Stage 写／删 command-issuers family，保留比较与 near-prefix 和 typed Register。总计5files／108insertions／3deletions。两个实行为 RED→GREEN、targeted race、package vet／gofmt／diff check 均通过。scoped independent review PASS，两项均 ADDRESSED，无新增 findings。

修复后的 controller native fault/race actual exit0：230顶层 PASS、0SKIP、0FAIL，98.792s，43条预期注入告警（38 lease missing、3 cancel、2 NOSPACE），无 DATA RACE。全仓 test actual exit0，native etcd82.873s，其他包通过或使用 cache；go vet ./...、go build ./...、diff check actual通过。专有新建本地 native script cluster 与 manual cluster 独立；所有测试完成后清理，exact project labels containers／volumes／networks 均0。全部历史 env ports 无效，不用于下一单元。

修复前的 controller 失败保留：0e2a67e 首次全仓 test exit1，2个 fault test 在验证 fixture ownership 时拒绝错误 project 名称 sandbox-etcd-state-authority-...；controller 名称不匹配 ^sandbox-etcd-state-test-[0-9]+-[0-9]+$，不是产品 native CAS 失败。纠正环境、重新建 owned cluster、完整重跑 exit0，native76.840s。没有放宽 ownership guard，没有用隔离重复测试掩盖失败。修复前 fresh native226PASS／0SKIP／0FAIL99.647s，与修复后的230PASS分别记录。

唯一无关已跟踪编辑 docs/superpowers/plans/2026-09-17-sentinel-kubernetes-129-compatibility.md 保留且未stage。本机 ignored var/tmp/cce 被全仓 go test 发现为无测试包，其内容／凭证没有读取或修改。无 push／merge／deploy。

边界状态变更：#38 generic Stage reservation 实现、验证、独立 scoped 复核均完成；#40 真实源码覆盖已完成。CA-M1 已修复。M2 actual Grant-boundary copy regression 非阻塞、下一 Stage/effect 单元补齐；没有确认 alias 产品 bug。#41 legacy claim codec 与 #42 birth delegate/root equality 的兼容／信任边界逐项裁决保留，不能声称旧协议都具备新角色结构拒绝，也不能豁免 production key custody。

下面保存完整初始审查及同座追加记录；早期 pending／OPEN 文本是当时快照，最终状态以本摘要及 scoped复核为准。所有1–42条边界仍逐项保留，mandatory later 项仍需实现。

# 初始 reviewer 报告与逐次覆盖记录（历史快照）

Frozen range: `b99b823d69de6baa860eb95d47bd269599b03f42..0e2a67edacfc5c1edf8cee554b053695726ea5ef`. Package: 58 commits, 131 files, 25,124 package lines, 1,348,151 bytes.

**This report does not certify a complete whole-branch review.** The supplied package exceeds this review seat's available context. After bounded reads and explicit truncation recovery, this seat has actually read **7,383 package lines**; **17,741 remain unreviewed**. Exact inclusive covered/pending intervals are in `final-review-coverage.md` and machine-readable `final-review-coverage.json`. The controller was notified early and instructed that remaining coverage must be completed without pretending predecessor review is a substitute. The final fixture tail was read through EOF, but that does not imply intervening gaps were read.

All current issuer production/test files, constructor wiring, their direct structural/crypto helpers, the complete original state-management/exec/issuer specifications, original Operation production chain, generic Stage production chain, dependencies and fixture scripts were read. Earlier plans/reviews, some older production and most older tests still have explicit gaps. The prior complete exec-start review was read as context, including all 34 dispositions, and is not used to certify unread source.

## Strengths

- `internal/storage/state/etcd/command_authority.go:15` provides the exact typed operator interface, retains provider/key-custody separation, accepts metadata-only configuration and rejects typed-nil issuers across all nil-capable kinds. It creates a copied pinned management verifier and makes no provider or clock call during construction. `client.go:155`/`:175` stores only the validated provider/verifier references. The real configured `New` fixture at `command_issuer_test.go:62` closes the prior successful-constructor test gap and asserts both startup call counters stay zero.
- `command_issuer.go:15` bounds context before provider access, copies returned bytes before the clock callback, checks cancellation, freshly verifies role/root/exact binding/time, and constructs the stored identity exclusively from authenticated getters. `management_issuer.go:69` authenticates the real signature against copied pinned roots and rejects delegates equal to any pinned root. No private root or delegate key is installed in Backend, and registry methods never call SignStart.
- `command_issuer.go:55` uses only base identity/restore comparisons plus absent UUID, with one permanent Put or three linearizable point reads. Both CAS outcomes require a final fenced point snapshot. Same canonical body retains the original immutable revision; different valid body conflicts; final absence and sent RPC errors remain unknown. `:69` checks corruption on CAS Else before a later read could hide it. There is no overwrite, delete, Stage, lease, scan or physical effect in these registry methods.
- `command_issuer.go:127` preserves full transaction headers and validates point cardinality, kind, Count/More, key, revisions and nested header compatibility before identity validation. This private reader correctly avoids the existing `readDomain` header-loss limitation without changing its older protocol. Both read branches contain default linearizable points; the prior empty-read-only comparison issue is not reproduced.
- `command_issuer_records.go:30`/`:76` separate strict immutable history from fresh crypto. Required outer fields, canonical UUID/digest/namespace, bounded opaque JSON, digest association, exact key, Lease=0 and equal positive first/modification revisions are checked. Destination replacement is atomic and certificates are copied. Load's public comments at `command_issuer.go:95` accurately state that neither history nor absence confers authority.
- New native tests exercise different valid certificates competing for one UUID, actual committed reply loss, lost final read, delayed complete CAS, corrupt response/record rejection and copied bytes. `command_issuer_fault_test.go:263` releases/cancels/joins the worker before restoring hooks; its child-Fatal regression at `:332` exercises both parent and worker failure. The fixed-cost instrumentation checks fresh 2 Txn/3 points/1 Put, replay 2 Txn/6 points/0 Put and Load 1 Txn/3 points, with zero Grant/KeepAlive/Revoke.
- Read integration code preserves authority distinctions: `exec_start.go:60` requires pinned expected scope, freshly authenticated issuer, actual descriptor digest, exact operation context and bounded start-only interval; `operation_admission.go:99` retains original private capability state and five record fences; `operation_lifecycle.go:16` cannot revive a lost/late capability or regrant its Lease; `operation_resolve.go:14` returns metadata outcome, never a new capability. The new registry entry does not bypass these boundaries.
- Generic Stage still copies all mutable comparison/write data before Grant (`mutation.go:39`/`:126`, `stage.go:77`) and uses durable commit/abort arbitration. The registry's simpler CAS is appropriate for passive certificate metadata; it is not presented as a substitute for the next dangerous effect-intent protocol.

## Findings in covered code

### Critical

None identified in the covered source. This is not a finding count for the unreviewed package remainder.

### Important

None identified in the covered source. Full-package coverage is a pending review gate, not a fabricated product defect.

### Minor — CA-M1 / P3: reject typed-nil channel clocks during configuration

**Location:** `internal/storage/state/etcd/publication_authority.go:37`, reached by new `command_authority.go:32`; regression gap in `command_authority_test.go:34`.

The existing clock nil-kind switch omits `reflect.Chan`, while the new issuer nil-kind switch includes it. Go permits a defined channel type to implement `AuthorityClock`. An interface containing a nil value of that type is nonnil and passes both `execAuthority` and `validateOptions`. Thus the new configured authority can accept a missing channel-backed clock despite the paired valid-clock/typed-nil configuration intent. With an ordinary context-honoring receive/select implementation, the eventual observation waits until its request timeout instead of reporting invalid configuration at construction.

This is a fail-fast configuration inconsistency, not an authentication bypass or a claim that trusted callbacks must be hostile-code safe. Fresh registration still cannot succeed without a valid clock observation. A nil channel receiver can technically implement stateless behavior, but the existing blanket rejection policy already rejects nil functions/maps/slices irrespective of receiver behavior; channels should be treated consistently.

**Focused execution:** one external-only Go overlay supplied a defined `reviewNilChannelClock chan ClockObservation` whose Observe selects on channel or context. `go test -overlay=/var/folders/3x/49xq7yh50m71scj26y3vgdpw0000gn/T/issuer-clock-review-1otouo3r/overlay.json ./internal/storage/state/etcd -run '^TestReviewRejectsNilChannelAuthorityClock$' -count=1` exited **1**, package 1.380s. Both assertions reported that the factory/options accepted the nil channel with nil error. The actual test source, overlay and `result.log` remain in that external directory. No checkout source/Git state changed, no native fixture or suite rerun was used.

**Recommendation:** add `reflect.Chan` to the shared clock nil-kind rejection and add a narrow constructor/configuration regression. Keep validation free of Observe calls. This can be handled in the controller's single final fix wave.

### Previously retained minor item

Prior **M2**, comprehensive generic Stage mutation-copy coverage at the actual Grant boundary, remains a nonblocking test-strengthening item. Prior report anchor: `stage_attempt_test.go:185`. This seat directly read the production deep-copy implementation, but did not newly read that older test file, so the precise remaining test gap is inherited report evidence rather than a fresh exhaustive test judgment. It is not counted as a new alias defect and is not silently dropped.

## Additional integrated consideration and controller ruling

`internal/storage/state/etcd/mutation.go:101` reserves only `meta` and `p/*/{attempts,stages}`. Consequently a trusted holder of Backend can currently use the generic `BeginStage` mutation API to write/delete `command-issuers/<UUID>`, even though Register/Load never do so. New typed registry methods therefore preserve immutability within their own API, not across every generic application mutation entrypoint. Original revision/value/Lease fences remain essential, and no execution capability is exposed now.

This was raised separately from arbitrary raw-etcd-writer attacks. The controller explicitly elected a small hardening in the one final fix wave: reserve the `command-issuers` family against generic Stage **writes/deletes**, keep comparisons legal, retain the dedicated typed Register path, and leave any future safe GC to a dedicated conditional protocol. No product edit was performed by this reviewer. This report does not falsely claim generic Stage already protects that family. I did not classify it as a current authentication bypass; the hardening is a concrete controller scope decision that still needs implementation and verification.

## Verification evidence and limits

Read the completed `controller-final-gates.md` after controller notification, then inspected current final log summaries directly:

- Fresh owned native fault/race: exit 0, **226 top-level PASS / 0 SKIP / 0 FAIL**, **99.647s**, no DATA RACE marker. `/tmp/etcd-command-authority-owned.log` is 563,699 bytes. Controller records 38 expected missing-lease warnings, 3 context-canceled and 2 NOSPACE warnings from injected paths.
- Corrected **full** repository run: exit 0; actual native etcd **76.840s**, remaining package results pass/cache. Inspected `/tmp/etcd-command-authority-repo-corrected.log` (2,306 bytes). Cached packages are not described as freshly rerun.
- Vet/build logs are zero bytes; controller separately records both exit 0 and diff-check pass. Empty output alone is not taken as exit evidence.
- Initial all-repository run remains recorded as exit 1, etcd 80.592s: `TestStageNOSPACEResolverKeepsUnknown` and `TestStageSurvivesFixtureLeaderFailure` rejected the controller's manual project name before global fault work. The exact ownership regex mismatch was corrected in environment only, with a fresh correctly named owned project, and a complete rerun was required. No product change/test weakening or isolated flaky-repeat claim occurred.
- Controller records all old manual, corrected manual and script fixture resources removed, exact labels zero containers/volumes/networks, and all test workers completed first. All fixture ports are historical and were not reused by this reviewer.
- Current Task 1/2 reports and independent reviews were read, with initial output truncation recovered. Their actual behavioral RED, corrected test-fixture mistakes and focused/race successes are retained as report evidence. Task 2's sample 558-byte certificate/817-byte record and fixed point counts do not prove capacity or SLOs.
- The only reviewer execution was the focused external overlay for CA-M1. No test/race/vet/build suite was repeated. No subagents, source edits, commits, index/branch changes, deployment, credentials or fixture mutations occurred. Requested report/coverage artifacts are the only checkout writes.

## Individually retained or declined behaviors

All prior 34 boundaries are retained individually below, from the exact prior list in `docs/superpowers/plans/2026-10-06-exec-start-protocol-final-review.md` under “Individually declined or set-aside behaviors.” Mandatory later means still required by overall phases 1–5, not waived by this intermediate unit. The controller must preserve/rule on each line.

1. **Physical Create/Prepare deduplication/recovery — mandatory later.** Metadata dispatch does not prove physical invocation occurs once or recovers safely.
2. **Truthful birth/ready observations and issuance — mandatory later.** Authentic signatures cannot replace trusted target observations of gate/mount state.
3. **Physical mount-once and FUSE retry exclusion — mandatory later.** Consumed metadata is not proof of one actual mount invocation.
4. **Durable command/effect intent and current operation/task authority — mandatory next.** Registry is a prerequisite; no opaque original-capability effect consumer or physical invocation is implemented by it.
5. **Target Begin/End, stream/descendants and terminal receipts — mandatory later.** Metadata completion/cancellation/Lease loss does not establish physical termination.
6. **CloseData/CloseAll, exclusive barriers and both drains/reopen — mandatory later.** Token disappearance cannot establish target drain or authorize reopening.
7. **TTL extension, destroy/task machine and safe owner release — mandatory later.** Permanent expiry/owner fields do not implement evidence-gated lifecycle transitions.
8. **Certificate install/activation/rotation/revocation and UUID uniqueness — partially advanced, remainder mandatory.** This unit implements a command-issuer immutable registry; it does not install or activate certificates at targets, prove live possession, implement revocation, or deliver the whole runtime certificate lifecycle.
9. **Production issuance and root/delegate key custody — mandatory later.** Operator DI and test signers provide an interface, not operational secret isolation or issuance policy.
10. **Trusted PID1/launcher, management UID, IPC/proc/FD/key isolation — mandatory later.** Pure metadata/crypto cannot establish physical Linux isolation.
11. **All raw/legacy execution bypass removal and descendant containment — mandatory later.** No complete runtime call graph has been migrated by this unit.
12. **Production controlled clock and target-time rechecks — mandatory later.** A trusted clock interface and metadata registration do not install or verify execution-time clock infrastructure; CA-M1 is separately a current configuration finding.
13. **Absolute atomic UTC expiry at commit/external effect — not promised.** Controlled bounded observations and target rechecks are required; etcd cannot atomically transact with external time.
14. **Warm pool, persistent slots/free indexes and recovery accounting — mandatory later.** Fixed registry costs do not implement pool/quota protocols.
15. **Docker and upload/extract/workspace mutation adapters — mandatory later.** Typed target binding is not actual adapter enforcement.
16. **Partition scheduler, R+1 watches, shared renewal and bounded queues/caches — mandatory later.** Primitive fixed cost does not replace old orchestration or prove compaction recovery.
17. **Collector/readiness/autosync and hung FUSE probe slots — mandatory later.** No real bounded physical probe scheduler is delivered here.
18. **Safe journal/receipt/snapshot GC, history/churn and unknown backpressure — mandatory later.** Permanent evidence accumulates; age alone cannot release unresolved state.
19. **Manager/API/SDK/config native-only wiring — mandatory later.** Current production callers have not been switched end to end.
20. **Old-version drain and Redis/Sentinel removal — mandatory later.** These explicit user goals remain unfulfilled; passing legacy packages are not migration completion.
21. **Production mTLS/auth/RBAC/prefix/authority bootstrap isolation — mandatory later.** Client validation and same-host unauthenticated fixture do not establish deployment access isolation.
22. **Disaster restore epoch and old writer/credential quarantine — mandatory later.** Metadata epoch checks do not isolate every external process or cloned authority.
23. **Real 10k/100k/1M Pod/FUSE, QPS/p99 and recovery SLOs — mandatory later.** Small fixtures, synthetic idle rows and wire samples are not real-scale acceptance.
24. **Multi-cell routing/cross-cell semantics — phase 6.** Explicitly outside the current authorized phases 1–5; current single-cell protocol is insufficient evidence.
25. **Malicious trusted Clock/TLS signer/callbacks — outside trusted dependency contract.** Implementations must honor context and remain immutable/concurrency safe; no in-process hostile-code isolation is promised.
26. **Arbitrary Byzantine envelopes throughout old generic native paths — not promised.** Supported native contracts remain trusted; the new registry's strict shapes were reviewed, but that does not retrofit every old path or waive strict untrusted signed-wire checks.
27. **Coordinated authority forgery by an authorized raw etcd writer — outside application capability boundary.** Structural checks cannot make the store Byzantine-proof; operational isolation and future original revision fences remain required.
28. **Early-Fatal join modernization for every old fault harness — deferred maintenance.** New registry workers have bounded join and regression; that does not certify every older harness.
29. **Prior M2 actual-Grant Stage copy regression — retained nonblocking.** Production copies were directly checked; the broader test matrix remains prior report-backed open work.
30. **Current dependency/image vulnerability/provenance certification — not performed.** Pinned dependency/image diffs were read, but no online advisory or supply-chain audit was executed.
31. **Unrelated Sentinel edits and out-of-range controller checkboxes — excluded/preserved.** They are not frozen source evidence; controller must record its process state without staging unrelated changes.
32. **Start deadline bounded by original remaining monotonic Lease — mandatory next consumer.** Thirty-second wire limit is not proof of current remaining Lease; parent/lost/deadline and exact original fences must be rechecked.
33. **Accepted-only renewal and other command purposes — separate mandatory protocols.** Start evidence cannot become renew/task/file authority or proof of target acceptance.
34. **Target payload reconstruction and credential/env/path/default policies — mandatory next consumer.** Adapters must resolve final values and target must execute those same bytes/options under actual isolation; descriptor alone is insufficient.

Additional considered boundaries for this unit:

35. **Coherent registry deletion/recreation detection by standalone Load — intentionally not promised.** Load lacks an original-revision argument and returns only copied structural history; future effect state must retain/fence the original value/revision/Lease=0 rather than adopt a recreated record.
36. **Permanent global issuer rotation retention and safe GC — mandatory later.** Count grows with real issuer rotation, not idle sandbox N, but that still needs explicit budgets/reference retention and safe GC; no expiry-based deletion may be inferred.
37. **Provider concurrency, immutable lifetime and rotation availability — trusted operator contract.** Registration cannot prove all external implementations obey it; the provider must retain/select the requested exact digest for later signing, and unknown registration is never authorization to change identity or execute.
38. **Generic Stage writes/deletes to registry family — considered, not claimed protected.** Current generic API allows them. Controller explicitly elected scoped reservation hardening as described above; arbitrary raw store writers remain separately out of scope. This item requires a recorded implementation/disposition, not silent deferral.
39. **Certificate valid at exact storage commit instant — not an execution-authority promise.** Fresh verification occurs before the bounded passive metadata write; storage history may outlive validity. Actual effect/target consumers must freshly authenticate and check remaining deadlines before authorizing work.
40. **Full requested original-base package review — incomplete required gate, not declined as out of scope.** Exact uncovered spans remain in the coverage ledger; predecessor approval or passing suites cannot close them.

## Recommendations and assessment

Apply/disposition CA-M1 and the controller-elected registry Stage reservation in the single scoped final fix wave; preserve the failing focused evidence and add focused regressions without weakening constructors, history or uncertainty rules. Complete all unreviewed package intervals and retain these 40 individual boundaries before issuing a whole-branch continuation verdict.

**Covered-code assessment:** C=0 / I=0 / M=1 new, prior nonblocking M2 retained, plus the explicit controller Stage hardening decision. The reviewed issuer implementation and its direct integration boundaries otherwise match the current narrow plan.

**Ready for next original-capability effect-intent unit? Not yet certified by this report.** Controller gates passed, but the required whole-package reading is incomplete and final fix/disposition remains. After those gates, continuation is limited to that next authorized unit.

**Ready for global merge/deployment or complete phases 1–5? No.** Physical runtime authority/lifecycle, production wiring, Redis removal, safe GC, restore/deployment and real capacity acceptance remain mandatory work.

## Same-seat continuation pass 1 — additional package reading

Read `final-review-pass-1.json` first, then every literal subset line 1–1360 in bounded sequential chunks, with no truncation. Original package **198–702 and 804–1649** are now covered: **1,351 additional lines / 179,954 source bytes**. Cumulative confirmed coverage is **8,734 of 25,124 lines**; **16,390 remain**. The exact covered/pending intervals are persisted in `final-review-coverage.json` and the latest section of `final-review-coverage.md`. Reaching this subset's EOF is not a whole-package EOF claim.

The newly read material records management identity contracts, permanent acquisition/original-Lease creation claims, Operation final corrections, dispatch recovery, and publication review/correction history. The historical Operation stale-fence/original-evidence/harness findings and publication sibling Begin-cleanup finding have explicit corrective entries; their original issue headings are not new defects at frozen 0e2a67e. The docs consistently distinguish metadata history from live execution authority, same-host synthetic evidence from production capacity, and this incremental gate from phases 1–5 completion. These records inform the remaining source review and do not replace it.

**Pass result: no new Critical, Important or Minor finding; no newly declined boundary.** Existing CA-M1 (typed-nil channel clock), the controller's scoped generic Stage registry-family write/delete reservation decision, the prior nonblocking Grant-boundary copy-test M2, and all 40 individually reasoned boundaries above remain intact. Comparisons remain legal in the elected reservation; dedicated typed registration remains unchanged; arbitrary raw etcd writer protection is still a separate boundary. Nothing in this pass claims that reservation is already implemented.

No suites or focused tests were rerun; no product code, Git state or fixture changed, and no subagents were used. Controller's frozen-source gates remain the previously read evidence, including the retained initial environment-only full-repository failure and corrected passing run. **Whole-package review and next-unit readiness remain pending**, with the single final fix wave waiting for the completed findings list. No global merge, deployment or phases 1–5 completion approval is issued.

## Same-seat continuation pass 2 — additional package reading

Read the pass-2 manifest first and the entire literal subset sequentially through actual subset EOF 2394, with no output truncation. Newly reviewed original package intervals: `1650–2031`, `2191–2258`, `2320–2817`, `3884–4496`, `4734–4982`, `5128–5690`. This adds **2,373 lines / 179,949 source bytes**, bringing confirmed coverage to **11,107 / 25,124 lines** with **14,017 still pending**. Exact intervals are recorded in the current coverage JSON/ledger. Whole-package EOF is not claimed; the partial management_runtime.go validator continues at original line 5691.

The selected historical reports preserve the difference between protocol invariants, finite native fixture results, pre-GC raw-value samples and future physical/production/capacity acceptance. The source tests independently sign raw envelopes to exercise verification beyond signer rejection; they cover actual payload changes, all original operation context fields, simultaneous changed expected binding and signed binding, certificate ID/digest/key replacement, role/domain separation, overlap-root rejection, conservative validity boundaries, malformed wire and zero evidence on failure. Descriptor tests independently construct canonical bytes/digest and assert both input/getter copy isolation and actual metadata-size boundaries. The selected runtime identity verifier prefix separately checks context, pinned management trust and the runtime receipt domain before creating copied evidence; its remaining validator body is explicitly left for later coverage.

**Pass result: no new Critical, Important or Minor finding, and no new declined boundary.** CA-M1, the explicit Stage registry-family write/delete reservation decision (comparisons permitted, typed registration unchanged), prior M2 and all 40 existing boundaries remain as recorded. No tests were rerun, and no product/Git/fixture state changed. Whole-package review and next original-capability effect-intent readiness remain pending. The single final fix wave still waits for the complete findings list; no global merge/deployment or phases 1–5 completion approval is issued.

## Same-seat continuation pass 3 — additional source review

Read pass-3 manifest first, then every literal subset line through EOF 4347 sequentially, without truncation. Original spans `5691–6210`, `6515–7623`, `7948–8488`, `9940–11746`, `11847–12049`, `12338–12483` add **4,326 lines / 179,983 source bytes**. Cumulative confirmed coverage is **15,433 / 25,124 lines**, with **9,691 pending**. `domain_read_test.go` stops within a response-envelope case; original line 12484 onward is still pending. Whole-package EOF is not claimed.

The completed runtime receipt verifier retains independent role/domain, exact trusted runtime context and pinned fresh management trust. Publication signing/verification preserves bounded strict wire, actual Ed25519 authentication, immutable copied evidence, certificate-contained ready windows, current-time margins and separate historical behavior. Tests directly sign semantically invalid claims, cover 2^53+1 int64 precision, malformed nested wire and exact wire budgets. Client tests preserve all-endpoint identity checks, no automatic bootstrap, permanent metadata and owned mutable TLS copies; these are test-source observations, not new claims of a production mTLS exercise.

CreationClaim independently generates a fresh UUID and original Lease, coherently reads six permanent records, preflights its eventual 24 comparisons, and fences original value/revision/Lease plus claim/guard value/Lease/CreateRevision. Renew checks the old local monotonic deadline before adopting the sent-time new deadline; unknown or late replies cannot revive it. Already copied Stage fences remain governed by server arbitration, and tests explicitly verify that local loss does not retroactively revoke those copies. Existing receipt/cleanup contracts and the actual sibling Begin-reply-loss plus Revoke-failure regression remain intact. No new confirmed Critical/Important/Minor finding arose.

Additional considered items — controller ruling requested as part of the existing review process (not permission to perform a change):

41. **Legacy creation-claim decoder strictness — no new blocker proposed.** `internal/storage/state/etcd/creation_claim_records.go:27` uses canonical UUID parsing without an explicit `uuid.Nil` rejection, and `:52` uses standard JSON decoding plus `DisallowUnknownFields`, which does not reject duplicate fields or require an explicitly present zero-valued partition. This is less strict than the signed management/new registry wire. The supported producer at `creation_claim.go:65` chooses its own `uuid.NewString()` and encodes a typed record; decoding an already present claim at `:66–74` only returns corruption or occupied/conflict and never reconstructs a capability or adopts its Lease. Mutation capabilities retain private original bytes and revisions. Therefore no supported-input authorization failure is demonstrated; the concrete difference is corruption classification for externally malformed stored claim bytes. Recommend explicitly retaining this under the legacy trusted-storage/hardening boundary unless a stricter legacy decoder contract is elected. This does not weaken strict checks on untrusted signed wire or new issuer registry records.

42. **Birth-publication delegate/root key equality — operational separation remains required; no new blocker proposed.** `internal/runtime/controlprotocol/publication_sign.go:22` and `publication_verify.go:114–128` validate the certificate and pinned signature but do not reject a delegate public key equal to a pinned root, unlike the newer management identity verifiers. The existing birth protocol therefore relies on the trusted issuer supplying an independent runtime delegate, as its public signer comment and implementation plan require; it does not enforce that part of key-custody policy structurally. A caller without an accepted root signature cannot exploit this to mint authority, and a root public-key comparison cannot establish actual private-key isolation. The management-identity design explicitly retained the existing publication compatibility contract while adding root-as-delegate rejection for the new roles. Recommend treating this as an explicit retained birth-issuer/key-custody boundary, separately from the already mandatory production isolation work, rather than silently claiming all certificate families now enforce the newer check. Controller may elect scoped hardening, but no such change has been made or assumed here.

**Pass assessment:** no new confirmed C/I/M; two additional considered boundaries with the above concrete reasons. CA-M1, the controller-elected Stage command-issuers write/delete reservation, prior M2, and original boundary items 1–40 are unchanged. No tests, subagents, product/Git changes or fixture operations. Whole-package review and next original-capability effect-intent readiness remain pending; final single fix wave awaits complete findings. No global merge/deployment or overall phases 1–5 approval.


## Same-seat continuation pass 4 — additional source review

Read pass-4 manifest first, then all literal subset lines 1–4405 in bounded sequential chunks without truncation. Original spans `12484–12825`, `13227–13630`, `13668–14145`, `14390–14479`, `14955–15750`, `15879–16280`, `16441–16736`, `16911–17071`, `17269–17597`, `17681–17810`, `17941–18346`, `18522–18940`, `19063–19172` add **4,363 lines / 179,973 source bytes**. Cumulative actual coverage is **19,796 / 25,124 lines**, with **5,328 pending**: `19173–20558`, `20753–23072`, `23359–24980`. The last selected span ends within TestRuntimeDispatchCodecAndKeys; its continuation is unreviewed. This is subset EOF, not whole-package EOF.

The newly read domain tests distinguish invalid immutable records from absent reads and external failures, reject leased permanent records and preserve caller-independent snapshots before Grant. Fence tests inspect actual gRPC Txn shapes and comparisons, not merely builder representations. Fixture fault gates verify owned Compose project, running service containers, distinct members and loopback port correspondence. Operation tests cover original receipt/token/lock/guard recreation, delayed and lost replies, cleanup uncertainty, irreversible loss, sent-time renewal deadlines, cancellation/renewal serialization and fixed idle RPC cost. The latter remains synthetic evidence, not production capacity acceptance. The delayed Operation resolve harness includes unconditional release/cancel/join and an early-Fatal regression; this does not silently close boundary 28 for every older fault harness.

The runtime dispatch producer snapshots payload and claim comparisons, retains exact original fences and immutable attempt references, handles Begin errors without discarding cleanup uncertainty, and requires both a committed historical entry and live local claim checks before successful return. Recovery tests independently corrupt real three-point responses and record bodies, preserving distinct record/receipt/identity classifications, coherent first revisions, unleased immutable entries, all-absent behavior and historical recovery without reconstructed authority. The record-test prefix checks binding fields, target byte/UTF-8 bounds, exact keys, payload digest and typed-nil codec behavior; its unreviewed remainder is not inferred from the prefix.

**Pass result: no new confirmed Critical, Important or Minor finding; no new declined boundary.** Existing CA-M1, the controller-elected generic Stage command-issuers write/delete reservation (comparisons legal, typed registration unchanged), prior M2 and each of boundaries 1–42 are preserved. The Stage reservation is still an elected change, not claimed implemented. The identity decoder's legacy JSON permissiveness and older fault harness behavior remain within previously enumerated boundaries, without a demonstrated new supported-input authority failure in this pass.

**Boundary 41/42 controller disposition:** controller notified this seat that progress.md / verification-draft.md explicitly retain #41 as trusted legacy stored-claim corruption classification and #42 as birth-issuer independent delegate/key-custody policy. This is a recorded controller ruling, not a reviewer claim of new hardening. The ruling does not expand this fix wave, waive strict signed/new registry checks, or waive production custody. Additional independently read source evidence for #42 is internal/storage/state/etcd/publication_authority_test.go:99–111: TestPublicationConstructorCopiesRoots deliberately uses the root key's public key as birth certificate delegate. Its compatibility behavior must not be silently changed by the scoped CA-M1 constructor fix.

No tests or suites were rerun, no product/Git/fixture state changed, and no subagents were used. Whole-package review and next original-capability effect-intent readiness remain pending; the single final fix wave still waits for the completed findings list. No global merge/deployment or phases 1–5 approval.


## Same-seat continuation pass 5 — additional source review

Read manifest first and literal subset 1–4294 consecutively with no output truncation: original `19173–20558`, `20753–23072`, `23359–23934`, **4,282 new lines / 179,820 source bytes**. Cumulative **24,078 / 25,124**; **1,046 remain**, exact original interval **23935–24980**. Coverage ledgers updated. TestStageSurvivesFixtureLeaderFailure continues in that unread interval; no whole-package EOF claim.

Preparation writes freshly authenticate the exact certificate, preserve permanent UID reservation and original claim/domain fences, and recheck after Begin. Its coherent loader does not promote structural records into authentication. Publication writes update the four original domain records and immutable journal/proof/receipt atomically, retain original comparison inputs, and keep replay/history distinct from runtime permission. Tests inspect the full 64-operation budget with the original 24 claim comparisons, exercise index/claim/guard recreation and proof/context mismatches, preserve unknown Begin references and separately expose cleanup errors, and check post-Begin clock/expiry/cancellation/copy behavior. Historical reads remain bounded and independent of business expiry/current runtime presence. Capacity tests explicitly count synthetic retained raw values, excluding real throughput, replication and MVCC acceptance. Stage tests cover independent attempt IDs, copied comparison/write bytes, durable lost-response resolution and commit/abort arbitration; their copy test mutates after Begin returns, so it does not close prior M2's actual-Grant-boundary regression gap. Older fault-harness join behavior remains boundary 28.

**No new confirmed C/I/M finding or newly declined boundary.** CA-M1, the elected Stage registry-family write/delete reservation, prior M2 and boundaries/rulings 1–42 remain intact. No product/Git/fixture changes, tests or subagents. Whole-review and next-unit readiness remain pending; the single fix wave still awaits completed source coverage. No global merge/deployment or phases 1–5 approval.


## Final whole-branch verdict after same-seat pass 6

**This final assessment supersedes the opening coverage caveat and all earlier provisional coverage/readiness counts.** The exact frozen original-base package `b99b823d69de6baa860eb95d47bd269599b03f42..0e2a67edacfc5c1edf8cee554b053695726ea5ef` has now been actually read in full: **25,124 / 25,124 lines, 1,348,151 bytes, 58 commits, 131 files; no unread intervals.** Pass 6 read its manifest first, then all subset lines 1–1052 sequentially without truncation, adding original 23935–24980 (**1,046 lines / 42,194 source bytes**). The final subset was checked against the exact original bytes, and the recorded inclusive coverage union is 1–25124. This certifies whole-package EOF, not merely the final subset's EOF.

The final source segment completes Stage leader failure/receipt/arbitration/budget/copy tests and the AcquireIntent producer/tests. Acquisition snapshots payload before RPC, preserves original idempotent identity and TTL, keeps permanent global sandbox placement and generation fencing, rejects overflow/corrupt existing records, and separates temporary Stage lease cleanup from permanent business state. Tests exercise same-revision permanent writes, cross-partition identity conflicts, delayed complete transaction arbitration, receipt recovery and independent business TTL. No additional confirmed defect or newly declined boundary arose in this segment.

**Complete finding/disposition list at the frozen HEAD:**

- **Critical: 0. Important: 0. New Minor: 1 — CA-M1 / P3, OPEN.** `internal/storage/state/etcd/publication_authority.go:37`, reached by `command_authority.go:32`, omits `reflect.Chan` from typed-nil Clock rejection. The existing external-overlay failing evidence above remains valid; constructor/options can accept the nil channel clock. Fix the shared nil-kind check and add a constructor regression that proves no Observe call. This is a configuration fail-fast defect, not an authentication bypass.
- **Controller-elected scoped hardening, OPEN:** `internal/storage/state/etcd/mutation.go:101` currently permits generic Stage writes/deletes under `command-issuers`. Reserve that exact family for generic writes/deletes while retaining comparisons and the dedicated typed Register path. The controller has explicitly elected this in the one final fix wave; it is not claimed implemented or counted as a demonstrated authentication bypass. Future GC remains a separate typed conditional protocol.
- **Prior M2, OPEN / NONBLOCKING:** actual-Grant-boundary generic Stage mutation-copy regression. Production deep copying was reviewed and no alias defect was found. With `stage_attempt_test.go` and `stage_test.go` now fully read, the earlier report-only caveat is superseded: their source mutation occurs after Begin returns, so they do not provide the comprehensive at-Grant regression requested by M2. Retain this test-strengthening item without inventing a product defect.

All individually reasoned boundaries **1–42** and recorded controller rulings remain part of this final review. **Item 40 is now satisfied as a source-reading gate**, while its historical incomplete checkpoints remain preserved for audit. Controller retains #41 trusted legacy stored-claim corruption classification and #42 birth-issuer independent delegate/key-custody policy; these do not waive new signed/registry strictness or mandatory production isolation. The concrete Stage-family exception in #38 still requires the elected implementation/verification. No other boundary is silently waived or upgraded to completed behavior.

**Overall frozen-source verdict: whole-branch review COMPLETE; scoped final fixes/verification REQUIRED before next-unit readiness.** No Critical or Important defect was identified across the full package. The issuer prerequisite and reviewed integration preserve the intended passive-history versus live-authority distinctions, bounded original-record comparisons, authenticated scope/time checks and unknown-outcome handling. Apply CA-M1 and the elected Stage reservation together in the single final fix wave, run checks appropriate to that delta, and perform the planned scoped rereview. The complete findings list is now available; no additional broad source-review pass is needed merely to recover missing coverage.

The controller's frozen-source gates remain the evidence already recorded above: native fault/race **226 PASS / 0 SKIP / 0 FAIL, 99.647s**, corrected full-repository run **exit 0** with native etcd **76.840s**, vet/build exit 0 and diff check pass. The original full-repository ownership-guard failure is retained and was corrected by fixture environment only. These gates predate the pending fix wave and do not validate edits that have not yet occurred. This pass ran no tests and made no product/Git/fixture changes or subagent calls; only authorized review artifacts changed.

**Not approval for global merge/deployment or phases 1–5 completion.** After the scoped fix/verification/rereview gate, continuation is limited to the next authorized original-capability effect-intent unit, with all mandatory later physical, lifecycle, production, migration, GC and real-capacity work retained.

## 最终 scoped 复核（两项均关闭）

审查范围：`0e2a67edacfc5c1edf8cee554b053695726ea5ef..67129ab46f34a8a086c6fd2218a64d6221f51b2c`，2 commits / 5 files / 108 insertions / 3 deletions。已一次完整读取 12507B review package，核对 final-fix-brief.md、final-fix-report.md 与当前 HEAD；产品只读，无产品/Git/fixture mutation，无凭据读取，无 subagents。

## FindingVerdicts

- **CA-M1 / P3 — ADDRESSED**。`internal/storage/state/etcd/publication_authority.go:37` 将 `reflect.Chan` 纳入原有 nil-capable kind 分支，nil channel 返回 ErrInvalidConfiguration。`command_authority.go:32` 复用该验证，`client.go:57` / `client.go:142` 在 `clientv3.New`（`:168`）之前拒绝；该路径无 Observe、Certificate、SignStart 调用。`publication_authority_test.go:17` 的 panic Observe 和 `:23` 的 factory/options/New 断言、`command_authority_test.go:49` 的 nil-channel exec case 覆盖所要求入口，现有 unused issuer 的 panic methods 捕获 provider 误调用。全 nil optional 分支及信任复制逻辑未改。
- **Controller ruling / generic Stage command-issuers reservation — ADDRESSED**。`internal/storage/state/etcd/mutation.go:103` 精确匹配 namespace-relative 首段 `command-issuers`，拒绝 bare family 与全部合法 descendants 的 Put/Delete，保留旧 meta/p-attempts/p-stages 条件。`mutation.go:63` 仅应用于 writes；comparisons 仍走原验证和复制。`stage.go:89` 的 prepareMutation 失败发生在 Grant（`:108`）与 key RPC 之前。`stage_test.go:50` 覆盖六个 bare/point/deeper Put/Delete 无 mutation/digest 的错误，`:79` 以 counted Grant fake 断言 zero grants，`:100` 覆盖 family/point/deeper comparisons、copy 和 near-prefix writes/deletes。Typed `RegisterExecIssuer` 的直接 CAS/无 Lease OpPut（`command_issuer.go:55`）未改，不经过 generic prepareMutation。

## Evidence

已核对 actual report 的实际 RED/GREEN 记录与对应测试源码：clock RED 是 ErrInvalidConfiguration 期望得到 nil，exec 子测试单独选择后同样失败；reservation 六例 direct RED 是期望错误得到 nil，BeginStage 六例 RED 是 counted Grant fake 返回的 ErrOutcomeUnknown，均不是编译错误或 panic。clock GREEN regular 0.593s / race 1.703s；reservation GREEN regular 0.614s / race 1.720s；两片 package vet、gofmt 与 diff check 均记录 exit 0。报告明确修正初次 combined regex 未选中 exec 子测试的问题，证据无遗漏。两片分别提交 d82e276b982e8f7c887f7a422f32897d043f042e 与 67129ab46f34a8a086c6fd2218a64d6221f51b2c。

本 seat 无未解代码疑问，因此按 dispatch 不重复测试 suite；以上运行结果属于 implementer 实际报告，独立结论来自完整 fix diff 与局部源码检查。未对 dial 作 hook instrumentation；无 dial 结论由 otherwise-valid constructor 测试及 validate-before-client 源码顺序支持。Root 的 fresh native/full gates 另行记录。

## NewBreakage

未发现本次 fix diff 引入的新问题。两个生产变更各一行，新增测试匹配限定需求，未扩展比较限制或引入 provider startup 调用。

## OutOfScope

M2 actual Stage Grant/copy regression 属于下一 Stage/effect unit；本报告不将其表述为当前 alias bug，也不重复审查。未重新 whole-branch、历史范围或 phases 1–5，未评判 safe GC、raw etcd writer、原有 birth/codec/SignStart 协议。typed Register 仅核对此次修复未改变其直接 CAS 路径。

## Verdict

**PASS（仅本次 scoped final fix re-review）**：两项均 ADDRESSED，新增 findings 为 0。此结论不代表 fresh full/native gates、整体 phases 1–5 或 merge/deploy approval。

## 本单元裁决（完整原文，按发生顺序）

1. Ruling: proceedwithincontinuousapprovedoverallarchitecturewithoutanotherartifactapprovalhandoff — userauthorizedimplementation+continue andthisisnecessaryeffectprerequisite, noproduction/external action — costifwrong reversibleprovider/schema refinement/downstreameffect rework.

2. Ruling: operatorDI typedsignerprovider with noBackend privatekey andnoStartupCertificate/Sign calls — actualdemandrotation canremainexternaltrustedservice, noidleN refresher — costifwrong laterproductionkeycustody/service API refinement, no current command sent.

3. Ruling: useimmutableidempotentcert-slot CAS withoutStage/Grant — registry ispassiveglobalmetadata andunknowncannotstartphysicaleffect, singleUUIDkeypreventscompetingbodyoverwrite — costifwrong effectconsumer integrationrework; actualeffectunknown stillrequiresdurableStage arbitration.

4. Ruling: preservehistorical-loaderstructuralboundary; don'tpromise standalonecoherentrecreate detection withoutoriginalrevision — authorizedraw writer canchangecoherentstate, originalfuturecapmustfencepreviousrecordrevision/bytes/Lease0 — costifwrong strongerhistoryauditdesign oroperatoraccesscontrolrefinement; doesnotwaiveoriginalexecutionfences.

5. Ruling: keepglobalcertrecordspermanentuntilseparatesafeGC/referenceprotocol — uniquenesscannotbe silentlylostbyexpiryGC; recordcountfollowsissuerrotationsnotidleN — costifwrong longtermglobalmetadata footprint, measuredandexplicitfutureGC/backpressure requirement.

6. Ruling: allow scoped registry reader to retain/strictly validate full native Txn response instead of blindly delegating to readDomain which discards header — positive header revision/nestedshape are required authority-registry checks, cannot weaken spec or refactor older sharedprotocols — costifwrong bounded extraidentitypoint/readplumbing maintenance; fixedcostindependentN. Task2context flags exacthelper limitation andspecificjoinfile.

7. Ruling: reservecommand-issuers family for genericStage writes/deletes whileallowingfixedpointcomparisons anddirecttypedRegisterCAS — registrypermanence shouldhold across normalapplication mutation APIs, notjustRegister; rawauthorizedetcdwriter remains outsideapplicationcapboundary andoriginaleffectfencesstillmandatory — costifwrong futurecertificate safeGC needsdedicatedtypedconditional protocol insteadgenericStage deletes, smallcompatibilityrestriction tocurrentunusedgenericfamily. ApplyinONEfinalfixwave afterwholefindingslist/coveragecomplete, no controller product edits. ReviewerprovisionalMinor/P3 nilchannelclock gap acceptedtofixaswell; fullcontextcoverage gatepending.

8. Ruling: retain #41 legacy creation-claim decoder strictness under the existing trusted-storage boundary rather than expanding this final fix wave — supported producer generates a nonnil fresh UUID and typed record; decoding occupied history cannot reconstruct capability or adopt a Lease, and original bytes/revisions fence mutations; no supported-input authority failure was demonstrated — cost if wrong later legacy decoder hardening and corruption-classification rework. Strict untrusted signed wire and new registry checks remain mandatory.

9. Ruling: retain #42 birth-publication delegate/root equality as explicit trusted birth-issuer key-separation policy rather than claiming all certificate families enforce the newer management-role rule — accepted root signature is still necessary, newer management-role checks are enforced, and structural public-key equality cannot prove actual key custody; existing publication compatibility was preserved — cost if wrong future birth-issuer validator refinement and operational key-policy enforcement. Production independent delegates/private-key isolation remain mandatory.

## 后续

继续原始 operation 的 Stage/effect producer 单元，随后接 runtime、生命周期与调度，完成生产入口切换和 Redis／Sentinel 删除。不得把本中间门槛完成当作整体实现完成。保留全部 mandatory later 项、M2 和当前未转换的后续草案；不删除工作空间导致裁决或下一单元约束丢失。
