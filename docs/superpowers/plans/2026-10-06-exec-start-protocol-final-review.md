# 最终执行参数与 start 协议：验证、整分支复核与裁决

冻结源码 `b2c5d54002607b6947a8e17250f498c13fb05e9f`；user baseline `b99b823d69de6baa860eb95d47bd269599b03f42`。两个独立 task spec+quality gates 通过；整分支完整 53 commits / 121 files / 23,376 lines / 1,243,886 bytes 读至 EOF，C0/I0/M0 new，prior M2 明确保留。只代表可以继续下一已授权单元，不代表整体迁移、merge、部署或容量验收。

## 小提交与验证

- `ff57828`：actual exec descriptor，2文件409行；真实行为 RED1.204s、focusedGREEN1.551s、targetedrace2.080s、oncefullaffectedpackage0.282s、packagevet/gofmt/stageddiff通过。固定 JSON、二进制stdin摘要、所有选项、nil/empty/alias/并发、64KiB exact边界已测试。canonical样本326B。
- `46f521b`：仅文档明确 ticket与expected同时改错scope也必须拒绝 pinned binding；包含Task1完成记录。
- `b2c5d54`：strict start票据，3文件840行；真实行为RED1.048s，开发中四个错误测试预期修正（signer无descriptor参数故可签另一合法hash，但verifier拒绝；zerooffsetJSON规范化；validissuer输入应合法），未放宽生产检查；独立binding4例withheld-checkRED1.115s，恢复后GREEN；finalfocused3.146s、oncefullprotocolrace3.261s、vet/gofmt/stageddiff通过。

Controller冻结源码后 `TEST_ETCD_ENDPOINTS= TEST_ETCD_FIXTURE_PROJECT= TEST_ETCD_CONTAINERS= go test ./...` exit0（/tmp/exec-start-protocol-repo.log，protocol1.737s、etcd2.557s，其余通过/缓存）；fixture依赖 native用例未执行，非verbose输出无exactSKIP数。`go vet ./...`、`go build ./...`均exit0空日志，`git diff --check`通过。报告中的race/RED为执行者记录，独立 reviewer 未重复suite。此前未改native source29c3cf6上的实际owned216PASS/0SKIP/0FAIL107.208s和全仓89.676s保留Operation永久报告；旧fixtures已释放，未重用。

代表性issuer/ticket/metadata=534/1132/304B，stdin内容/stdout不在这些wire中；不证明全部持久effect/history/MVCC预算，也不证明10k/100k/1M或SLO。descriptor工作量随actualpayload，纯函数无idle-N后台循环。

## Rulings

Ruling: follow approved overall continuous execution with next narrow required protocol plan, without repeated artifact approval — user authorized implementation and said continue, no new production/external action — costifwrong reversible protocol refinement and downstreamintegrationrework.
Ruling: scope first commands to actual exec/stream payload and start-only evidence; effect/currentcapability/activeissuer/gate/renew/terminal remain mandatory next consumers — generic signed arbitrarydigest would notbindactualeffects and genericrenew risks stale start — costifwrong additional protocoltypes/integrationwork, no currentruntime permission.
Ruling: canonical metadata cap64KiB and stdin1MiB are explicit failclosed protocol bounds, not a claim existing unbounded code input remains supported at arbitrary size — boundedmemory/wire and current APIstdinlimit require exact inputbudget — costifwrong documentedadaptererror or future versioned/largerpayloadprotocol refinement.
Ruling: defer Stage-vs-specialized effect admission choice until intent integration plan; this unit adds no Grant or journal — no unsettled Stage detail needed for these pure byte contracts — costifwrong next integration may require extra temporaryguardLease accounting or API refinement, never adoption/regrant of originalopLease.
Ruling: clarify expectedcontextbinding must equal pinnedManagementVerifier binding, independently of ticket==expected — issueridentityevidence hasno bindinggetter; matchingcaller+wire couldotherwisehide delegation scope mismatch — costifwrong stricterpureverifier rejection/consumerconfigurationrefinement; add independently signed both-sides bindingtests, no production action.
Ruling: preserve each34reviewdispositions as scopedcontinuation boundaries, no overall mandatoryacceptance waiver — purebyteprotocol gates cannotprovephysicaleffects/production/capacity — costifwrong downstreamcurrentcap/launcher/GC/wiring rework, plus disclosed protocolavailabilitylimits.
Ruling: retain ownworkspace until allrulings surfacedandfutureeffectnotesconvertedtoverifiedplan — ephemeralreview/clock/targetdeadlineconstraints mustnotdieatlocalfinish — costifwrong scratch/docsfootprint only, not production authority.

## 每项 review 边界的处理

| # | 审查项 | Controller 处理 |
|---|---|---|
| 1 | 真实 Create/Prepare 防重与恢复 | 后续必做；元数据 dispatch 不能证明真实副作用只执行一次。 |
| 2 | birth/ready 的真实观察与可信签发 | 后续必做；签名不替代物理 gate/mount 观察。 |
| 3 | 真实 mount-once/FUSE 重试排除 | 后续必做；消费 mount intent 不等于已挂载。 |
| 4 | 永久 command/effect intent 与 current operation/task 授权 | 后续必做；actual payload descriptor/start wire 已推进，但原 capability 消费者、durable effect 与真实 invocation 仍未实现。 |
| 5 | target Begin/End、stream、后代与终态 | 后续必做；Cancel/失租不是 End。 |
| 6 | CloseData/All、独占与两侧 drain/reopen | 后续必做；token 消失不是物理排空。 |
| 7 | 续业务 TTL、销毁/task、安全释放 owner | 后续必做；保留完整任务状态机与证据。 |
| 8 | 证书安装/激活/轮转/撤销/ID 唯一注册 | 后续必做；比较 exact CertificateID、digest/key 与状态链，不凭 stateless cert 推断私钥持有。 |
| 9 | 生产签发服务及 root 私钥保管 | 后续必做；纯 signing helper 不提供运维隔离。 |
| 10 | 可信 PID1/launcher、UID/IPC/proc/FD/私钥隔离 | 后续必做；真实 Linux 隔离验收不可省略。 |
| 11 | raw/legacy 旁路移除与所有用户后代收拢 | 后续必做；新版运行时必须所有入口遵守 gate。 |
| 12 | 受控时钟生产实现与 target 实时复核 | 后续必做；纯 now 参数不是 API 调用者时钟。 |
| 13 | etcd commit/外部 effect 时刻绝对原子 UTC 到期 | 不承诺；采用受控误差边界及 target 执行前复核，不能宣称外部时间与 Txn 原子。 |
| 14 | warm pool/持久 slot/free 索引与恢复 | 后续必做；容量占用与未知状态必须真实计入。 |
| 15 | Docker 与上传/解压/工作区 mutation | 后续必做；真实 adapter 必须接入同一授权链。 |
| 16 | partition scheduler/R+1 Watch/共享续租/有界队列缓存 | 后续必做；原语固定成本不能证明 orchestration 已替换。 |
| 17 | collector/readiness/autosync/挂死 FUSE 探测槽 | 后续必做；挂死工作不能释放执行槽后无界补发。 |
| 18 | GC/引用保留/churn/history/未知 backpressure | 后续必做；不得按年龄删除未知证据，当前永久记录成本仍未满足最终预算。 |
| 19 | Manager/API/SDK/config native-only 接线 | 后续必做；生产调用链仍未切换。 |
| 20 | 旧版本 drain 与 Redis/Sentinel 运行依赖移除 | 后续必做；用户明确要求彻底替换。 |
| 21 | 生产 mTLS/auth/RBAC/prefix/authority 隔离/bootstrap | 后续必做；loopback fixture 不能证明部署安全。 |
| 22 | 灾备 restore epoch 与旧 writer/凭证隔离 | 后续必做；元数据纪元比较不能隔离所有外部旧进程。 |
| 23 | 真实 10k/100k/1M、Pod/FUSE/QPS/p99/恢复 SLO | 后续必做且尚未验证；1000 synthetic idle 与 raw-value 样本只证明局部结构。 |
| 24 | multi-cell 路由与跨 cell 语义 | 保留 phase 6；不纳入当前已批准 phases 1–5。 |
| 25 | 恶意 trusted Clock/TLS signer/callback 实现 | 不纳入进程内 hostile dependency 防御；仍要求 immutable、context-honoring、并发安全。 |
| 26 | 所有旧 raw native client/server envelope 的任意 Byzantine 输入 | 不作该保证；支持的 native server 契约与实际响应形状验证保留；signed untrusted wire 的严格校验绝不豁免。 |
| 27 | 有权 raw writer 协同恶意改写 etcd 权威记录 | 不承诺 Byzantine-proof store；item21 运维权限隔离仍必做，结构 loader 不授予执行能力。 |
| 28 | 所有旧 fault harness 系统性 early-Fatal join | 延后维护；特定 Operation M1 已修复并有回归，未声称所有旧 harness 均现代化。 |
| 29 | M2 Stage 实际 Grant 边界 comprehensive copy 回归 | 保留明确非阻塞测试加强项；已读源码未找到别名缺陷，不将其悄然删除。 |
| 30 | dependency/image 当前漏洞与 provenance 认证 | 尚未验证；版本/digest pin 不代表无漏洞，本 review 未运行在线审计。 |
| 31 | 无关 Sentinel 与冻结范围外 controller checkbox | 排除且保留；controller 自行记录当前 checkbox 完成证据，无关 Sentinel 编辑不被 staging。 |
| 32 | 原 operation 实际剩余 monotonic Lease 截止时间 | 下一消费者必做；30秒协议 interval 不是原 Lease 剩余时间证明，可信集成须保守换算并重检，public ref/history 不重建能力。 |
| 33 | accepted-only renewal / 其他命令目的 | 后续专用协议必做；start ticket 不复用为 renew/task/file 权限，须绑定已接受且未终态 journal 与独立 purpose/domain。 |
| 34 | target 实际 payload 重建、credential/env/path/defaults 策略 | 下一消费者必做；adapter 显式先解析默认、target 对实际字节重算，管理UID/ScopedFS与公开限额错误均须真正执行。 |

没有新 finding，不派发空 fix wave。当前计划结束后继续 overall effect/target/生命周期/GC/scheduler/接线工作；无push/merge/deploy动作。完整 workspace 暂保留。

## 原始独立整分支 review

# Whole-branch final source review — exec descriptor and start protocol

## Scope and method

Frozen base: `b99b823d69de6baa860eb95d47bd269599b03f42` (`feat/workspace-fuse-mount`). Frozen head: `b2c5d54002607b6947a8e17250f498c13fb05e9f`. A read-only `git rev-parse HEAD` still returned that head after the source review.

Read `final-review-context.md` first, then the supplied reviewer method and applicable SDD constraints. Read the **entire** supplied `review-b99b823..b2c5d54.diff`, sequentially from its header through the final `foreign-data:` line at EOF: **53 commits, 121 files, 23,376 lines, 1,243,886 bytes**. File count was determined from all `diff --git` headers. Early truncated outputs were recovered using bounded overlapping rereads; no omitted section was replaced by a narrower diff, prior approval or report. The package includes the overall state-management spec, current exec spec/plan, preceding protocol specs, implementation, tests, dependency changes, permanent verification/review records and isolated fixture files. Read the current progress ledger and both current task reports and independent reviews after completing the package.

This is an independent integrated source/test review of the whole frozen branch for the **next authorized implementation continuation**. It is not another review limited to the five latest protocol files. I compared the native authority chain, management and birth roles, descriptor/start producer-consumer contract, bounded costs, time assumptions and unknown-result semantics against the overall phases 1–5 vision and the current narrow acceptance criteria. Earlier rulings were considered explicitly rather than treated as proof.

No source, index, HEAD, credentials, environment files, fixtures or deployment state were changed. No network, subagents or test/race/vet/build reruns were used. Additional reads only obtained exact source line anchors and inspected existing verification logs. One line-anchor search used a nonexistent review-directory glob and failed before reading files; `rg --files` located the actual report paths. This report is the sole write.

## Integrated strengths and findings

The execution descriptor binds final argv order, sorted complete environment, UID/GID, clean absolute POSIX working directory, timeout, binary stdin length/hash, TTY and network intent. `execution_descriptor.go:60` copies the successful request into private storage, uses a fixed typed metadata representation and an actual-NUL domain, and applies the aggregate 65,536-byte canonical limit; `:97` copies all mutable request members for consumers. The 1 MiB stdin ceiling is separate. Independent expected JSON/hash tests, all-field changes, nil/empty equivalence, getter mutation/concurrency and exact aggregate boundaries exercise the contract. This adds bounded payload work rather than work proportional to idle sandboxes.

`exec_start.go:60` independently checks expected context against the verifier's pinned binding, freshly authenticates supplied issuer wire, matches exact issuer certificate ID/digest, strictly decodes the ticket, compares every original-operation context field and the opaque descriptor's actual digest, verifies the actual delegate signature, and applies conservative time checks. The ticket has a distinct start-only purpose and actual-NUL domain, is limited to 4,096 wire bytes, has an ordered interval no longer than 30 seconds, and is contained by issuer validity and business expiry (`:127`, `:148`). `:139` compares expiry instants consistently before the remaining comparable context. No public metadata reference is promoted into a capability by this API. The signed same-change-to-ticket-and-expected tests separately enforce the pinned scope, preventing a matching caller-supplied context from hiding delegation mismatch.

The issuer/runtime management identities remain separate from birth/publication proofs: copied pinned roots, independent delegates, exact runtime context, fixed domains, fresh validity and real signatures are enforced in `management_issuer.go:27`, `:69`, `:113` and `management_runtime.go:48`. A same-UUID replacement issuer cannot borrow an old key or digest. The shared strict codec (`publication_codec.go:46`, `:67`, `:133`) rejects missing/null/duplicate/unknown nested fields, malformed integers, malformed UTF-8/surrogates, offset timestamps and trailing JSON before typed decoding. The new tests use independent raw signing to avoid signer prevalidation masking verifier defects. Historical identity evidence and a fresh start check still do not establish target activation; the separate continuation requirements below remain necessary.

The native chain remains coherent. Acquire writes the seven permanent domain records and receipt atomically, preserves request identity/TTL on replay, reserves sandbox IDs across partitions and increments the permanent workspace generation under CAS (`workspace_acquire.go:46`). Creation claims retain their original Lease, monotonic deadline, claim/guard revisions and six exact permanent records. Dispatch, binding, UID reservation, consumed mount intent and publication retain the original claim and immutable dependency fences. Structural historical loaders validate complete associated records and receipts in bounded identity-fenced point snapshots; cryptographic mutation paths separately authenticate the stored certificate/proof.

Publication still updates only the six intended business values plus its receipt atomically (`runtime_publication.go:31`), retains every dependency fence and the reserved UID index, and preflights the complete **64-operation** budget. Dispatch/Bind/Consume budgets remain 42/50/54. No fence was removed to fit publication. Replayed publication is explicitly a historical report with fresh proof checks, not a new permit or Lease. Bind/Consume replay uses the same-Txn default linearizable point read rather than an empty read-only transaction (`runtime_preparation.go:247`, `fence_check.go:8`); the actual protobuf interceptor tests both branches.

Operation admission retains all five permanent original records, immutable original-operation descriptor, original guard and admission revisions, and token/receipt/mutation lock under the same original Lease (`operation_admission.go:99`, `:274`). Failed admission publishes a historical outcome only after complete receipt/guard/token shape, original guard revision and ordering validation (`:390`). Renewal retains original fences, request context and deadline, performs a default linearizable read in the fence Txn, and validates the old deadline after delivery (`operation_lifecycle.go:16`). It does not adopt new control or regrant a Lease. Resolver arbitration permits an abort only for the validated guard-only attempt and never reconstructs a capability (`operation_resolve.go:14`, `:120`). Cancellation is explicitly metadata cancellation, not successful target End (`operation_lifecycle.go:99`). The earlier I1/I2 repairs are present; their former defects are not reopened by the current additions.

Generic Stage still copies comparison Key/RangeEnd/oneof data and write bytes before Grant (`mutation.go:39`, `:126`; `stage.go:77`). Permanent committed/aborted receipts arbitrate a delayed whole transaction, and neither missing reads nor an RPC error is mistaken for an external abort. Higher-level Acquire/Dispatch/Bind/Mount/Publish separate primary Begin failure from independent original-guard cleanup diagnostics (`stage.go:62`, `workspace_acquire.go:204`, `runtime_dispatch.go:112`, `runtime_preparation.go:224`). The original Operation delayed-worker test joins before hook/fixture teardown, including the deliberately failing child-process path. Older harness limitations and prior M2 remain separately recorded below.

**Critical: 0. Important: 0. Minor: 0 new.** No confirmed new defect requiring a source fix was found in the frozen range. Prior **M2 remains an explicitly retained nonblocking test-strengthening item**, with its precise location `internal/storage/state/etcd/stage_attempt_test.go:185`: a comprehensive generic Stage actual-Grant-boundary mutation matrix is still absent. Current pre-Grant copying is correct on source inspection; existing post-Begin and higher-level RPC-boundary tests are narrower than that requested matrix. This is not counted as a newly discovered alias bug.

## Verification evidence and practical limits

Inspected `/tmp/exec-start-protocol-repo.log`: all listed packages pass or are cached, including fresh `controlprotocol 1.737s` and `etcd 2.557s`. The controller's recorded command explicitly cleared native fixture endpoint/project/container variables, and the current context/ledger records exit 0. Thus this log is **not** a fresh execution of the real native fault cases; the nonverbose log does not provide an exact skip count. `/tmp/exec-start-protocol-vet.log` and `/tmp/exec-start-protocol-build.log` are both zero bytes; the controller records exit 0 for both and a clean whitespace gate at the frozen head. Empty logs alone are not substituted for that execution record.

Current implementation reports record Task 1's actual-behavior RED/focused GREEN, targeted race 2.080s, full affected package 0.282s, formatting and vet; Task 2 records actual-behavior RED, corrected test expectations, an additional four-case pinned-binding RED with the check withheld, final focused GREEN 3.146s, once full protocol race 3.261s, vet and formatting. Those reports were checked against the actual tests and production code; historical command chronology and race execution were not regenerated by this review. Independent task approvals provide context but do not replace this source review.

Native source is unchanged from the recorded `29c3cf6` boundary. The permanent Operation verification report retains the prior owned-fixture **216 top-level PASS / 0 SKIP / 0 FAIL**, native race **107.208s**, and actual repository run **89.676s**, including the post-I1/I2/M1 repair state. Those are report-backed earlier executions, not new native runs at `b2c5d54`. No old fixture was reused. There is no new claim of whole-repository race, online vulnerability review or clean historical golangci output.

Cost evidence is scoped accurately. The exec report's actual representative issuer/ticket/descriptor metadata sizes are 534/1,132/304 bytes; the descriptor's separate sample is 326 bytes. These are samples, not maximum storage or runtime capacity proofs. The native pre-GC creation sample includes **20 retained records**, with the 12 journal/index/receipt records plus seven original domains and Acquire receipt; varying dispatch payloads retain the fixed fixture snapshot. It excludes keys, MVCC/WAL/history/replicas and does not meet the final live-state 8 KiB target by itself. The two-active-operation test retains identical **16 Txns / 38 point reads / 2 Grants / 2 KeepAlives** before and after 1,000 synthetic idle records; it proves fixed primitive structure, not QPS, latency or large-N acceptance. No new unanswered executable doubt justified violating the no-rerun constraint.

## Individually declined or set-aside behaviors

The following preserves all 31 prior dispositions, updates the command item for what this increment actually implements, and adds three distinct exec integration requirements. Each item has its own reason. Mandatory later items remain acceptance work for phases 1–5 and must not disappear when this review is recorded.

1. **Physical Create/Prepare deduplication and recovery — mandatory later.** Dispatch is durable metadata; no actual runtime producer/consumer in this range establishes at-most-once physical effects or recovery.
2. **Truthful birth/ready issuance and physical gate/mount observations — mandatory later.** Signature verification authenticates assertions; the trusted target implementation that observes and issues them has not been installed by this range.
3. **Physical mount-once/FUSE operation identity and retry exclusion — mandatory later.** Consuming one mount intent records a decision; it does not establish the actual mount invocation or forbid adapter retries physically.
4. **Committed command/effect intent and current operation/task authorization — mandatory later.** The actual-payload descriptor and start-only signature protocol are now implemented, but the durable effect declaration, opaque current-capability consumer and invocation path are not. The old item is partially advanced, not closed.
5. **Target Begin/End, stream/child accounting and terminal receipts — mandatory later.** Metadata committed/aborted/expired and Lease cancellation do not prove runtime termination or cessation of descendants' effects.
6. **CloseData/CloseAll, exclusive barriers and dual etcd-plus-target drain/reopen — mandatory later.** No physical close/drain/reopen consumer exists in this increment; token disappearance alone is insufficient.
7. **TTL extension, destruction/exclusive tasks and safe owner release — mandatory later.** Permanent expiry/ownership fields are present, but the proof-gated lifecycle task machine and release path still need implementation.
8. **Certificate install/activation/rotation/revocation and certificate-ID uniqueness registry — mandatory later.** Stateless fresh verification and exact ID/digest do not demonstrate durable active membership or live possession by the target; operational certificate lifecycle remains required.
9. **Production issuance service and root/delegate key custody — mandatory later.** Helpers validate supplied keys; isolated service policy, storage and deployment of those keys are outside this source increment.
10. **Trusted launcher/PID1, management UID, IPC/proc/FD and private-key isolation — mandatory later.** Nonroot descriptor credentials and signatures do not establish the supervisor's physical privilege boundary.
11. **Raw/legacy entrypoint bypass removal and complete descendant containment — mandatory later.** A pure start verifier cannot prove all production user-code paths pass through it or remain inside a trusted process tree.
12. **Production controlled-clock source and target-time rechecks — mandatory later.** The verifier receives a trusted observation; actual source installation and each execution-time consumer must establish its freshness/error bound. No public API-supplied clock is newly authorized.
13. **Absolute atomic UTC expiry at etcd commit or external-effect instant — not promised.** The protocol uses controlled bounded observations and fences, not an atomic transaction with external time; subsequent physical checks remain necessary.
14. **Warm pool, persisted slots/free indexes and recovery accounting — mandatory later.** Fixed metadata paths do not implement the runtime pool or persistent admission-slot recovery.
15. **Docker parity and upload/extraction/workspace-mutation adapters — mandatory later.** Target binding in a schema does not prove these real adapters consume native authority correctly.
16. **Partition scheduler, fixed-revision/R+1 watches, shared renewal and bounded queues/caches — mandatory later.** The primitive costs do not prove replacement of old scans/periodic orchestration or compaction recovery.
17. **Collector/readiness/autosync and hung FUSE probe slots — mandatory later.** Publication verification does not implement physical bounded probes or prove hung work retains an accounted execution slot.
18. **Safe journal/receipt/snapshot GC, retention/reference rules, churn/history and pending-unknown backpressure — mandatory later.** Current permanent data accumulates. Age alone cannot authorize deleting unresolved evidence; raw pre-GC samples are not final live-state budget acceptance.
19. **Manager/API/SDK/config and native-only production state wiring — mandatory later.** The production call graph has not been switched end to end by these isolated primitives.
20. **Old-version drain and Redis/Sentinel runtime dependency removal — mandatory later.** These are user requirements still unfulfilled; remaining legacy packages are not considered migrated.
21. **Production mTLS/auth/RBAC, prefix/authority credentials and operator bootstrap — mandatory later.** Client validation and a loopback unauthenticated fixture do not prove deployment access isolation.
22. **Disaster restore unique epoch and old-writer/credential quarantine — mandatory later.** Exact metadata comparisons fence this client when metadata changes; external old writers and cloned authorities also need operational quarantine.
23. **Real 10k/100k/1M Pod/FUSE capacity, QPS, p99 and failure-recovery SLOs — mandatory later.** Synthetic idle points, byte samples and small isolated fault runs are not acceptance at those scales.
24. **Multi-cell routing/cross-cell semantics — phase 6 later.** Explicitly outside the currently authorized phases 1–5; single-cell evidence cannot establish it.
25. **Malicious trusted Clock/TLS signer/callback implementations — trusted dependency contract.** Owned data is copied; supplied in-process implementations must remain immutable, context-honoring and concurrency safe. No hostile-code sandbox is promised for those dependencies.
26. **Arbitrary Byzantine native client/server envelopes across older generic Stage/client/domain paths — not promised.** Specified Operation/replay checks are present; native client/server behavior remains trusted elsewhere. No supported native-server response path causing a new defect was found. This does not weaken strict signed untrusted-wire requirements.
27. **Coordinated authoritative-state forgery by an authorized raw etcd writer — outside application capability boundary.** Historical loaders are structural, and actual mutation paths add crypto/CAS. Operational access isolation, not checksums alone, must prevent such a writer.
28. **Systematic early-Fatal worker joins across all older fault harnesses — deferred maintenance.** The concrete Operation M1 harness and regression are fixed; broader old Stage/dispatch/preparation/publication teardown modernization was not performed and is not claimed as complete.
29. **Prior M2 comprehensive generic Stage copy regression at actual Grant — retained, nonblocking.** `stage_attempt_test.go:185` tests narrower post-Begin mutations; `mutation.go:39` and `:126` already deep-copy before Grant. No actual alias defect was identified, but the requested boundary regression remains open rather than silently removed.
30. **Online dependency/image vulnerability and provenance certification — not performed.** Pinned versions/digests and source changes were reviewed; network/advisory/provenance audit was outside the allowed review operations.
31. **Unrelated Sentinel edits and working-tree controller checkbox changes — outside frozen range.** They were not modified or used as part of the branch verdict. The controller retains responsibility for recording its own process evidence and preserving unrelated edits.
32. **Ticket expiry bounded by the original operation's actual remaining monotonic Lease deadline — mandatory next consumer.** The pure signer/verifier can enforce a 30-second maximum interval, but it has no current opaque capability/deadline input. A trusted signer integration must derive the stricter remaining bound and recheck liveness; a public reference or historical outcome cannot supply it.
33. **Accepted-only renewal and other command purposes — separate mandatory protocols when integrated.** This wire permits only start. It cannot safely be reused as renewal, task authority, file mutation authority or proof that a target previously accepted the command; purpose/domain and durable acceptance semantics require their own consumers.
34. **Actual target payload reconstruction and adapter credential/environment/path policy — mandatory next consumer.** The descriptor binds the provided final fields and applies no defaults. Production adapters must resolve defaults before construction, reconstruct and execute those same bytes/options, enforce management-UID/ScopedFS policy, and return explicit bound failures. This range has no runtime executor to verify those obligations.

## Verdict

**Ready to continue the next authorized implementation unit. C=0 / I=0 / M=0 new; prior nonblocking M2 retained.**

The entire frozen branch provides a coherent metadata and cryptographic foundation for the next effect/current-capability integration, and the new exec primitives preserve actual-payload, pinned-scope, issuer-role and start-only boundaries. No confirmed new defect blocks that continuation. All 34 individually listed dispositions remain explicit; the controller should record them before advancing. Overall phases 1–5, migration completion, merge, deployment, production activation and real capacity acceptance are not approved or established by this verdict.

Coverage: **53 commits / 121 files / 23,376 lines / 1,243,886 bytes, read through EOF with truncation recovery**. Evidence: independent complete source/test review, inspected current controller logs, and clearly distinguished report-backed protocol/native executions. No tests rerun.
