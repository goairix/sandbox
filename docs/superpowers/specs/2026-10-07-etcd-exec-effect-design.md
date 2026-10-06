# 原始 operation 的 native exec effect intent

整体授权来自 etcd 单环境替换方案的 phases1–5；本单元是 issuer registry 闭环后的下一实现步骤。目标是把 original OperationCapability、可信 issuer/clock、ExecStartTicket 和永久 intent 连起来，产出私有 PreparedExecEffect。没有生产切换或物理执行；Manager、Redis/Sentinel 删除、target gate/accepted journal/terminal 等后续目标仍必须完成。

## 边界与取舍

采用现有 Stage 仲裁永久 intent，先改善它的实际 response/receipt contract，不复制另一套私有 Stage。未知结果只仲裁同一 reference；不能 remint command/claims/Stage。原始 operation Lease 是唯一请求生命期，Stage Lease 只是 metadata attempt，不能续接它。

不在 etcd 保存 argv/env/stdin/full descriptor；只保存有界 digest/ticket/original operation 和 attempt。creator crash 后只能查询／settle 原 command，不能从历史执行。需要持久排队 payload 的产品需求必须另有受控 payload store 和 fresh task authority。

先保留每 demand 的 registry Register 固定成本，不在本单元引入 cache。性能改进目标是避免 idle N 工作，不承诺 etcd 单次写比 Redis 快；下一 hot path 优化须基于 measured RPC/latency。没有 per-sandbox goroutine/timer/Lease/Watch/scan；活跃 payload 内存 O(C×(1MiB stdin+64KiB descriptor metadata))，入口并发限制和 GC/backpressure 后续必做。

## Task1 所提供的 Stage evidence contract

公共签名、wire、key layout、64operations/256KiB mutation budget 和 requested TTL (0,24h] 保持。复用 operationResponseHeader/NestedHeader、operationPutResponses、strictPreparationMetadata、immutableDispatchKV 等适当 helpers，避免整段复制旧协议。

Grant：先完成 builder/prepareMutation deep-copy/preflight。RPC error unknown；成功必须 response 非nil、ResponseHeader 非nil、matching cluster、positive revision/ID/TTL、Error empty。请求 cancel 后禁止 guard write。匹配header/positive ID 的 original grant 才能 bounded cleanup；foreign/nil/header0/无身份 claimed ID 不 revoke 任意 lease。TTL不能要求 <= requested；etcd server minimum 可提高短请求。这里不增加任意 returned TTL 上限，exec 的时间安全由原始 operation deadline+ticket保证。合法 native1s requested必须通过。Stage cleanup/Revoke 仍只表达 guard metadata cleanup，不声称 external terminal；本任务不扩大它的公共 proof contract。

Begin：Then exact1Put/no PrevKV/nested header一致；Else exact2 identity/restore Range，Count/More/kind/nested/key/revisions/permanent expected value 验证，false正常Conflict、identity变化IdentityMismatch；畸形未知、不返回Stage。guardRevision取原成功header revision。

Commit：Then exact business operations 顺序／kind/count，加1永久 receipt Put；single-key DeleteRange Deleted为0或1且无PrevKvs，nested header一致；无畸形 response 可以证明 committed。Else exact4 identity/restore/receipt/guard points；所有 envelope 验证再读取。合法 immutable receipt 决定已有 outcome；无receipt时只有完全匹配 original guard value/Lease/CRev、ModRev==CRev 的点才判 Conflict，否则 GuardExpired；这些都仍 OutcomeUnknown。未知 RPC不guess失败或panic。

Resolve：Then exact1 aborted Put；Else exact3 identity/restore/receipt points，同样严格。permanent receipt decode 必须 UTF8／maxRecordBytes、Lease0、CRev>0且MRev==CRev、key与ref namespace/partition/request/stage/attempt精确关联；strict required snake_case/unknown/duplicate/null/trailing，包括 embedded StageReference；version1、outcome committed或aborted、ref exact。既有 wire不变、existing absent read不是失败证据。不从 public reference 重建Stage。

所有 Then/Else native Txn 外层header必须positive revision/matching cluster；nested header可缺省（native），存在时 cluster0或matching+revision相同。point最多一个KV、Count==len、More false、keyexact、0<CRev<=MRev<=outerRev，meta values permanent且expected。畸形 errors 保持 errors.Is(ErrOutcomeUnknown/ErrIdentityMismatch/ErrCorruptReceipt 等)，outcome不能成功；不能把valid false fence错误吞成成功。

私有共享helper `(b *Backend).stageEvidencePoints(*clientv3.TxnResponse, []string) ([]*mvccpb.KeyValue,error)` 在stage_response.go：keys的前2项必须是identity/restore；只验证outer/全部point shape与identity，不把false Succeeded统一猜成identity错误，具体caller解释业务CAS；Task2固定point reader复用它。返回的KV仅调用栈内使用，公共entry再deepcopy。

包括此前非阻塞 M2 回归：actual Grant callback 改 caller Cmp.Key/RangeEnd/value oneof/write bytes，证明 preGrant copy 后的真实 digest/guard/committed key+value仍为原输入。不是只在 Begin返回后改数据。新 fault workers 要 release/cancel/bounded join(5s)先于 hook恢复／fixture清理。

## Task2 record、reference、历史读取

生产新文件 exec_effect_records.go／exec_effect_read.go／exec_effect_types.go；对应 tests。对外类型与签名：

```go
type ExecEffectRecord struct {
 Version uint32 `json:"version"`
 Operation OperationRecord `json:"operation"`
 AdmissionRevision int64 `json:"admission_revision"`
 CommandID string `json:"command_id"`
 DescriptorDigest string `json:"descriptor_digest"`
 TicketDigest string `json:"ticket_digest"`
 IssuerCertificateID string `json:"issuer_certificate_id"`
 IssuerCertificateDigest string `json:"issuer_certificate_digest"`
 IssuerRevision int64 `json:"issuer_revision"`
 Ticket json.RawMessage `json:"ticket"`
 Attempt StageAttemptLocator `json:"attempt"`
}
type ExecEffectReference struct {
 Operation OperationReference
 CommandID string
 Stage StageReference
}
type ExecEffectEntry struct {
 Reference ExecEffectReference
 Outcome Outcome
 Record *ExecEffectRecord
 Revision int64
}
func (r ExecEffectRecord) Validate() error
func (r ExecEffectReference) Validate() error
func (b *Backend) LoadExecEffect(context.Context, ExecEffectReference) (*ExecEffectEntry, error)
```

Record Version1、Operation合法且OperationData／其 encoded<=4096、admission/issuer revisions正、command/cert UUIDcanonical非nil、3hash lowerhex64；ticket合法UTF8 nonnull JSON<=4096且snapshotDigest==TicketDigest；整个encoded<=16384。Attempt合法且nonNilUUID／Namespace/partition/restore/request与Operation.Reference一致，StageID固定 operation_exec_start。Ticket是 opaque structure，历史codec不fresh verify；新 producer只存Verify得到的canonical wire。

Reference Validate 要合法data OperationReference、commandUUIDnonNil、Stage完整合法/nonNilAttemptID/lowerhexDigest，Stage locator身份同上。本 producer 在 Stage 未建立前可以返回部分诊断 reference，Validate拒绝且Load不能使用；任何Stage RPC发送前必须已保存完整reference。

private Namespace.execEffectKey(OperationReference) 产生 p/<02x partition>/intents/<OperationID>/exec-start，origin namespace/restore匹配由Backend确认。private encodeExecEffectRecord(ExecEffectRecord)(string,error)／decodeExecEffectRecord(*mvccpb.KeyValue,*ExecEffectRecord)error，strict所有typedfields/null/duplicate/unknown/trailing、first revision／Lease0／key exact、bytes copy／dst原子替换；encoder不额外HTMLescape改变ticket digest。

Load一次固定 identity-fenced snapshot读 identity/restore/effect/receipt/issuer：issuer ID需从初次 effect point发现，故允许第一次 identity+restore+effect、第二次 coherent5points。初次effect absent返回nil,nil只是snapshot；不能作为迟到effect已失败。若有record取exactissuerkey+给定Stage receipt key；复核全部point/headers/identity/restore。committed需要exact reference+record.Attempt关联、receipt outcome committed、record与receipt同firstrev、registry permanent original IssuerRevision/ID/digest/namespace/restore关联。没有record但初次absent不得另行重建authority；Load历史只在已有record路径读receipt，aborted诊断由ResolveStage处理而非伪造record。record存在但receipt/issuer缺失／错revision属于CorruptRecord/CorruptReceipt，始终无cap。Record/Entry copied，过期ticket仍可历史查询且不调用clock/provider/sign/Grant。无range/watch/cache。

## Task3 producer 与私有 original-cap draft

```go
type PrepareExecEffectResult struct {
 Outcome Outcome
 Reference ExecEffectReference
 Prepared *PreparedExecEffect
 GuardCleanupError error
}
type PreparedExecEffect struct { /* private fields only */ }
func (p *PreparedExecEffect) Reference() ExecEffectReference
func (b *Backend) PrepareExecEffect(context.Context, *OperationCapability, controlprotocol.ExecutionRequest) (PrepareExecEffectResult,error)
```

Prepared 只留原Backend/cap/private draft，没有wire/payload public getter，Reference只有诊断历史。OperationCapability新增 private execDraft *execEffectDraft，受已有 c.mu保护；保持 Reference/Control复制、Renew/Cancel语义。不允许 exported struct fields/private signer/key在Backend。

ctx非nil/未取消、same origin cap、original parent非nil、OperationData、admissionLive，配置execIssuer/verifier/authorityClock/publicationVerifier合法。NewExecutionDescriptor(request)自身copy和Validate；相同descriptor重试同draft，不同digestErrConflict。所有RPC/provider调用上下文同时受 caller、original parent和original monotonic old deadline约束；parent AfterFunc停止，无 idle N常驻worker。mutex等待后再次检查caller/live；不引入轮询锁或扩展旧deadline。

首次draft：bounded RegisterExecIssuer，fresh Observe＋Verify该entry exactcertificate（不采纳别的rotation identity）；保存original key/value/ModRevision/Lease0。生成一次UUID CommandID，full ExecStartContext从私有原OperationRecord、admitRevision、Backend pinned binding、该issuer ID/digest产生；Runtime显式类型转换已有controlprotocol.RuntimeReference。不从 untrusted ticket生成expected。

Clock返回U后立即采样monotonic m，D为原deadline，Δ=1s；NB=max(U-2Δ,issuerNB)，NA=min(U-Δ+(D-m),NB+30s,businessE,issuerNA)。验证有保守窗口（U-Δ>=NB且U+Δ<NA）再保存固定claims Version1/purpose operation_exec_start/context/descriptorDigest/NB/NA。NB/NA/CommandID永不因Sign/Stage unknown或Renew改变；短剩余Lease拒绝、通常约28s nominal有效窗口，是明确可用性代价。

SignStart bounded exactdigest／fixedclaims，copy结果；fresh Observe＋VerifyExecStartTicket full original expected+actualdescriptor，canonical verified Wire/Digest保存。provider失败允许下次仍sign同claims/digest；成功ticket必须缓存，不再次sign。返回错误不隐式revoke originaloperation；original parent/deadline/lost不能复活。

Stage builder一次生成record+locator。先计算 prepareMutation 得digest，并保存full StageReference与exactexpected record body，任何 Grant前完整ref已赋值。mutation comparisons 10originalcontrol(ModRev+Lease0)+9guard/token/receipt(value/Lease/CRev)+3issuer(value/ModRev/Lease0)+1effectabsent，write1，加Stage reserved14=38。admission receipt encoded original reference+committed。不能比较 public reread后新rev或重新grant operationLease。

Begin成功保存原Stage；unknown有ref但无Stage，重试只能Resolve同ref，不重新Begin／新Lease。Commit调用originalStage；unknown保留所有原state，可重试sameStage或Resolve同ref，不能remint。known aborted锁定draft，返回Aborted nilPrepared，不在同cap生成新attempt/command；未知或creator crash历史不执行。Begin失败cleanup通过stageBeginFailure分离 GuardCleanupError；任何 cleanup不覆盖真实metadataOutcome。

knowncommitted后用LoadExecEffect验证exactexpected原record/ticket/body/receipt/issuer原rev；freshClock＋ticketVerify，originallive，再同Txn验证全部original23+issuer3 fences（base4+control10+op9+issuer3=26），两branch含defaultlinearizable identityGet，使用existing validateFenceCheckResponse；falseConflict且无Prepared。最后再次local live、ticket窗口过时拒绝（可freshObserve再次check，禁止systemUTC替代）。OutcomeCommitted保留即使后check失败，Prepared=nil+error；不能声称metadataAbort。成功只返opaquePrepared；未来真实transport必须重新check原fences/time/targetgate/acceptedjournal，这里没有startwire出口或物理call。

第一轮已知commit与重试每次都重新进行postcommit验证；不得因cachedPrepared跳过live/fresh/fences。成功结果可以留draft Prepared引用但返回前必须重新授权；reference/recordentry永不恢复cap。

## 验证、成本与剩余范围

Task1 actual nil/malformed native after-RPC hooks、真实committed-replyloss/aborted/delayed arbitration、native小TTL兼容，M2 actualGrantcopy；TDD实行为RED，不用编译错。Task2 strictcodec/matching/key/copy/bounds+actualnative fixedpoint/load afterticketexpired零clock/provider/Grant。Task3 slow Observe/signer/Stage、sign wrongpayload/cert/context、rotated provider exactdigest、shortremaining/olddeadline/parentloss、Renew不remint、unknownretry sameCommand/claims/Stage、actualservercommit-replyloss/abort迟到CAS、fence/issuercoherent recreate拒绝；无Prepared在所有失败／unknown/history路径。worker bounded join和focusedrace。

每任务自审验证即小commit，Task3可将coherent producer slices分别commit，不留大提交。independent spec/quality gates，然后一次本unit final review与fresh完整native/allrepo/vet/build。准确记录Grant/Txn/points/Put/KA/Revoke，actual descriptor/ticket/record bytes，以及0与1000syntheticidle records固定成本比较；不是100k实际SLO。marker/intent原始rev安全GC与unknown背压仍后续；每exec新增StageLease/registryRPC开销不能掩盖。

当前baseline b99→0e2完整whole review以及0e2→671 scoped review已永久保存。后续finalreview接收fullbranch context与本unit全delta，实际读所有新source和受影响integration边界，明确哪些历史未重复读；不虚报fresh全历史EOF。整体生产callgraph/真实容量/phases1–5验收仍要求全局终审。

## 不豁免的后续

target UID/BootID possession/bootstrap、rootPID1/managementcredentials/UID/cap/proc/FD isolation、activeissuer安装/rotation/revoke、accepted-only续期与Begin/End/stream/descendant终态、closed gate/exclusive/reopen、远端写settled/fence、task/pool/TTL/destroy/ownerSafeRelease、scheduler watch R+1/shared renewal/boundedqueue、collector/hungFUSE、safeGC、Manager/API SDK/config nativeonly、Redis/Sentinel删除、restore/RBAC/deployment与10k/100k/1M容量。多cell仍phase6，不在本单元做。

## 自审

Stage增加strict证据不改变业务wire和原TTL最低值兼容；历史record不加crypto/clock/cap；producer只使用私有原状态且meta未知不执行。Task2 publichistory只处理已有record，aborted沿用Stage resolver避免inventedemptyrecord。ticket+originalOperation bounded16KiB，小于Stage64KiB；没有把64KiB descriptor全存etcd。three task生产/消费签名一致，所有未实现physical/runtime项显式保留。依赖pure协议/registry已闭环，不停在artifact审批；当前单元不会外部发布。
