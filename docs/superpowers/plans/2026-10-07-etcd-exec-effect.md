# Native exec effect intent Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 将 original OperationData capability、root-authenticated issuer和有界startticket接到永久effect intent，失败／unknown／history不能恢复执行能力。

**Architecture:** Task1强化实际Stage证据producer并补M2；Task2 strict有界record+metadata history；Task3 originalcap下sign/Stage/postchecks生成opaquePrepared，无物理transport。每task独立验证即小commit、独立spec+qualitygate；本合并unit最终一次integrationreview与全gates。

**Tech Stack:** 现有 Go／controlprotocol／clientv3／testify／owned etcd3.6.15 fixture，无新dependency。

**Spec:** docs/superpowers/specs/2026-10-07-etcd-exec-effect-design.md；上单元永久finalreview/verification为baseline证据，source67129ab、docs3c7905d。

## Global Constraints

- 所有public签名/typedfields/values以spec为权威；Version1／purpose或StageID operation_exec_start、Δ1s、start≤30s、operation原Lease30s、record16384B／ticket4096B／originalOperation4096B，无argv/env/stdin/full descriptor持久化。
- 不改Stage wire/key/requestTTL(0,24h]／64ops／256KiB mutation；返回nativeTTL可高于request，不加错误的exactTTL/upperbound。Stage cleanup不是physicalterminal；knownmatchingheader/positiveoriginalID才cleanup，不adopt其他Lease。
- originalcap sameBackend/parent/lost/mu/monoDeadline/originalcontrol+guard/token/receipt/admissionRevision；dataonly，public历史不重建cap。一个cap固定一个command/claims/ticket/issuer原value+rev；unknown只原reference，Renew不remint start。
- opaquePrepared无public wire/payload getters，所有actualRPC/provider受caller+parent+oldmonoDeadline；每阻塞步骤后live/fresh检查，fulltrustedexpected不来自untrustedticket；OutcomeCommitted即使postchecksfail也保留、Preparednil。
- Stage effect预算38；postFence26cmp且两branch含defaultlinearizable identityGet，不用emptyserializableTxn；idle N不增加任何watch/timer/Lease/goroutine/scan。
- 本unit不物理执行，不接Manager／config，不改image，不移除尚在用的Redis，不push／merge／deploy；整体phases1–5仍必需完成。Sentinel无关tracked编辑不读写stage。只controller-ownedfreshfixture，worker release/cancel/5sjoin先于hookrestore/cleanup。

## Review Focus

1. native成功回复nil/wrongkind/count/header或receipt畸形不能变为commit/abort/Prepared（Task1 evidencefault/strictreceipt）。
2. callerCmp/write在actualGrant callback改变不能影响原已固定mutation（Task1 M2 actualboundary）。
3. historicalentry/ticket过期、原rev被coherent重建不能重新产生authority（Task2 loader/noCalls，Task3 originalissuer/fence recreation）。
4. Observe/signer/Stage迟到、parent取消/olddeadline失效、Renew成功不能延长既有ticket（Task3 deadline/nativeunknown）。
5. nativecommittedreplyloss/abort与迟到CAS竞争不能换command/Stage或重写业务，失败/unknown不得Prepared（Task3 actualnativefaultsameidentity）。

## Task 1: strict Stage response and receipt evidence

**Files:** Modify internal/storage/state/etcd/stage.go、receipt.go、stage_test.go（仅兼容／M2必要）；Create stage_response.go、stage_response_test.go、stage_response_fault_test.go（纯receipt测试可新建receipt_test.go）。不改其他metadata producer/records、operation/publication/registrycode。

**Interfaces:** 公共BeginStage/CommitStage/ResolveStage/ReleaseStage保持；产生private `(b *Backend).stageEvidencePoints(*clientv3.TxnResponse, []string)([]*mvccpb.KeyValue,error)`（keys前2identity/restore、validated native envelope/identity、不猜falseCAS），Task2消费。decodeReceipt已有签名不变。消费existingoperationHeader/Nested/Putresponses、strictPreparationMetadata/immutableDispatchKV。完整Stage contract exact为spec“Task1 所提供的 Stage evidence contract”，controller须把该节拷贝到brief/context，worker不读整个plan。

- [ ] Write TestStageResponseEvidence／TestStageReceiptStrict：nativehook覆盖nilGrant、ID0、TTL0、Error、foreign/nil/headerrev0；nil/wrongheader/kind/cardinality/PrevKV/nested/Count/More/keys/revisions Then/Else；Commit Delete0合法（positive nested outer或outer-1，native leading与after-Put分别覆盖）、Delete>1/negative／PrevKvs非法；receipt leased/mutated/wrongkey/missingzeroPartition/duplicate/null/trailing/unknown/invalidUTF8/version/ref/outcome，dst不得影响outcome。使用actualnativeCommit再破坏返回reply，真实committed仍Unknown直到exactreceipt仲裁，不能虚构KV代替CAS。新worker须boundedjoin。
- [ ] Write TestStageMutationCopiedDuringGrant，用nativeLease wrapper actualGrant callback改变 caller Cmp.Key／RangeEnd／value oneof／write bytes；验证originaldigest/guard和实际committed originalkey/value。native1s requestedTTL实际返回positive可高于1仍允许Begin/Commit/Release；报告真实TTL。旧existingStage arbitration/replyloss/leader/NOSPACE契约不变。
- [ ] controller供freshownedfixture，focused `go test ./internal/storage/state/etcd -run '^(TestStageResponseEvidence|TestStageReceiptStrict|TestStageMutationCopiedDuringGrant)$' -count=1` 实行为RED（新API stub允许编译，但不以编译错当RED），实现specexact必要helper与sharedStage checks，不改变outcome/cleanup分离。
- [ ] focusedGREEN+同selector-race、既有Stage happy/replay/abort/claim siblingUnknownCleanup相关测试一次（精确selector由现有test名列出，不重复全faultsuite），pkgvet/gofmt/diff、自审；按coherent response/receipt/copy片段验证即小commit，不能堆最终大提交。完整actualreport，独立spec+qualitygate后Task2。

## Task 2: strict bounded exec effect metadata and historical loader

**Files:** Create internal/storage/state/etcd/exec_effect_types.go、exec_effect_records.go、exec_effect_read.go、exec_effect_records_test.go、exec_effect_read_test.go。No producer/no OperationCapability edits。

**Interfaces:** exactspec ExecEffectRecord／ExecEffectReference／ExecEffectEntry+Validate、Backend.LoadExecEffect、Namespace.execEffectKey、encode/decodeExecEffectRecord；消费Task1 stageEvidencePoints／decodeReceipt、existing operation/schema/registrycodec helpers。复制spec“Task2 record、reference、历史读取”到taskcontext供完整exact字段与规则。Prepared/result类型留Task3。

- [ ] TDD TestExecEffectRecord/Reference：strictnested/allrequired/duplicate/nullmissingpartition/unknown/trailing/UTF8/ticketdigest/UUIDnil/rev/key/firstrev/lease，OperationData only，4096/16384预算、copy/dst原子替换、不HTMLescape改变rawticket；construct真实合法完整最长context，报告encodedworstcase不是仅typicalsample。
- [ ] TDD native TestLoadExecEffect：initial3points→coherent5points、record/receipt同CRev、exactoriginalissuerrev/digest/ref关联、history过期仍读且0clock/provider/sign/Grant/KA/Revoke、copy、absentnilnil仅snapshot、malformednativeHeader/points、missing/corruptreceipt/issuer、同certbody删除重建rev不adopt。publichistory任何路径无Prepared。
- [ ] actualbehaviorRED后实现唯一boundedpoint流程、codec/types。focusGREEN+race/pkgvet/gofmt/diff/selfreview即smallcommit；准确RPC/point／recordsize报告，独立spec+qualitygate后Task3。

## Task 3: original-capability exec intent producer

**Files:** Modify internal/storage/state/etcd/operation_admission.go（仅privateexecDraft字段）；Create exec_effect.go、exec_effect_validation.go、exec_effect_test.go、exec_effect_fault_test.go、exec_effect_capacity_test.go；Modify exec_effect_types.go加入exactspecresult/Prepared。若helpers需拆文件仅同责任新exec_effect_*.go允许；不改现有Renew/Cancel或protocol签名，不加cache/providerstartup/productionwiring。

**Interfaces:** exactspecPrepareExecEffect／PrepareExecEffectResult／opaquePrepared.Reference；privateexecEffectDraft字段由specbookkeeping决定，消费Task2/Stage/operatorprovider/verifier/Clock。完整spec“Task3 producer 与私有 original-cap draft”+时间／retry／预算／error语义拷贝taskcontext。signed全context由originalprivateOperation构造。

- [ ] Write TestPrepareExecEffect：actualconfiguredNew startup0calls；samepayloadretry同command/claims/ticket/Stage、differentpayloadConflict、mutation/foreign/publicfakecap拒绝、actualdescriptorcopy、providerSign exactdigest、wrongcert/payload/context/root/purpose/time/signaturenilPrepared；draft ticket不因Renew改变且cachedPrepared每次live/fresh/fence重验。
- [ ] Write TestPrepareExecEffectDeadline：delayObserve postm anchor／shortremaining≤2Δ拒绝；delaysigner／Stage过原oldD、parentcancel/lost、clockuncertaintyfailure／postcommitfreshfail =>真实Committed+nilPrepared；boundedprovidercontext、AfterFuncstop，无墙钟fallback，originalLease不regrant。
- [ ] Write actualnative TestPrepareExecEffectUnknown：Commit已提交replylost→retry原Stage确认同recordrev/command；Beginreplylost→保存同ref/Resolve无新Grant；resolverabort vsbeforeServer迟到完整CAS at most one，unknownabsent不失败；control/origguard/token/receipt/issuer value/Lease/CRev/recreation拒绝，不从read恢复cap。所有hooks child release/cancel/join5s，earlyFatal regression。
- [ ] actualbehaviorRED后implement fixed one-shotdraft/clockmath/signverify/recordbuilder+38Stagebudget/knowncommitpostload+26linearFence+live；不写physicaltransport，不exportwire，无新Lease替代operation。先coherentvalidation/producer片段focusGREEN、自审即smallcommits，然后下一片，完整报告仅本taskstage。
- [ ] focusGREEN+native race/pkgvet/gofmt/diff、TestExecEffectFixedCost（0/1000syntheticidle）准确Grant/Txn/points/Put/KA/Revoke/budgets/record/ticket/descriptorbytes；no actualN benchmarkclaim。独立gate后controller最终sourcefreeze、freshnative完整／全仓test/vet/build，fullbranchcontext+全newdelta integrationreview，ONEfinalfixwave/scoped复核若需要，逐项残留/rulings永久保存；完成这个中间单元后继续actualtarget/physical/lifecycle/production迁移。

## 自审

| producer/consumer或task | 约束／测试的一致性 |
| --- | --- |
| 1→2 | stageEvidencePoints+strictreceipt只证metadata；Task2固定点复用，不产生cap。 |
| 2→3 | record/ref/key/codec/historyexactinterfaces供producer，record16KiB内保存4KiBticket+4KiBoperation；StageRef不在record内digest自引用。 |
| Task1 | publicwire/TTL/key保持；native短TTL与actualGrantcopy涵盖合法兼容；header/point/receipt tests确证新增拒绝路径；Delete0只允许native有据的positive outer-1例外，Put/Delete1/Range规则不变。 |
| Task2 | 只历史无需clock/provider；initialabsent不制造abort，aborted只ResolveStage，coherentrecreate检查原IssuerRevision。 |
| Task3 | capprivatefield与同mu，固定ticket时间来自oldD，unknown不换身份；zero-Prepared和真实Committed错后验同时断言。 |

所有ReviewFocus各有具体task测试。正式参数/字段与spec一致，nexttarget权威尚未交付。无待用户回答的选择或外部副作用，沿已授权连续实施。
