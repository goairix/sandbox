# etcd authenticated runtime publication 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 只有来自登记 runtime authority 的、绑定原 dispatch 和当前 claim 的 target 证据，才可原子发布 exact runtime 为 active。

**Architecture:** cell issuer 对每个 runtime incarnation 的公开签名密钥签发委托证书；target 用独立私钥签 ready/gate/mount 观察回执。repository 仅配置公开 trust roots 和受控 clock，在原 claim CAS 下将 先以原 claim 固定 runtime/index 并消费一次 mount 决策，再将 owner/control/request/intent、immutable publication/proof 与 committed receipt 组合提交。私钥不进入 etcd，证据认证不代替后续 launcher 的物理执行实现。

**Tech Stack:** Go1.25 标准库 Ed25519/SHA256/JSON，现有 uuid/client3.6.14/server3.6.15，无新增依赖。

**Spec:** `docs/superpowers/specs/2026-10-06-etcd-state-management-design.md` 创建发布、受控 UTC、target 准入与可信 launcher 章节；前置 dispatch 已实现于 `e3579dc`。

## Global Constraints

- 分支 `codex/etcd-state-management`；保留既有 Sentinel 计划修改。每单元真实验证、自审即提交，独立审查修复另提交，不切生产、不调用 runtime、不写 Secret/外部环境。
- 证据只能由预先配置的 authority roots 认证；Publish 参数不能提供自己的 root/verifier 或 caller bool。issuer private key 仅在未来可信签发方，runtime 仅获其独立委托私钥，用户进程不得访问；本批提供签名协议，不伪称可信 launcher 已实现。
- namespace/authority/target/restore、workspace/intent/sandbox/generation、原 dispatch operation/input digest、snapshot/expiry、exact ID/UID/BootID 都要绑定。ready 回执还绑定当前 claim UUID/CreateRevision/LeaseID 与 data gate epoch；旧 claim/boot/gate/restore 证据拒绝。
- 业务到期采用受控 UTC，Δ=1秒；clock未知、误差+取样耗时超过1秒、`now+Δ>=expires_at` 均拒绝。事务前重检，本批不承诺客户端检查与服务端 wall time 原子等价；真正执行还必须由 target 重新检查期限。
- 证书最大4096 bytes、完整 ready proof最大8192 bytes、持久 proof record最大16384 bytes、binding certificate record最大8192 bytes；其他新记录4096。严格UTF8/字段/重复/null/trailing JSON与正数/UUID/hash/UTC，拒绝 malformed key/signature 长度导致panic。复制所有输入、root/key/wire/输出切片。
- 所有域记录永久Lease0、namespace key<=1024、root<=512。原claim24cmp不变，Stage预留14与总64不放宽。仅固定point keys，不按N scan，不新增perN goroutine/Lease。unknown靠exact receipt仲裁，不以missing判断target失败。
- Signed ready/gate/mount 断言成立依赖后续可信 producer 从实际durable target状态生成；不能把通过密码学测试表述为物理gate/mount已验证。FUSE mount须在实际发送前消费一次持久attempt，后续实现不得从本批Publish跳过这一准备步骤。

## Review Focus

1. 未登记root、自签root、跨namespace/restore/authority/target证据不能发布；签名只证明来源，不能转化为普通API caller的完成断言。
2. 有效签名的旧claim、旧BootID/gate、错dispatch/input/snapshot/expiry不能复用；context由private claim和已提交dispatch派生。
3. 回复丢失、resolver先abort、runtime index争用不能半发布；未知保留owner，不另runtime或operation。
4. caller/root/wire/返回对象alias不能污染证据；非法wire、未知clock和预算在Grant前拒绝。
5. 两个intent同UID不同Boot不能同时绑定，index按UID防重复；已提交后replay/readonly恢复不授予新执行能力，也不刷新期限。

## Task 1：委托证书与 signed ready proof

**Files:** Create `internal/runtime/controlprotocol/publication_types.go`、`publication_codec.go`、`publication_sign.go`、`publication_verify.go`及按职责tests；不改etcd客户端。

**Interfaces:**

```go
type RuntimeReference struct { ID,UID,BootID string }
type SnapshotReference struct { Version,Digest string }
type ClaimReference struct { ClaimID string; CreateRevision,LeaseID int64 }
type CertificateContext struct {
    Namespace,AuthorityID,Target,RestoreEpoch string
    IntentID,SandboxID,WorkspaceHash string; Generation int64
    OperationID,PayloadDigest string; Snapshot SnapshotReference; ExpiresAt time.Time
}
type PublicationContext struct {
    Certificate CertificateContext; Runtime RuntimeReference; CertificateDigest string
    Claim ClaimReference; DataGateEpoch int64; MountAttempt uint8; MountOperationID string
}
type TrustBinding struct { Namespace,AuthorityID,Target,RestoreEpoch string }
type RuntimeCertificateClaims struct {
    Version uint32; RootKeyID string
    Namespace,AuthorityID,Target,RestoreEpoch,IntentID,SandboxID,WorkspaceHash string
    Generation int64; OperationID,PayloadDigest string
    Snapshot SnapshotReference; ExpiresAt time.Time; Runtime RuntimeReference
    WorkspaceMode string // only "plain" or "fuse", certified from immutable snapshot by trusted issuer
    PublicKey []byte; NotBefore,NotAfter time.Time
}
type ReadyReceiptClaims struct {
    Version uint32; CertificateDigest string; Claim ClaimReference
    DataGateEpoch int64; GateState string // only "open"
    MountAttempt uint8; MountOperationID string
    ObservedAt,ValidUntil time.Time
}
type PublicationVerifier // immutable private roots/binding
type PublicationEvidence // private verified values and normalized private wire
type CertificateEvidence // private verified certificate and normalized private wire
func NewPublicationVerifier(TrustBinding,[]ed25519.PublicKey)(*PublicationVerifier,error)
func SignRuntimeCertificate(ed25519.PrivateKey,RuntimeCertificateClaims)([]byte,error)
func SignReadyReceipt(ed25519.PrivateKey,certificate []byte,ReadyReceiptClaims)([]byte,error)
func (*PublicationVerifier) VerifyCertificate([]byte,CertificateContext,time.Time)(CertificateEvidence,error)
func (*PublicationVerifier) VerifyHistoricalCertificate([]byte,CertificateContext)(CertificateEvidence,error)
func (*PublicationVerifier) Verify([]byte,PublicationContext,time.Time)(PublicationEvidence,error)
func (*PublicationVerifier) VerifyHistorical([]byte,PublicationContext)(PublicationEvidence,error)
func (PublicationEvidence) Runtime() RuntimeReference
func (PublicationEvidence) MountAttempt() uint8
func (PublicationEvidence) Wire() []byte
func (PublicationEvidence) Digest() string
func (CertificateEvidence) Runtime() RuntimeReference
func (CertificateEvidence) WorkspaceMode() string
func (CertificateEvidence) Wire() []byte
func (CertificateEvidence) Digest() string
```

所有wire字段显式snake_case。certificate envelope=`{claims:<typed claims>,signature:<base64>}`；ready proof=`{certificate:<certificate envelope>,claims:<ready claims>,signature:<base64>}`。签名字节为固定前缀`"sandbox-runtime-certificate:v1\x00"`或`"sandbox-runtime-ready:v1\x00"`加typed claims的确定性JSON；Ready claim绑定SHA256(canonical certificate envelope)。rootKeyID=SHA256(root public key)，签证书时内部填充，不能由caller冒充。roots允许1–2个，复制、去重；常规轮换需未来受控部署/restore契约，不在请求中信任新root。

typed schema规范化再签/验，codec严格拒绝nested unknown/duplicate/null/missing/trailing和非法UTF8，不用float解码，不引reflect；可用静态字段集合token parser。签名方法先validate，priv长度64且公开suffix与seed派生结果一致，delegate私钥公开部分须等于certificate.PublicKey；验证先检查pub32/sig64避免stdlib panic。Wire返回规范化独立copy，Digest对该wire SHA256。

证书CertificateContext字段与expected精确匹配，certRuntime三字段有效opaque<=128；certificate不绑定当前claim，允许新claim查询旧runtime。NotBefore<NotAfter<=ExpiresAt且均UTC，VerifyCertificate要求`now-1s>=NotBefore`与`now+1s<NotAfter`及业务到期。ready还必须绑定expected.Runtime和CertificateDigest，当前claim/gate精确匹配；mount只能0或1，0必须无MountOperationID且mode=plain，1必须canonicalUUID且mode=fuse，必须等于expected消费的attempt/op。cert.NotBefore<=ObservedAt<ValidUntil<=cert.NotAfter且窗口<=5s。Verify要求ObservedAt不晚于`now+2s`、`now+2s-ObservedAt<=5s`、`now+1s<ValidUntil`和业务expiry检查；保守边界拒绝，不延长旧证明。Historical仍验证全部签名/schema/context与证书内部/ready窗口，忽略当前时间的新授权检查，只返回只读证据。

- [x] 编写真实签名assertion RED→GREEN：登记root及delegate有效，篡改/另root/越界context/旧claim/gate/错摘要/ref/expiry拒绝；证书和proof时间边界、expired历史只读可读而Verify拒绝。
- [x] 覆盖nested unknown/duplicate/null/missing、UTF8/trailing、wire上限、错误key/sign长度无panic、root/priv/certificate/wire/output mutation隔离，区别签名认证与物理断言来源。
- [x] 定向race/vet/gofmt/diff、自审后立即提交，再spec→quality独立审查。

## Task 2：先固定 exact runtime 与一次 mount intent

**Files:** Create `runtime_preparation_records.go`、`runtime_preparation_read.go`、`runtime_preparation.go`、`publication_authority.go`及对应tests；Modify `domain_records.go`封闭codec新增四种记录、`client.go`仅扩展Options/Backend私有authority与clock依赖。

**Interfaces:**

```go
type ClockObservation struct { UTC time.Time; Uncertainty time.Duration }
type AuthorityClock interface { Observe(context.Context)(ClockObservation,error) }
type RuntimePublicationTrust struct { AuthorityID,Target string; Roots []ed25519.PublicKey }
// Options.PublicationTrust *RuntimePublicationTrust; Options.Clock AuthorityClock
type RuntimeBindingRecord struct {
    Version uint32; IntentID,SandboxID,WorkspaceHash,RestoreEpoch string
    Generation int64; Snapshot SnapshotReference; ExpiresAt time.Time
    OperationID,PayloadDigest,CertificateDigest,WorkspaceMode string; Runtime RuntimeReference
    Claim DispatchClaimReference; Attempt StageAttemptLocator
}
type RuntimeBindingCertificateRecord struct {
    Version uint32; IntentID,SandboxID,WorkspaceHash,RestoreEpoch,CertificateDigest string
    Generation int64; Payload json.RawMessage
}
type RuntimeIndexRecord struct {
    Version uint32; IntentID,SandboxID,WorkspaceHash,RestoreEpoch string
    Generation int64; Runtime RuntimeReference
}
type RuntimeMountIntentRecord struct {
    Version uint32; IntentID,SandboxID,WorkspaceHash,RestoreEpoch string
    Generation int64; Runtime RuntimeReference; CertificateDigest,DispatchOperationID,OperationID,WorkspaceMode string
    MountAttempt uint8; Claim DispatchClaimReference; Attempt StageAttemptLocator
}
type RuntimeBindingEntry struct { Record RuntimeBindingRecord; Certificate json.RawMessage; Reference StageReference }
type RuntimeMountIntentEntry struct { Record RuntimeMountIntentRecord; Reference StageReference }
type PreparationResult struct { Outcome Outcome; Reference StageReference; Binding *RuntimeBindingEntry; Mount *RuntimeMountIntentEntry; Replay bool; GuardCleanupError error }
func (*Backend) BindRuntime(context.Context,*CreationClaim,[]byte)(PreparationResult,error)
func (*Backend) ConsumeRuntimeMount(context.Context,*CreationClaim)(PreparationResult,error)
func (*Backend) LoadRuntimeBinding(context.Context,WorkspaceIdentity,string)(*RuntimeBindingEntry,error)
func (*Backend) LoadRuntimeMountIntent(context.Context,WorkspaceIdentity,string)(*RuntimeMountIntentEntry,error)
func (Namespace) runtimeBindingKeys(uint8,string)(binding,certificate string,err error)
func (Namespace) runtimeMountIntentKey(uint8,string)(string,error)
func (Namespace) runtimeIndexKey(uid string)(string,error)
```

Options允许trust/clock均nil来继续metadata foundation，但Bind/Consume/Publish fail closed；只配置一个拒绝New。trustedClock是constructor依赖，必须不可变引用并发安全，Observe契约返回该调用期间的受控UTC/已验证误差，不是caller提供Now。trust.AuthorityID等于Identity.RuntimeID，target合法opaque<=128，roots复制；constructor内部NewVerifier绑定namespace/restore，绝不system wall clock fallback。clock取样以monotone衡量调用耗时，Uncertainty>=0且加耗时<=1s，UTC有效；保守now=UTC+耗时，使用Δ检查期限。

binding/cert=`intentKey/runtime-binding`、`intentKey/runtime-certificate`，mount=`intentKey/runtime-mount-intent`，stages分别`runtime_bind`与`runtime_mount`。index=root/indexes/runtime/SHA256(framed `"runtime-uid:v1"`,uid)，不含BootID以防同UID另boot双绑定。binding/cert/index/receipt同首次revision并永久；cert完整record<=8192，其余4096。mount intent与其receipt永久同首次revision。固定point loader两次读取定位和同Txn重验exactreceipt；Bind的最终bundle必须在同一只读Txn取得dispatch三条和binding/cert/index/receipt四条，Consume与Publish同样使用一次固定point读取取得将参与CAS的全部record，不能把不同快照拼成可写bundle。公共loader只返回证据，内部loader同时返回验证后的exact KV供ModRevision/Lease CAS。严格schema/归属/hash/context/revision/lease，copy wire；不能以missing确认target失败。

Bind先以private claim和已提交dispatch派生CertificateContext，clock与VerifyCertificate认证，固定原Runtime/UID/BootID/Mode/CertificateDigest。已有binding只允许原规范证书/上下文readonly replay，不覆盖、更换boot或刷新期限；不同认证身份冲突。新Bind原claim24+dispatch三个不可变record的ModRevision/Lease0六条+binding/cert/index三absence，写三条+Stage预留14，共50。原owner/control仍provisional，domain24不改变；UID index作为永久预留，未发布也不能当成free。unknown保留exactref，不清owner/index。

Consume重新核验dispatch及binding/cert/index/receipt的同首次revision、signature/context和clock；固定一个永久mount intent。mode=fuse只消费MountAttempt=1和内部新OperationID，在实际mount发送前必须成功；mode=plain写明确MountAttempt=0/no-mount决策（内部OperationID仍UUID，ready证明MountOperationID必须空），不从absence猜。原claim24+dispatch6+binding四record八条+mount absence一条+write一条+预留14，共54。已有intent返回原operation/creator，不换ID或重新mount；元数据本身不授权外部调用。stage30s，Begin后ctx/live/clock重检，cleanup只原Stage；回执unknown不能当abort。

- [ ] 真实signed binding/index原子RED→GREEN；两个workspace同UID不同Boot只一个预留；新claim恢复旧binding，不换Runtime/cert/expiry，invalid root/clock/sign/context beforeGrant拒绝。
- [ ] 先消费FUSE mount=1、重放保持原operation，plain显式no-mount=0；旧binding/boot/restore/claim/control变化或wrong receipt拒绝，unknown/lost reply/delayed完整Txn仲裁无半套，caller/output/root copies。
- [ ] 真实定向race/vet/gofmt/diff、自审即独立提交，再spec→quality审查。不发送runtime/mount调用。

## Task 3：永久发布journal/proof与历史恢复

**Files:** Create `internal/storage/state/etcd/runtime_publication_records.go`、`runtime_publication_read.go`及tests；Modify `domain_records.go`封闭codec新增两种记录。

**Interfaces:**

```go
type RuntimePublicationRecord struct {
    Version uint32; IntentID,SandboxID,WorkspaceHash,RestoreEpoch string
    Generation,DataGateEpoch int64; Snapshot SnapshotReference; ExpiresAt time.Time
    OperationID,PayloadDigest,ProofDigest,CertificateDigest,MountOperationID string; Runtime RuntimeReference; MountAttempt uint8
    Claim DispatchClaimReference; Attempt StageAttemptLocator
}
type RuntimePublicationProofRecord struct {
    Version uint32; IntentID,SandboxID,WorkspaceHash,RestoreEpoch,ProofDigest string
    Generation int64; Payload json.RawMessage
}
type RuntimePublicationEntry struct { Record RuntimePublicationRecord; Proof json.RawMessage; Reference StageReference }
func (Namespace) runtimePublicationKeys(uint8,string)(journal,proof string,err error)
func (*Backend) LoadRuntimePublication(context.Context,WorkspaceIdentity,string)(*RuntimePublicationEntry,error)
```

journal=`intentKey/runtime-publication`、proof=`intentKey/publication-proof`，stageID固定`runtime_publish`。RuntimePublicationRecord额外保存CertificateDigest、MountOperationID（plain为空/fuse=原mount UUID），与原binding/consumed mount关联。所有归属/UUID/digest/positive gen/gate/UTC/ref/partition/restore validate；proof.RawMessage digest按规范compact完整wire计算，record上限16KiB。Claim attribution存原签名currentclaim的worker/epoch/Lease，不重构capability。

Loader与dispatch同类两point定位+三point同Txn重验的immutable journal/proof/receipt。三者永久同首次revision，attempt.namespace/p/request/stage/restore精确匹配、receipt committed与合法digest；错receipt error分类沿用key-attributed reader。丢失/半套/改写/corrupt分类与dispatch保持一致；全部消失可nil，不证明target状态。此loader仅恢复metadata，不授予执行权限；签名历史核验由配置authority的Task4方法补充，不让未配置trust的读构造Publish权限。index不作为历史entry必需项，因为后续安全cleanup可删除当前index。

- [ ] model/codec/key与真实co-revision recovery RED→GREEN，覆盖context/lease/rewrite/receipt错误、别boot相同UID相同key、8192 bytes proof wire/16KiB完整proof record预算与copy。
- [ ] 新Backend可从永久receipt恢复ref，expired历史metadata仍只读，不用missing确认外部失败，不scan/N常驻资源。
- [ ] 定向race/vet/gofmt/diff、自审即独立提交，再spec→quality审查。

## Task 4：已绑定与已消费mount的组合Publish

**Files:** Create `runtime_publication.go`及happy/fault/validation tests。authority/clock和preparation接口已由Task2实现，不另建可绕过的证据入口。

**Interfaces:**

```go
type RuntimePublishDisposition string // PublishDeclared="published",PublishReplay="replay"
type RuntimePublishResult struct { Outcome Outcome; Disposition RuntimePublishDisposition; Reference StageReference; Entry *RuntimePublicationEntry; GuardCleanupError error }
func (*Backend) PublishRuntime(context.Context,*CreationClaim,[]byte)(RuntimePublishResult,error)
```

继续使用Task2构造时固定的authority/clock，Publish参数只能proof bytes，不接受root/verifier/Now/完成bool。不接配置文件/生产producer。

Publish先ctx/origin/local claim、copy/bounded proof，再在固定point读取中重验原dispatch、binding/cert/index/receipt、consumed mount/receipt；以private request/control检查原context，record.Target是配置target。派生PublicationContext包含原Runtime/CertificateDigest与mode对应原mount attempt/op，不能仅从传入proof自选Runtime。配置clock保守now和Verify上述认证/期限/新鲜度；没有binding/mount明确决策、proof/clock/trust、非法输入、expired或budget错误都在任何Grant之前拒绝。

existing publication合法且原runtime/op/digest/snapshot/expiry/gate/claim及ProofDigest一致则readonly replay，不Grant、不刷新证明；不同proof或runtime冲突，corrupt/context错拒绝。Historical读取只能证明先前committed，不能绕过current claim/clock赋予新权限。新backend恢复用LoadRuntimePublication+exact Resolve，不复建旧claim。

absent时与已认证entry一致的永久dispatch三record、binding四record、mount两record每条比较ModRevision+Lease0（不可改写且ModRevision固定已锁住其值，不放宽原claim24）。组合mutation追加journal/proof两absence，写owner.runtime/mount、control.active/runtime/mount、request.completed、intent.published、journal、proof；已预留runtime index保持原值并被比较。owner/control/request/intent新值从claim原24 cmp保存的raw永久值或private原context解码，不能丢未知字段/overwrite别的owner；request/control已有副本，owner/intent可从原comparison按exactkey提取或固定point读取并比较原版本。预算24+6+8+4+2 cmp、6writes、14预留=64，恰好上限，不得加未预算比较；Stage固定30s，内部builder的locator只存不含digest。Begin后重检ctx/live并重新clock/Verify，CommitStage；只有committed返回Entry，其余exactReference/nilEntry并单独bounded stageCleanup。不释放owner/claim，发布改变原四domain记录使原claim服务器能力自然失效。

- [ ] 真实有效签名发布RED→GREEN，六写+receipt同Txn，原fence/placement/snapshot/claim和已预留index不改，owner/control exactUIDBoot/requestintent/index一致。
- [ ] 并发Publish一次；确认Bind阶段两个workspace争同UID不同Boot仅一个预留；Publish拒绝缺失/错误/同值重建index；合法重放不Grant、不延长TTL，caller自签/未知root/过期/旧claim/gate/错operation/input/snapshot/runtime证据拒绝且不Grant。
- [ ] lost real reply→exact committed recovery；delayed完整Txn resolver先abort无半发布；原claim丢失/同值重建/control/restore与index篡改或重建失败；postBegin取消/clockunknown/expired拒绝；cleanup failure与known outcome独立；root/proof/output copy和wire/operation预算。
- [ ] 定向race、fresh owned全fixture、全仓/vet/build/gofmt/diff，自审即独立提交；spec→quality与whole-branchfinal，保存验证报告。实际helper来源与mount调用、配置/wiring仍未交付，不宣称切换完成。
