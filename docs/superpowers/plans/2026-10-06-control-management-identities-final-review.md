# 持续管理身份整分支复核与裁决

冻结源码 `39a669bbe74ca77ec3e77838812cbda1f23e8b52`；基线 `b99b823d69de6baa860eb95d47bd269599b03f42`。独立 reviewer 完整阅读 48 commits / 113 files / 21,797 lines / 1,132,281 bytes 至 EOF。C=0 / I=0 / M=0 new；此前 M2 明确保留。只允许继续下一已授权实现单元，不代表整体迁移、上线、merge 或真实容量通过。

## Controller rulings

Ruling: implement narrow ongoingidentity prerequisite withoutanotherapprovalhandoff — userapprovedfullarchitecture and continuousimplementation; this adds no new productgoal/deploy/secretbinding — costifwrong is reversible wire/API refinement and subsequentactivationprotocol rework, not a productionsecurity action.
Ruling: ongoingcerts intentionally are stateless attribution, activation/revocation/rotation must later compareexactstatechain and targetdurableactiveCertificateID/key — root certificate cannotprovekeycustody orphysicallifecycle — costifwrong is downstreamcontract rework; noexecution authorityexposed in thisincrement.

Ruling: 逐项保留以下 mandatory 验收，不把 narrow identity gate 当整体完成；特定可信依赖边界及 phase6 范围维持原设计 — 当前两个增量未构建真实执行/运维/容量消费者 — 若误判，成本为下游协议和接线返工；任何 mandatory 行都不能靠此表豁免。

| # | 独立审查项 | Controller 处理 |
|---|---|---|
| 1 | 真实 Create/Prepare 防重与恢复 | 后续必做；元数据 dispatch 不能证明真实副作用只执行一次。 |
| 2 | birth/ready 的真实观察与可信签发 | 后续必做；签名不替代物理 gate/mount 观察。 |
| 3 | 真实 mount-once/FUSE 重试排除 | 后续必做；消费 mount intent 不等于已挂载。 |
| 4 | signed command/effect 与操作/task 授权 | 下一实现依赖；身份不能单独授权执行。 |
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

## Verification

两个小提交 `e12e6e2` / `39a669b` 已各自行为 RED、GREEN、自审并通过独立 spec+quality review。Task2 完整 controlprotocol race 2.413s（执行报告证据）。Controller 冻结源码后 `TEST_ETCD_ENDPOINTS= TEST_ETCD_FIXTURE_PROJECT= TEST_ETCD_CONTAINERS= go test ./...` exit0，实际 controlprotocol 3.125s、etcd 3.230s，其余通过/缓存；未配置 fixture，相关 native integration 跳过，非新 native fault 验收。`go vet ./...` 与 `go build ./...` exit0、空输出；`git diff --check` 通过。

Native 源码自 `29c3cf6` 未变；此前 fresh owned native race 216 top-level PASS / 0 SKIP / 0 FAIL、107.208s 与实际全仓 89.676s 证据保留于 Operation 永久报告。fixture 已全部释放，旧端口不可复用。未声明全仓 race、历史 golangci 或 production security/capacity 通过。

没有新 finding，故不派发空 final fix wave。SDD workspace 暂保留，完整 rulings/原文已存 Git，整个替换仍持续实施。

## 独立原始 review

# Whole-branch final review — management identities increment

Reviewer: management_whole_branch_review. Reviewed 2026-10-06. This is an independent source review for continuation of the authorized work, not approval to merge, deploy, or call the overall migration complete.

## Scope and coverage

Frozen baseline: `b99b823d69de6baa860eb95d47bd269599b03f42` (the user's `feat/workspace-fuse-mount` baseline). Frozen head: `39a669bbe74ca77ec3e77838812cbda1f23e8b52`; read-only `git rev-parse HEAD` confirmed the head.

Read own `final-review-context.md` first, then the requesting-code-review reviewer instructions and applicable SDD final-review rules. Read the ENTIRE supplied `review-b99b823..39a669b.diff`, sequentially in bounded segments, through its final Compose volume declaration at EOF. Early tool-output truncations were recovered by bounded rereads; no section was inferred from an approval or omitted. Verified the supplied package is **21,797 lines / 1,132,281 bytes**, covering the supplied 48-commit, 113-file whole-branch range. No diff was regenerated and no narrower self-created review range replaced this package.

Coverage includes all changed specs/plans/permanent reports/dependencies, publication and management cryptography and tests, namespace/TLS/identity client, generic Stage/receipt/attempt protocol, domain records and readers, Acquire, creation claim/lifecycle, dispatch declaration/recovery, binding/mount preparation, publication/recovery, Operation admission/original evidence/resolution/renew/cancel, all fault/validation/capacity tests, and fixture ownership/script/Compose documentation. Focused reads of actual function locations were used only for cross-reference. The overall etcd design remains the authority for phases 1–5; prior reports supplied constraints and historical evidence, not proof that code is correct.

The unrelated Sentinel plan edit and controller-owned management plan checkbox changes are outside the frozen range and were preserved. No source, index, HEAD, branch, fixture, environment, credential, service, or runtime changes were made. This report is the sole review write. No subagents were dispatched.

## Strengths and integrated assessment

- The new verifier is immutable and deployment-pinned: `management_issuer.go:27` copies one or two Ed25519 roots and binding; `:113` rejects any pinned root as a delegate, including overlap root A certifying root B. Wire data never installs a new root. Role-specific verification actually checks signatures after the common root/context/time helper (`management_issuer.go:69`, `management_runtime.go:48`). Fixed distinct signing domains include the actual NUL byte; common helper reuse has not accidentally turned root selection into signature authentication.
- `command_issuer` and `runtime_receipt` remain separate schemas, roles, domains and evidence. Runtime receipt identity binds exact sandbox/workspace/generation/runtime ID/UID/BootID and exact deployment namespace/authority/target/restore. No business expiry was smuggled into ongoing identity, and no birth certificate limit was weakened to obtain ongoing management authority. The <=4096-byte strict typed wire path rejects missing/null/unknown/duplicate fields, malformed UTF-8/surrogates, integer overflow/fractions, invalid key/signature sizes and non-UTC wire timestamps. Conservative one-second freshness boundaries and copied input/output evidence are covered with independently signed positive and negative cases.
- The producer-to-consumer chain is coherent: Acquire creates a permanent ownership/control/request/intent/snapshot/placement graph; a creation claim carries the exact original permanent and leased fences; dispatch preserves immutable payload/operation/attempt identity; binding reserves an exact runtime UID; mount consumption records one attempt or explicit plain-mode decision; publication authenticates birth and fresh ready evidence before committing the six business updates plus receipt atomically. Structural recovery APIs do not confer mutation or execution capability. Unknown outcomes retain their exact reference and cannot be replaced by absent-read guesses.
- Publication retains all 24 original claim comparisons. Original immutable metadata is loaded coherently, validated and then fenced by its original ModRevision and Lease=0. `runtime_publication.go:31` and `mutation.go:39` preserve the complete 64-operation and 256-KiB accounting rather than dropping fences to fit. Payload and protobuf-comparison deep copies precede Grant. Cleanup diagnostics remain separate from the primary outcome in all high-level Acquire/Dispatch/Bind/Consume/Publish consumers.
- Operation admission (`operation_admission.go:99`) binds its original caller context, positive bounded original Lease, original permanent fences, immutable guard and token/receipt first revisions. Mutation exclusion blocks new admissions without claiming already admitted work has drained. The failure path (`:390`) validates original guard, exact original body/reference, guard/completion ordering and token/receipt pairing before publishing historical outcome. The resolver never reconstructs an opaque capability from public evidence; it only arbitrates the original guard-only attempt.
- `RenewOperation` (`operation_lifecycle.go:16`) and preparation replay (`runtime_preparation.go:247`) now have a default nonserializable point Range in BOTH branches of the SAME comparison Txn. The response shape is validated before authority is used. This addresses the actual stale follower comparison failure, not merely a separate preceding read. Renewal preserves the old monotonic deadline across replies, requires the original Lease/envelopes, and makes failed renewal irreversible. Cancel serializes with renew and fences locally before its original bounded Revoke, without conflating cancel with terminal End.
- The delayed original Operation fault test joins the worker before hook/fixture teardown on parent Fatal and worker Goexit paths. The child-process regression (`operation_resolve_test.go:153`) exercises both failures. This is a concrete correction of the cited M1 path, not a claim that every older fault harness was modernized.
- Fixed point reads and private per-active-operation state avoid introducing work proportional to idle N in these primitives. The new certificate code starts no renewal loops, watchers, tickers or goroutines. Tests distinguish native server transactions and genuine reply-loss/delayed-dispatch faults from pure cryptographic tests; synthetic capacity figures are labeled correctly.

## Issues

### Critical — 0

No new confirmed Critical finding in the reviewed range.

### Important — 0

No new confirmed Important finding in the reviewed range or load-bearing defect in the current management identity plan. The remaining overall phases are listed individually below; they are not completed or waived by this verdict.

### Minor — 0 new

No new confirmed Minor finding. The prior M2 request for a comprehensive generic Stage copy regression at the actual Grant boundary remains a nonblocking, explicitly retained test-strengthening item. Source review found copies of comparison Key/RangeEnd/oneof payload and write bytes before Grant; existing tests cover copies after Begin and higher-level RPC-boundary inputs. No actual alias defect was found, so M2 is not reclassified as a new functional issue.

## Verification evidence and limits

No suite, race run, vet, build, fixture or focused test was rerun during this review: source inspection did not uncover a concrete doubt that justified the permitted focused-test exception.

Read the actual current repository log `/tmp/management-identities-repo.log`: the affected controlprotocol package reports 3.125s and etcd 3.230s; other packages pass/cached. The context records this command exited 0 with native endpoint/fixture variables explicitly empty. Therefore native fixture-dependent cases skipped in this run; the package-level `ok` is not fresh native fault evidence. Read the actual vet/build log sizes: both zero bytes. Their successful exit status is controller-reported, since an empty log alone does not establish exit status.

Read both current implementation reports. Task 2 records meaningful behavioral RED after an initial missing-API compile RED, focused GREEN 0.521s, and the once-run full controlprotocol race GREEN 2.413s. The two failed intermediate test fixtures were corrected as inputs (509-byte alleged overlimit namespace and zero-offset location lost in JSON serialization), with no production validation relaxation. Task 1 records behavioral RED 1.527s, focused GREEN 1.236s, race 1.572s and whole affected package 0.300s. These are report-backed execution results, not commands executed by this reviewer; the corresponding test sources were independently reviewed in full.

The permanent prior native report/context records fresh owned-fixture **216 top-level PASS / 0 SKIP / 0 FAIL**, native race 107.208s, whole-repository 89.676s and vet/build pass at the unchanged native-source baseline `29c3cf6`. The earlier `c3fd` stale-renew failure remains part of the history and is explained by the inspected I1 correction. Exact old fixtures were reported released with zero owned container/volume/network resources; no old ports or resources were reused. There is no claim of a fresh native run at 39a, whole-repository race coverage, clean historical golangci output, or production capacity/security certification.

The structural scale test (`operation_lifecycle_test.go:263`) checks two active operations before/after 1,000 synthetic idle points. The recorded exact cost is 16 Txns / 38 point reads / 2 Grants / 2 KeepAlives. Creation samples retain 20 permanent KVs with about 12.8/16.8/20.9 KB of raw values for 16 B / 4 KiB / 8 KiB dispatch inputs and a fixed 56-byte snapshot. Neither measurement proves B_live <=8 KiB, MVCC/WAL/replica cost, safe retention, or real large-N/SLO acceptance.

## Recommendations for continuation

Carry the following individually adjudicable boundaries into the next implementation unit and overall ledger. Future command/effect consumers must take the authenticated identity together with their own exact current capability/context/clock checks; exposing a copied public key must never become sufficient execution authorization. Preserve explicit unknown and target-drain semantics when wiring these primitives into actual runtime work. Keep physical-effect and capacity acceptance open until demonstrated by the applicable integration work.

## Declined to judge / set aside for individual controller rulings

Each item below was considered and deliberately not treated as completed by this increment. Items marked mandatory remain part of the authorized phases 1–5; their absence from this identity increment is not a waiver of the overall requirements.

1. **Physical runtime Create/Prepare deduplication and recovery — mandatory later:** dispatch currently declares permanent operation metadata; it contains no actual target invocation implementation to establish at-most-once effects or recovery.
2. **Truthful target birth/ready issuance and physical gate/mount observations — mandatory later:** signatures authenticate the asserted evidence, while the trusted producer and target observation implementation are not installed by this range.
3. **Physical mount-once/FUSE operation identity and retry exclusion — mandatory later:** mount intent consumption is metadata. It neither mounts nor demonstrates the target executed exactly the recorded attempt.
4. **Signed command/effect protocol and current operation/task capability authorization — mandatory later:** the new certificates provide identity attribution; command schema, effect validation, active capability rechecks and actual command execution are a separate next dependency.
5. **Target Begin/End, execution/stream/child accounting and terminal receipts — mandatory later:** Operation metadata and Lease cancellation do not establish runtime termination, acknowledged End, or cessation of side effects.
6. **CloseData/CloseAll, data/exclusive barriers and dual target-plus-etcd drain — mandatory later:** no physical close/drain/reopen consumer was implemented. Lease or token disappearance alone remains insufficient.
7. **TTL extension, destruction/exclusive task workflows and safe owner release — mandatory later:** business expiry and permanent ownership records exist, but the lifecycle task machine and proof-gated release paths still require implementation.
8. **Ongoing certificate installation, activation, rotation, revocation and certificate-ID uniqueness registry — mandatory later:** the present primitive is stateless. It does not demonstrate active delegate membership or possession of the certified private key at a live target.
9. **Production root/delegate issuance service and root key custody — mandatory later:** signing helpers correctly validate supplied keys, but service isolation, storage, operator issuance policy and installation are not provided here.
10. **Trusted launcher/PID1, management UID, IPC/proc/FD isolation and private-key protection from user descendants — mandatory later:** no launcher/supervisor boundary was built by this increment; a valid certificate is not proof of physical custody.
11. **Raw entrypoint/legacy bypass removal and all user-code descendants under the trusted process tree — mandatory later:** identity verification alone cannot demonstrate absence of bypass execution paths in the eventual target.
12. **Production controlled-clock source and target-time authorization rechecks — mandatory later:** the pure verifier accepts a trusted caller observation; clock production/installation and command/effect consumers must establish that trust. No caller-controlled API clock is newly wired here.
13. **Absolute atomic UTC expiry at the etcd commit/effect instant — not claimed:** the reviewed contract uses conservative bounded observations and fencing, not an atomic transaction with external time. Future target checks remain necessary; no impossible absolute guarantee is inferred.
14. **Warm pool, persisted slot/free-index admission and recovery accounting — mandatory later:** no pool/32-slot runtime management path is implemented in the reviewed native primitives.
15. **Docker parity and upload/extraction/workspace mutation integration — mandatory later:** schemas and target binding are not evidence that these real adapters use the native state/command protocol.
16. **Partition scheduler, bounded fixed-revision/R+1 watches, shared renewal and bounded queues/caches — mandatory later:** fixed-point primitive costs do not demonstrate replacement of existing periodic/scanning orchestration or recovery under compaction.
17. **Collector/readiness/autosync and hung FUSE probe slot accounting — mandatory later:** publication proof validation does not implement the bounded physical probes or prove hung work retains its bounded execution slot.
18. **Safe receipt/journal/snapshot GC, reference retention, history/churn and pending-unknown backpressure — mandatory later:** permanent records currently accumulate. Age alone must not justify deleting unknown evidence or ownership references; pre-GC samples do not satisfy the final storage budget.
19. **Manager/API/SDK/configuration wiring and native-only runtime state selection — mandatory later:** current etcd and management primitives are not proof the production call graph uses them end to end.
20. **Old-version drain and Redis/Sentinel runtime dependency removal — mandatory later:** the user explicitly requires removal; legacy code/packages still present are not considered migrated by these isolated foundations.
21. **Production etcd deployment mTLS/auth/RBAC, prefix/authority credential separation and operator bootstrap — mandatory later:** client-side TLS/identity validation was reviewed, but the loopback fixture and supplied configuration do not prove operational deployment isolation.
22. **Disaster restore fencing, unique restore epoch and old-writer/credential quarantine — mandatory later:** exact restore comparisons protect this client against changed metadata, but cannot independently isolate cloned old authority processes or credential access to external targets.
23. **Real 10k/100k/1M capacity, Pod/FUSE load, QPS, latency and failure-recovery SLO acceptance — mandatory later:** the 1,000 synthetic idle-point experiment and per-creation raw-value samples are structural evidence only.
24. **Multi-cell routing and cross-cell operation semantics — phase 6 later:** explicitly outside the user's presently authorized phases 1–5; no single-cell result establishes them.
25. **Malicious implementations of trusted Clock/TLS signer/callback dependencies — trusted dependency contract:** copies protect mutable owned data, while caller-supplied implementation behavior is required to be immutable, context-honoring and concurrency safe. This is not a sandbox for hostile in-process dependencies.
26. **General Byzantine or arbitrarily malformed native client/server envelopes in older generic Stage/client/domain paths — not claimed:** operation and replay boundaries received their specified defensive checks; the broader raw native etcd client contract is trusted. No concrete supported native-server response path causing a new failure was identified. This does not waive the strict handling already required and implemented for signed untrusted wires.
27. **Coordinated malicious rewriting of authoritative etcd records by an authorized raw writer — outside the application capability boundary:** coherent historical loaders are structural; producer mutation paths add crypto and original CAS fences. Preventing hostile authoritative writers requires the operational access controls in item 21, not a claim that checksums make the state store Byzantine-proof.
28. **Systematic early-Fatal worker joins across all older Stage/dispatch/preparation/publication test harnesses — not performed:** the inspected M1 original Operation harness is fixed with a regression. Broad modernization of prior harnesses remains a separate maintenance item, not new proof of runtime unsafety.
29. **Generic Stage comprehensive actual-Grant-boundary copy regression (prior M2) — nonblocking test strengthening retained:** source copies precede Grant and no alias defect was found; existing tests are narrower than that proposed matrix. Controller must retain or explicitly disposition this item.
30. **Dependency/image supply-chain and current vulnerability certification — not performed:** versions/digest pins and dependency changes were reviewed, but no online advisory or independent provenance audit was authorized for this read-only review.
31. **Unrelated Sentinel compatibility work and uncommitted controller plan edits — excluded from frozen range:** preserved without review or modification; their working-tree presence cannot be represented as part of this branch verdict.

## Verdict

**Ready for NEXT authorized implementation unit: Yes.**

The entire frozen branch integrates the new ongoing management identities with the existing birth/publication protocol without conflating identity with capability, and the reviewed native metadata chain retains its original fences, budgets and unknown-outcome behavior. No new confirmed Critical, Important or Minor defect blocks building the next authorized unit. This verdict is limited to continuation: phases 1–5 remain incomplete, all 31 items above require explicit individual controller disposition, and there is no permission or readiness conclusion for global migration completion, merge, deployment, production activation or real capacity acceptance.

Counts: **C=0 / I=0 / M=0 new; prior nonblocking M2 retained. Full-range coverage: 48 commits, 113 files, 21,797 lines, 1,132,281 bytes, read through EOF with truncation recovery. Evidence: full independent source/test review plus inspected controller log and report-backed native/pure-protocol executions; no tests rerun. Declined/set-aside behaviors: 31 individually listed.**
