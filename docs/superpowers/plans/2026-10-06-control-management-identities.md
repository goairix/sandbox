# 持续管理身份 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox syntax for tracking.

**Goal:** 给后续指令和target回执提供独立于业务expires_at的严格root-certified身份，保持角色和exact runtime边界。

**Architecture:** 同一controlprotocol包新增专用ManagementVerifier和两种独立wire/signature domains。Task1交付root-certified issuer，Task2交付exact runtime receipt identity；消费已有strict schema/Ed25519 helpers，不改publication/etcd Lease授权。验证证书只建立归因，执行仍需后续durable effect与target gate。

**Tech Stack:** Go标准库Ed25519/JSON/time，现有controlprotocol严格codec与testify。

**Spec:** `docs/superpowers/specs/2026-10-06-control-management-identities-design.md`，细化已批准overall设计。沿用当前分支、SDD执行方法与逐功能提交，不增加确认流程。Operation最终gate先通过，再实现源码。

## Global Constraints

- 两个roles固定command_issuer/runtime_receipt；Version1，canonical nonnil UUID CertificateID；delegate32B/signature64B；root ID SHA256派生；签发时delegate不能复用签发root public key；verifier拒绝delegate等于任一pinned root。
- 独立domains `sandbox-command-issuer-certificate:v1\x00` / `sandbox-runtime-identity-certificate:v1\x00`，实际NUL；每envelope<=4096B；严格required snake_case JSON/no null/duplicate/unknown/trailing/invalidUTF8/surrogate，UTC Z与现有ID/hash/namespace limits。
- pinned copied1–2roots；binding exact Namespace/AuthorityID/Target/RestoreEpoch。Runtime additionally exactSandbox/WorkspaceHash/Generation/ID/UID/BootID；不读wire来设信任或expectedcontext。
- Δ1s：now-Δ>=NotBefore、now+Δ<NotAfter；UTC非零且NotBefore<NotAfter；持续cert不绑定businessExpires。纯协议now仅供可信调用层，不加API调用者clock/历史恢复能力。
- evidence私有字段；key/wire输入输出复制。无backend root privatekey、无RPC/runtime/issuer service、无额外dependency、无Manager/config切换，无修改publication协议。
- 每验证自审通过单元立即独立commit；review修复另commit。保留Sentinel修改，禁止读取.env/生产凭证、deploy/push/merge。身份激活/撤销、targetkeycustody与实际执行权限属后续必须实现。

## Review Focus

1. 有效publication或issuer委托签名不能跨domain取得runtime回执角色。
2. 已过businessTTL但仍有效ongoing身份可以归因cleanup，不能因此产生新操作capability。
3. root overlap/removed root与mutable root/key/wire别名不能改变已构造verifier/evidence。
4. 同PodUID容器重启的BootID必须严格拒绝旧expectedcontext；role/restore/generation也不能模糊匹配。
5. 等号时间边界、非法wire及失败decode不能返回半套有效identity。

## Task 1：root-certified command issuer

**Files:** Create `internal/runtime/controlprotocol/management_types.go`, `management_issuer.go`, `management_issuer_test.go`；允许在新文件声明专用schemas并复用现有codec helpers，publication文件不改。

**Interfaces:** consumes TrustBinding、copyPrivateKey、wireDigest、signingBytes、strictschema/codec/valid* helpers。Produces：

```go
type ManagementVerifier struct // private copied binding/root map
func NewManagementVerifier(TrustBinding,[]ed25519.PublicKey)(*ManagementVerifier,error)
type CommandIssuerCertificateClaims struct // exact spec fields, snake_case
func SignCommandIssuerCertificate(ed25519.PrivateKey,CommandIssuerCertificateClaims)([]byte,error)
func (*ManagementVerifier) VerifyCommandIssuerCertificate([]byte,time.Time)(CommandIssuerIdentity,error)
// private evidence; Wire() []byte; PublicKey() ed25519.PublicKey;
// Digest()/CertificateID() string; NotBefore()/NotAfter() time.Time
```

私有ManagementVerifier certificate认证helper仅按task2需要复用binding/root/time/keylength逻辑，不搞通用任意role动态schema或publication重构。

- [ ] 写 `TestCommandIssuerIdentity` RED：真实root/delegate roundtrip，派生RootKeyID、copygetter、time窗口与无partial evidence；`TestCommandIssuerIdentityRejects` 覆盖strictwire/role/domain/非法key/委托伪root/unknownroot/Δ等号；`TestManagementVerifierPinsTrust` 覆盖binding/root复制、overlap/removal、非法root数量和并发验证，以及root A签root B为delegate但两者皆pinned时拒绝。
- [ ] 跑 `go test ./internal/runtime/controlprotocol -run 'TestCommandIssuerIdentity|TestManagementVerifier' -count=1`，确认真实behavior RED而非只有编译失败。
- [ ] 按上述signatures实现Task1；签发root self-delegate拒绝，wire claims不能选择roottrust。修改输入字节不影响已签wire，输出别名不能污染identity。
- [ ] focused GREEN，再 `go test -race ./internal/runtime/controlprotocol -run 'TestCommandIssuerIdentity|TestManagementVerifier' -count=1`、package vet/gofmt/diffcheck；覆盖既有publicationtests一次，记录命令结果，自审。
- [ ] stage仅本任务文件，立即commit `feat(controlprotocol): certify independent command issuer identities`；独立spec/qualityreview通过再Task2。

## Task 2：root-certified exact runtime receipt identity

**Files:** Create `internal/runtime/controlprotocol/management_runtime.go`, `management_runtime_test.go`；Modify `management_types.go` 添加Task2类型，复用Task1 pinned认证helpers。

**Interfaces:** consumesTask1 ManagementVerifier/helpers/evidence复制模式；produces：

```go
type RuntimeIdentityContext struct {
    SandboxID,WorkspaceHash string
    Generation int64
    Runtime RuntimeReference
}
type RuntimeIdentityCertificateClaims struct // issuer common spec fields + exact runtime context
type RuntimeReceiptIdentity struct // private fields
func SignRuntimeIdentityCertificate(ed25519.PrivateKey,RuntimeIdentityCertificateClaims)([]byte,error)
func (*ManagementVerifier) VerifyRuntimeIdentityCertificate([]byte,RuntimeIdentityContext,time.Time)(RuntimeReceiptIdentity,error)
// same getters as issuer plus Context() RuntimeIdentityContext
```

不会用初始CertificateContext.ExpiresAt或callerbusinessTTL限制ongoingcert；身份通过不发送或授权命令。只提供fresh验证；不存在从historicalidentity恢复权限的入口。

- [ ] 写 `TestRuntimeReceiptIdentity` RED：root/delegate/exactcontext roundtrip、ongoinginterval跨已过businessTTL（测试使用同一synthetic过期publicationcontext作为对照，ongoingtype没有ExpiresAt）、复制与并发、birthwire仍按旧business语义拒绝；`TestRuntimeReceiptIdentityRejects` 覆盖每一exactcontext字段/role/domain/signature/wire/Δ；`TestManagementIdentityRoleSeparation` 两种有效wire双向错用、issuerdelegate签runtime cert不被root信任、removedroot/restore/BootID拒绝。
- [ ] 跑 `go test ./internal/runtime/controlprotocol -run 'TestRuntimeReceiptIdentity|TestManagementIdentityRoleSeparation' -count=1`，确认behavior RED。
- [ ] 按接口实现runtime独立schema/sign/verify及完整context相等；无wildcard，不采用proof自述expectedcontext。不更改issuer/publication授权语义。
- [ ] focused GREEN；完整 `go test -race ./internal/runtime/controlprotocol -count=1`、package vet/gofmt/diffcheck，记录真实结果，自审即commit `feat(controlprotocol): bind management receipts to exact runtime identities`；独立spec/qualityreview。
- [ ] controller source冻结，`go test ./...`/`go vet ./...`/`go build ./...`并行必要checks；此增量未改etcd不重复ownedfaultsuite，previousOperation实际source证据保留。做完整base..head整分支review；一次finalfixwave与scoped复核，完整记录rulings/declined和未完成整体验收。

## Self-review与连续执行

本计划只细化ongoing身份先决条件；command内容digest/有界ticket、effect intent、terminal安全End、task claim/close/reopen与target durable activeCertificateID/rotations另有后续单元，不提前给身份原语增加执行能力。两task每一生产/消费名称完全一致，严格wire/time/copy/role/BootID五类输入均有归属测试；不将这两task通过当整体迁移完成。现有整体授权足以实施这一必需细化，无新增外部副作用或部署动作。

Self-review completed: exact signatures/types and all Review Focus tests mapped; overlap-root delegate key reuse explicitly rejected by verifier as well as direct self-delegate signing. Source implementation still waits for prior Operation gate, now controller final checks and scoped review have passed at29c3cf6. Full operational activation/rotation remains a later requirement, not stateless certificate capability.
