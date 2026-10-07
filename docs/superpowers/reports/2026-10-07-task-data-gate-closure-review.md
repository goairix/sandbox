# Task data-gate closure — independent whole-unit review

Spec compliance: **Approved for this unit**. Code quality: **Approved**. **Critical 0 / Important 0 / Minor 4** retained nonblocking evidence/fixture concerns; no new product fix requested. **CannotVerify / declined: 34 individually numbered items below.** The original Task4 I1 is addressed. This unit is ready for acceptance and dependent cleanup work; production readiness and complete Redis migration are not established.

## Scope and method

Reviewed the complete unfiltered original range `9a7e37d2bfc4c39e92168d226226423d737ea0e4..d955cf738262ed281b9ac9f75603864219a637db`. The supplied package was read once, contiguously through EOF: 57 files, 7,179 lines, 378,516 bytes; independently recomputed SHA256 `c66e6f59ca270df0e38141eebbbf387e701bf4ed051f74663f6c0667897d3725`. Final product source is `4867821013786259b748c740609ecf811bfff42e`; subsequent gate changes are documentation. Read binding design/plan, all packaged source/tests/review/verification documents, and current OWN/progress.md. Applied the requesting-code-review/code-reviewer.md rubric, including reasonable caller expectations. I authored no product in this unit.

This is the one entire-unit integration review, not a second review of the whole migration branch. No Git operation, suite/build/native replay, Docker operation, nested agent, or source mutation was performed. This report is the sole output file. Evidence below is retained execution evidence with its original attribution, not newly generated results.

Focused unchanged seam reads were limited to concrete risks:

- `internal/runtime/controlrunner/supervisor_renew.go:1–116`, `supervisor_failure.go:1–150`: examine supervisor/execution/journal lock ordering, asynchronous failure, original monitor renewal and Close/key/journal lifetime.
- `internal/runtime/controltarget/journal_renew.go:1–65`, `journal_accept.go:109–139`, `journal_activation.go:88–139`, `journal.go:93–113`: determine whether an operation passing the earlier supervisor guard can bypass the newly closed gate or reuse a poisoned handle. Journal live checks still require an open gate; same journal mutex serializes renewal and closure.
- `internal/storage/state/etcd/task_claim.go:33–100`, `stage.go:77–190`: confirm original claim Value/Lease/Create/Mod comparisons and irreversible loss, builder-before-Grant behavior, original guard/receipt CAS and copied mutation boundaries. The new draft does not replace these authorities.

Two initially guessed seam filenames (`supervisor_close.go`, `journal_live.go`) did not exist; read-only lookups exposed that immediately, and the actual `supervisor_failure.go` and `journal_activation.go` were inspected. These are review navigation errors, not product or verification failures.

## Integration findings and strengths

1. **Typed attribution composes with authoritative producers.** `controlprotocol/task_close_types.go:11`, `task_close.go:31`, and `etcd/task_close_effect_validation.go:46` distinguish a valid signature from a live original claim. The task digest is derived from canonical retained TaskRecord; destroying birth, claim birth, original Lease and complete immutable context are pinned. New typed wire bounds and domains are separate from exec and retain strict nested validation and copied evidence.
2. **One immutable command survives uncertainty.** `etcd/task_close_effect.go:15`, `:49`, `:119`, and `task_close_effect_validation.go:28` keep one original draft/deadline/command, reuse the same Stage reference over unknown responses, and never adopt historical discovery as Prepared. Renewal cannot widen the draft deadline. The conservative 59-operation charge is correctly distinguished from the actual 58 serialized nodes, with unchanged 64-operation/64-KiB-record/256-KiB-mutation limits.
3. **Local persistence and proof have distinct authority.** `controltarget/journal_task_close.go:15`, `:80`, `:169` preserve pending → closed gate → terminal attribution and late reauthentication. The scanner counts retained temporary bytes, capacity preflight includes both additional slots, and local records contain no ticket/payload. Structural readable terminal history is not proof. `journal_task_close_receipt.go:16–76` holds the journal mutex across clean-live/accounting/installation/key/certificate checks, fixed-record signing and final reauthentication; concurrent terminal IO cannot poison the handle between check and signature creation.
4. **Dispatch is one-shot; historical query stays read-only.** `etcd/task_close_delivery.go:18–150` rechecks original intent/fences/deadline before connection, after handshake and before application bytes, sets deliveryAttempted before Send, and retains unknown rather than resending. Query loads structural original history but still requires the exact independently pinned destination certificate and runtime signature. `task_close_authority.go:21` supports trust+clock query-only construction without calling or requiring a signing provider; Prepare still requires one.
5. **Existing transport and ownership remain coherent.** `controltransport/task_close_frame.go:61` routes a separate bounded canonical DTO through the existing exec decoder, preserving old serialization; `destination.go:154` shares the same one-shot Session owner. `controlrunner/supervisor_task_close.go:13` seals admission before journal work and retains active owners. `supervisor_transport.go:300` independently checks query eligibility while holding s.mu, uses TryLock for the execution owner, and clears kind/wire after late context/failurePending loss. This addresses original I1 without disabling healthy terminal queries after data closure. Borrowed transport lifetime still precedes key/journal destruction.
6. **Scope stays narrow.** No new idle-N actor, clock, dependency, scheduler/watch, production backend selection, execution End, remote settlement or owner release was added. No receipt field silently upgrades data-admission closure into those stronger effects. The fixed-purpose atomic signer and query-only verifier are justified changes to the earlier plan direction; neither exposes arbitrary-record signing or caller-selected trust.

## Critical

None found in the reviewed unit.

## Important

None open. Original **Task4 I1 is addressed** by `supervisor_transport.go:300–329`: the final guard runs before s.mu unlock, rejects closed/failed/failurePending/missing activation/context loss, and returns nil wire on late loss. Retained evidence contains five real failing cases before the change, portable 2-top/8-sub race PASS and the new actual PID1 1-top/6-sub eligibility PASS. This proves those cases, not every schedule or retroactive invalidation of a previously returned receipt.

## Minor — retained evidence and fixture concerns

These are nonblocking for the bounded unit, not a declaration that the fixture output was pristine. All previous Minor IDs are explicitly triaged here.

1. **M1 — Expected missing-Lease diagnostics are noisy.** `docs/superpowers/reports/2026-10-07-task-close-etcd-review.md:11` and `2026-10-07-task-close-delivery-review.md:13` retain Task3 M1 / Task4 M1: 7 + 5 client missing-Lease warnings and 21 + 15 replicated apply warnings around deliberate revoke/lost-reply cleanup. The behavior is consistent with those exercised boundaries, but unclassified warning streams would obscure a new failure. Keep exact expected-log classification in future fixture maintenance; do not suppress product logging or weaken idempotent cleanup tests. No production fix required here.
2. **M2 — The expired original RPC warning is not CAS evidence.** `2026-10-07-task-close-etcd-review.md:12` retains Task3 M2. Its DeadlineExceeded only establishes expiry of that call; `etcd/task_close_effect_delayed_test.go:79–92` separately submits the captured original comparisons through a fresh bounded context while the original Stage guard remains live and observes server refusal. Keep these claims separate. Existing corrected attribution is sufficient for this unit.
3. **M3 — Fixture startup settings do not certify production settings.** `2026-10-07-task-close-etcd-review.md:13` and `2026-10-07-task-close-delivery-review.md:14` retain Task3 M3 / Task4 M2: shared HTTP/gRPC port, directory0755 and simple-token notices (112 across Task3 batches; 16 in final metadata gate). These remain isolated test configuration concerns. Production TLS/auth/permissions require their own gate; improve the three settings independently when changing that fixture.
4. **M4 — Historical storage/schema errors remain unresolved.** `2026-10-07-task-close-etcd-review.md:14` and `2026-10-07-task-close-delivery-review.md:15` retain Task3 M4 / Task4 M3: Task3 E/F each had two schema warnings and one storage error; failed Task4 metadata preparation had the same 2-WARN/1-ERROR pattern before healthy wait/test start. All relevant final member checks showed storage3.6.0/no OOM, but that does not establish the cause or safety of a restore/upgrade. Preserve exact raw timing and investigate in the relevant maintenance/production gate; do not promote the bootstrap interpretation into diagnosis.

**Task4 M4 closed as a documentation/evidence-classification issue:** `2026-10-07-task-close-delivery-review.md:16` now explicitly labels the misnamed RED as passing characterization and Darwin no-tests output as compile-only. Original filenames/raw failures are preserved. Neither is counted as meaningful preimplementation RED or native PASS. Tasks1/2 had no open Minor.

## Retained test evidence and practical limits

- **Protocol:** final focused 8 top / 360 sub PASS, no FAIL/SKIP; once-only affected-package race and vet. Initial missing API and fixture mistakes are compile/setup failures, while four intentional binding-mutant failures demonstrate sensitivity. No claim of a behavioral RED for every new API.
- **Journal:** host final 15 top / 187 sub PASS and actual Linux arm64 CGO0 15 top / 187 sub PASS, 29 raw commands exit0 with constrained payload/source/compiler custody and empty cleanup inventories. The 52.010s full-package race used an earlier ordinary test-file hash; final focused evidence covers the later alias/U+FFFD tests. Native CGO0 is non-race.
- **Metadata:** seven actual batches, 14 batch tops / 100 subs PASS, 91 actual namespace fixtures, 294 raw commands exit0; warnings/errors are retained above. Host 13-top/43-sub output includes eight native parent-only PASS entries; real host coverage is 5 top / 32 sub with 91 native SKIP, not 91 native successes. Corrected native evidence covers the test assertion fix. The real record-birth RED has three behavioral failures; earlier API/hook/build/assertion setup failures are not product RED.
- **Delivery:** real TLS portable tests; separate actual protected PID1 closure, active valid-renew/query-shutdown and fixed query-eligibility gates; separate actual Linux atomic-journal signing (4 top / 4 sub); final actual three-member metadata gate 4 top / 6 sub. The metadata handler uses real mTLS/codecs/journal but is not the protected PID1 supervisor. This is compositional validation, not a same-fixture backend-to-PID1 end-to-end result.
- **Source inheritance:** consume original manifest/audit attribution recorded in tracked verification documents and the retained Root audits; do not relabel all old suites as final-byte runs. Final I1 gate binds 101 compiled repository files + three compiler files to4867821. Journal52 and final metadata196 compiled sets remain unchanged; earlier target101 sets differ in three runner files. Task3 final host's test-only difference and Task2 full-race test-only difference remain disclosed. No new blanket provenance/suite certification was generated by this review.
- The failed project-name preparation, byte-slice-vs-RawMessage fixture assertion, Docker CAP_ comparison preparation, invalid BootID activation fixture, receipt-test hang/SIGQUIT and evidence-parser mistakes remain historical failures with their original classification. None contributes positive behavioral proof. Actual final terminal status, no OOM, source custody and exact cleanup inventories apply only to their named fixtures.

## Prior CannotVerify triage now satisfied within this unit

The following are no longer missing cross-task implementation; their wider limits remain numbered below.

| Prior item | Independent whole-unit disposition |
| --- | --- |
| T1 CV1 | Canonical task/revision/claim derivation and native fence/budget producer are implemented in Task3; reviewed complete source and retained native evidence. |
| T1 CV2 | Durable-close/signing composition is implemented in Tasks2/4; no drain/release implication. |
| T1 CV3 | Controlled clock/context producers and exact activated peer/runtime interval checks compose in Tasks3/4; operator implementation and exhaustive TLS limits remain CV3/CV6/CV13. |
| T1 CV5 | Current unit, Linux target/journal and metadata task gates are present; production/platform/scale portions remain separately declined below. |
| T2 CV1 | Fixed-purpose atomic signer closes the structural-lookup/poison race. |
| T2 CV2 | Original Prepared/claim/Stage/fence/deadline ownership reviewed across Tasks3/4. |
| T2 CV3 | Separate one-shot Session plus exact pinned mTLS/destination implemented and exercised. |
| T2 CV4 | Healthy closed terminal query and shutdown borrower lifetime implemented; I1's process eligibility loss fixed. |
| T2 CV5 / T3 CV14 | Entire new Task1 protocol diff is included here; no second execution of its accepted matrix. Inherited libraries remain CV12. |
| T2 CV24 / T3 CV22 / T4 CV20 | This complete unfiltered original-unit review satisfies the outstanding review, not whole-branch acceptance. |
| T3 CV1 | Activated peer and original interval independently checked at delivery and target. |
| T3 CV2 | One-shot application send and original-authority checks compose. |
| T3 CV3 | Clean live atomic journal signing composes with durable close. |
| T3 CV4 | Original historical query after claim/ticket expiry implemented; fresh certificates still required. |
| T3 CV13 | Complete changed journal source included here, with original accepted evidence retained; no inherited full-journal recertification. |
| T3 CV15 | Actual protected Linux target now exists; Linux race remains CV2. |
| T3 CV21 | Concrete original Stage/issuer extraction/clock coupling reviewed; whole inherited protocol/operator assurance remains CV12/CV13. |
| T4 CV19 | All task-owned seams are now visible together without ownership exclusions; original execution/source boundaries remain CV14/CV34. |
| T4 CV21 | I1 addressed with real behavioral RED and focused host/native GREEN. |
| Fix CV4 | All original concerns are individually resolved, retained as Minor, or mapped below; none is silently waived. |

## CannotVerify / declined to judge — individually retained

Each item is a limit of this review/available evidence, not an inferred product defect. Mandatory downstream lifecycle/deployment work remains mandatory.

1. **CV1 — Same-fixture backend-to-protected-PID1 E2E.** T4 CV1 / Fix CV3: real three-member metadata and physical target ran separately; require the combined gate before production integration.
2. **CV2 — Linux application race instrumentation.** T2 CV8 / T3 CV15 / T4 CV2 / Fix CV1: Linux CGO0 native execution is non-race; Darwin race does not certify Linux races.
3. **CV3 — Every live TLS/context adversarial combination.** T4 CV3: full typed field mismatch tests plus selected real peer/claim negatives are not an exhaustive TLS Cartesian matrix.
4. **CV4 — Forced accepted-before-spawn/in-flight Start interleavings.** T2 CV16 / T4 CV4: actual running-owner retention and source locking are checked; every registered pre-spawn schedule was not forced, and needs the next quiescence gate.
5. **CV5 — Sixteen-connection saturation.** T4 CV5: inherited16/5s bounds and lifetime coupling remain, but this unit does not measure saturated resource/latency behavior.
6. **CV6 — Forced preemption of uncooperative dependencies.** T2 CV12 / T4 CV6: context-honoring Clock/issuer/Dial is the contract; cancellation cannot terminate arbitrary nonreturning code.
7. **CV7 — Real power loss/device-cache/reboot durability.** T2 CV6 / T4 CV7: actual syscall/fault-hook/cold-open tests do not simulate physical power loss; cold/poisoned history remains unable to sign.
8. **CV8 — Production mount/image custody and storage permissions/capabilities.** T2 CV7: owned constrained tmpfs/payload fixtures certify their own layout only, not production delivery or persistent-device properties.
9. **CV9 — Unsupported-platform execution.** T2 CV9: helper behavior is statically fail-closed; no actual unsupported-GOOS run is available.
10. **CV10 — A real 65,536-file journal/deep-directory workset.** T2 CV10: file-slot tests alter counters while byte tests perform bounded real IO; arithmetic coverage is not directory-scale measurement.
11. **CV11 — Large-existing-N, long-running throughput/latency/recovery and performance improvement.** T2 CV11 / T3 CV16 / T4 CV16: no10k/100k/1M benchmark or sustained resource result exists; no new idle actor is only a source property.
12. **CV12 — Whole inherited security/parser/crypto/Stage stack and root provisioning.** T1 CV4 / T3 CV21: focused coupling and the complete new unit were reviewed, not every predecessor, standard library, module-cache byte or root-issuance procedure.
13. **CV13 — Concrete operator issuer/clock/Dial key security and concurrency guarantees.** T1 CV3 / T3 CV21: interfaces require immutable context-honoring concurrent-safe behavior; fixture providers do not certify a production provider.
14. **CV14 — Every accepted suite rerun at identical final test bytes.** T2 CV13 / T3 CV23 / Fix CV5: original host/native source differences are expressly retained; focused final gates cover changes without recreating every old suite result.
15. **CV15 — Meaningful initial behavioral RED for every new API.** T4 CV8 and protocol history: missing symbols, harness errors and passing characterization cannot retroactively become RED; real record-birth, query-only and I1 regressions have their separately reported proof.
16. **CV16 — CloseAll.** T2 CV14 / T3 CV5 / T4 CV9: independent user-admission/quiescence effect and receipt are outside this CloseData unit.
17. **CV17 — Etcd operation-prefix drain.** T2 CV15: no linearizable operation-token-empty completion protocol is implemented or certified here.
18. **CV18 — Complete launcher accepted/active/descendant/sole-Wait drain.** T2 CV16 / T3 CV6 / T4 CV10: retaining original owners or test teardown does not prove business quiescence or all descendant absence.
19. **CV19 — FUSE/data/mounter drain.** T3 CV7 / T4 CV11: separate topology and trusted drain ownership are required; execution-registry absence would not establish them.
20. **CV20 — Final sync.** T2 CV17 / T3 CV9 / T4 CV12: no final typed management sync effect or outcome evidence is implemented.
21. **CV21 — Final flush.** T2 CV18 / T3 CV9 / T4 CV12: local journal fsync is not workload/remote flush completion.
22. **CV22 — Remote settlement.** T2 CV20 / T3 CV8 / T4 CV13: receipt does not establish settled/rejected external requests or prevent an old PUT/DELETE/CompleteMultipart arriving later.
23. **CV23 — Remote external-write fencing.** T2 CV21 / T3 CV8: local closed admission is not an independent external resource write fence.
24. **CV24 — Exact runtime UID/Boot termination/runtimeGone.** T2 CV19 / T3 CV10 / T4 CV14: no business termination protocol or evidence is supplied by CloseData or fixture cleanup.
25. **CV25 — Domain-specific End.** T2 CV25 / T3 CV10 / T4 CV14: local closure/terminal diagnostics are not completion of the domain operation.
26. **CV26 — Safe owner/control/index conditional release.** T2 CV22 / T3 CV11 / T4 CV15: required local isolation and remote/effect proofs do not exist yet; this unit does not release those records.
27. **CV27 — Production Manager/API/adapters/images/watch/FUSE/deployment wiring.** T2 CV23 / T3 CV12 / T4 CV16: no production selection change is in this unit; dependency readiness is not cutover readiness.
28. **CV28 — Redis removal and old-version reader/writer drain.** T2 CV26 / T3 CV12 / T4 CV16: the overall migration remains unfinished.
29. **CV29 — Actual cause of historical schema/storage errors.** T3 CV17 / T4 CV17: timestamps and pinned-source interpretation support an inference only; root cause is unproven (M4).
30. **CV30 — Etcd rolling upgrade/downgrade behavior.** T3 CV18 / T4 CV18: finite pinned-version fixture arbitration does not exercise version transitions.
31. **CV31 — Backup, crash recovery and restore-epoch maintenance.** T3 CV19 / T4 CV18: final healthy storage-version checks do not certify disaster recovery or restored-state fencing.
32. **CV32 — Detecting coherent malicious historical replacement before first discovery.** T3 CV20: structural Load has no external initial anchor; its result cannot construct Prepared, and authenticated target receipt remains necessary.
33. **CV33 — Exhaustive concurrency/fault schedules and failures after a returned receipt.** T4 CV22 / Fix CV2: finite actual lock/IO schedules are covered; a later failure cannot retroactively revoke bytes already returned at a valid observation point, nor do those bytes prove drain/release.
34. **CV34 — Blanket re-certification of inherited suites/platform or source supply chain.** T1 CV5 / T3 CV13–15 / T4 CV19 / Fix CV5: original manifest/payload/compiler custody supports the named runs; this review adds integration analysis, not a fresh branch-wide suite, supply-chain or production security audit.

## Assessment

**Ready to accept this unit: Yes. Spec: Approved. Quality: Approved.** The complete new implementation composes original permanent claim/intent authority with strict typed tickets, one-way durable local attribution, atomic clean-live signing, bounded one-shot delivery and eligible historical queries. No Critical/Important defect or required in-unit product repair remains. Retain the four Minor concerns and all34 explicit limits; proceed with the mandatory downstream cleanup/production work without treating CloseData as drain, End, release or completed migration.

## Root acceptance and individual dispositions

Accepted bounded CloseData unit at reviewed HEAD d955cf7; product source 4867821. C0/I0; no product fix wave. This is not whole-migration acceptance. Original Task4 I1 is fixed and independently reviewed. Task4 M4 is closed only as corrected evidence classification. Prior CV items resolved by integration retain the mappings above. OWN is retained until exhaustive human publication of rulings; no prior closed OWN is accessed or deleted.

- M1 — Ruling: 保留逐条 missing-Lease 与复制 apply 日志分类；不全局静音，不视为 pristine。误判会遮蔽新的 fixture 故障。
- M2 — Ruling: 将过期 RPC 与独立新 context 提交原始 CAS 被服务器拒绝分开归因。误判会把客户端超时当成服务端隔离证明。
- M3 — Ruling: 保留 startup 配置限制；生产 TLS/auth/权限须单独验收。误判会推广测试配置到生产。
- M4 — Ruling: 保留历史 schema/storage 错误及时间线，维护门排查；健康结果不证明根因或恢复安全。误判会遗漏运维缺陷。
- CV1 — Ruling: 同一 fixture 的三成员 Backend→真实受保护 PID1 完整链路列为下一单元强制门；组件验证不能替代。 — 若判断错误，会高估本单元证据或遗漏对应后续门；后续门未完成前不允许生产切换/安全释放。
- CV2 — Ruling: 保留 Linux CGO0 非 race 限制，Darwin race 单独归因。 — 若判断错误，会高估本单元证据或遗漏对应后续门；后续门未完成前不允许生产切换/安全释放。
- CV3 — Ruling: 接受有限 typed/TLS 组合覆盖，不声明穷尽对抗组合。 — 若判断错误，会高估本单元证据或遗漏对应后续门；后续门未完成前不允许生产切换/安全释放。
- CV4 — Ruling: 下一单元强制覆盖 accepted-before-spawn 与 Start-in-flight 原始 owner 仲裁。 — 若判断错误，会高估本单元证据或遗漏对应后续门；后续门未完成前不允许生产切换/安全释放。
- CV5 — Ruling: 16 连接饱和资源与延迟留到生产容量门，不从配置上限推导性能。 — 若判断错误，会高估本单元证据或遗漏对应后续门；后续门未完成前不允许生产切换/安全释放。
- CV6 — Ruling: 保持 context-honoring 依赖契约，任意不返回代码无法由 Go 强制取消；不得伪称 join。 — 若判断错误，会高估本单元证据或遗漏对应后续门；后续门未完成前不允许生产切换/安全释放。
- CV7 — Ruling: 保留物理断电/设备缓存/重启未模拟限制，cold/poison 无签名权。 — 若判断错误，会高估本单元证据或遗漏对应后续门；后续门未完成前不允许生产切换/安全释放。
- CV8 — Ruling: 生产 mount/image/持久存储权限与能力列入部署门，fixture 仅证明自身。 — 若判断错误，会高估本单元证据或遗漏对应后续门；后续门未完成前不允许生产切换/安全释放。
- CV9 — Ruling: 保留 unsupported GOOS 实际执行未覆盖，静态 fail-closed 单独归因。 — 若判断错误，会高估本单元证据或遗漏对应后续门；后续门未完成前不允许生产切换/安全释放。
- CV10 — Ruling: 真实 65536 文件/深目录工作集留到容量门，计数器测试不是规模测量。 — 若判断错误，会高估本单元证据或遗漏对应后续门；后续门未完成前不允许生产切换/安全释放。
- CV11 — Ruling: 1万/10万/100万已有 sandbox 及持续延迟/恢复压测为迁移强制容量门；不声明性能提升。 — 若判断错误，会高估本单元证据或遗漏对应后续门；后续门未完成前不允许生产切换/安全释放。
- CV12 — Ruling: 接受命名依赖与新单元完整 review，不重认证全部继承安全/解析/crypto/Stage/root provision。 — 若判断错误，会高估本单元证据或遗漏对应后续门；后续门未完成前不允许生产切换/安全释放。
- CV13 — Ruling: 生产 issuer/clock/Dial 的密钥安全、不可变并发及取消契约须在接线前验证。 — 若判断错误，会高估本单元证据或遗漏对应后续门；后续门未完成前不允许生产切换/安全释放。
- CV14 — Ruling: 保留每次实际 source 归因；只补变更 seam 的检查，不把旧 suite 重标为最终字节执行。 — 若判断错误，会高估本单元证据或遗漏对应后续门；后续门未完成前不允许生产切换/安全释放。
- CV15 — Ruling: 保留真实 RED 与 compile/setup/characterization 分类，不能补造每个 API 的初始 RED。 — 若判断错误，会高估本单元证据或遗漏对应后续门；后续门未完成前不允许生产切换/安全释放。
- CV16 — Ruling: CloseAll/USERS 永久屏障与排空为下一单元必做，不由 CloseData 推导。 — 若判断错误，会高估本单元证据或遗漏对应后续门；后续门未完成前不允许生产切换/安全释放。
- CV17 — Ruling: 原始 claim 完整 fencing 下的 operation-prefix 线性化零观察为下一单元必做。 — 若判断错误，会高估本单元证据或遗漏对应后续门；后续门未完成前不允许生产切换/安全释放。
- CV18 — Ruling: 原始注册/Start/monitor/sole Wait/完整 namespace 排空为下一单元必做。 — 若判断错误，会高估本单元证据或遗漏对应后续门；后续门未完成前不允许生产切换/安全释放。
- CV19 — Ruling: FUSE/data/mounter 排空由独立可信 topology 与 owner 完成，后续必做。 — 若判断错误，会高估本单元证据或遗漏对应后续门；后续门未完成前不允许生产切换/安全释放。
- CV20 — Ruling: 双层排空后 final sync 必做。 — 若判断错误，会高估本单元证据或遗漏对应后续门；后续门未完成前不允许生产切换/安全释放。
- CV21 — Ruling: 双层排空后 final flush 必做；journal fsync 不等于业务 flush。 — 若判断错误，会高估本单元证据或遗漏对应后续门；后续门未完成前不允许生产切换/安全释放。
- CV22 — Ruling: 远端外部请求 settled/rejected 证明后续必做。 — 若判断错误，会高估本单元证据或遗漏对应后续门；后续门未完成前不允许生产切换/安全释放。
- CV23 — Ruling: 未 settled 时独立外部 write fencing 后续必做，本地闭门不是远端 fence。 — 若判断错误，会高估本单元证据或遗漏对应后续门；后续门未完成前不允许生产切换/安全释放。
- CV24 — Ruling: 精确 UID/Boot 的终止/runtimeGone 后续必做，fixture cleanup 不是业务证明。 — 若判断错误，会高估本单元证据或遗漏对应后续门；后续门未完成前不允许生产切换/安全释放。
- CV25 — Ruling: 领域 operation End 后续必做，local terminal 不是领域成功。 — 若判断错误，会高估本单元证据或遗漏对应后续门；后续门未完成前不允许生产切换/安全释放。
- CV26 — Ruling: 获得完整本地/远端/effect 证据后才允许 owner/control/index 条件释放，后续必做。 — 若判断错误，会高估本单元证据或遗漏对应后续门；后续门未完成前不允许生产切换/安全释放。
- CV27 — Ruling: Manager/API/adapters/images/watch/FUSE/deployment 生产接线为迁移必做。 — 若判断错误，会高估本单元证据或遗漏对应后续门；后续门未完成前不允许生产切换/安全释放。
- CV28 — Ruling: 完整 Redis 移除与旧版本 reader/writer 排空为迁移必做。 — 若判断错误，会高估本单元证据或遗漏对应后续门；后续门未完成前不允许生产切换/安全释放。
- CV29 — Ruling: 历史 schema/storage 根因尚未确证，关联 M4 留到维护/生产门排查。 — 若判断错误，会高估本单元证据或遗漏对应后续门；后续门未完成前不允许生产切换/安全释放。
- CV30 — Ruling: 滚动升级/降级须独立运维门，固定版本 fixture 不证明版本过渡。 — 若判断错误，会高估本单元证据或遗漏对应后续门；后续门未完成前不允许生产切换/安全释放。
- CV31 — Ruling: backup/crash recovery/restore epoch 须独立部署恢复门。 — 若判断错误，会高估本单元证据或遗漏对应后续门；后续门未完成前不允许生产切换/安全释放。
- CV32 — Ruling: 保留首次发现前一致恶意历史替换的信任边界；Load 无 capability，仍需认证 target receipt。 — 若判断错误，会高估本单元证据或遗漏对应后续门；后续门未完成前不允许生产切换/安全释放。
- CV33 — Ruling: 有限 IO/调度覆盖不能证明穷尽；返回后故障不能撤销既有字节，receipt 仍无排空/释放权。 — 若判断错误，会高估本单元证据或遗漏对应后续门；后续门未完成前不允许生产切换/安全释放。
- CV34 — Ruling: 命名 source/compiler/payload custody 仅支持命名执行，不新增全分支/平台/供应链认证。 — 若判断错误，会高估本单元证据或遗漏对应后续门；后续门未完成前不允许生产切换/安全释放。

Review package custody: `.superpowers/sdd/2026-10-07-task-data-gate-closure/review-whole-unit-9a7e37d..d955cf7.diff`; SHA256 `c66e6f59ca270df0e38141eebbbf387e701bf4ed051f74663f6c0667897d3725`. Complete independent report copied verbatim above; retained Root native audits/raw/source/compiler/payload records remain in current OWN and their tracked per-task verification reports. No new suite or Docker execution accompanies this documentation acceptance.
