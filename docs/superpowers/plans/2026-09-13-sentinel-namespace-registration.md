# Sentinel namespace 身份登记 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use subagent-driven-development or executing-plans. 原分支执行，不创建 worktree，只有主 agent 提交。

**Goal:** 经真实签名的新鲜三成员空数据 Reserved 证明，原子登记不可覆盖的 marker/公钥摘要，且不提前开放 Initialized。

**Architecture:** namespace ConfigMap 是安装身份和一次性 seed grant，不是持续选主数据库。读取严格验证已有对象 UID/resourceVersion；get/update 指定名称，不 create/delete，不重试 CAS 冲突。Initialized 转换由真实拓扑与 ACK 初始化任务单独完成，本批绝不提供仅凭身份证明就开放业务的函数。

**Tech Stack:** Go、现有 Ed25519 identity challenge、client-go typed ConfigMapInterface、fake client reactors（确定性模拟 CAS，不称为真实 API server 实验）。

---

## 文件契约

只新建 `internal/redisbootstrap/registration.go`、`registration_kubernetes.go`、`registration_test.go`、`registration_kubernetes_test.go`。不得修改已复审 challenge/local-volume/state/decision 文件。

```go
type BootstrapRegistration struct {
    Cluster ClusterState `json:"cluster"`
    KeyDigest string `json:"keyDigest"`
    MarkerIDs [3]string `json:"markerIDs"`
}
func (BootstrapRegistration) Validate() error
func ParseBootstrapRegistration([]byte) (BootstrapRegistration,error)
func RegisterFreshVolumes(ClusterState,[3]ed25519.PublicKey,[3]IdentityChallenge,[3]IdentityProof) (BootstrapRegistration,error)
func VerifyRegisteredProof(BootstrapRegistration,[3]ed25519.PublicKey,IdentityChallenge,IdentityProof,*AuthenticatedEndpoint) error
type NamespaceBootstrap struct {
    Cluster ClusterState
    Registration *BootstrapRegistration
    UID types.UID
    ResourceVersion string
}
func LoadNamespaceBootstrap(context.Context,corev1.ConfigMapInterface,string,string) (NamespaceBootstrap,error)
func RegisterNamespaceBootstrap(context.Context,corev1.ConfigMapInterface,string,string,types.UID,string,[3]ed25519.PublicKey,[3]IdentityChallenge,[3]IdentityProof) (NamespaceBootstrap,error)
```

字符串参数依次 namespace/name，client 必须由调用者按 namespace 限定，返回对象 namespace/name 仍需一致。ConfigMap Data 只有 `cluster.json` 和可选 `registration.json`，不接受 missing/null/unknown/duplicate JSON。cluster.json 用已有 ParseClusterState；registration 严格 top keys、nested cluster、markerIDs 恰3项小写128-bit hex且互不相同，KeyDigest64小写hex。registration.Cluster 必须与 cluster.json 所有字段相同；Initialized 缺 registration 拒绝。

FreshVolumes 仅 Pending，三个 challenge 目的 inventory、nonce 互不相同；每槽独立 VerifyIdentityProof 固定公钥/成员/nonce，必须是 Reserved identity，无配置/数据/runID/digest；收集三 marker 和有序 PublicKeySetDigest。不得接受 Empty（成员先预约身份）、Configured、foreign、replay、混槽、重复 marker/nonce、弱或复用公钥。不因新多数默认意见产生 grant。

VerifyRegisteredProof 先验证 registration、公钥摘要，再用 proof.Member.Ordinal 取登记 marker 和固定 DNS：inventory 在 Pending 允许 Reserved→Configured，Initialized 必须 Configured；live 始终要求 Configured expected + freshly authenticated endpoint。更换 key/marker、empty replacement、同名异卷、旧进程一律拒绝。它不证明 nonce 的生成时间，调用者须在本次检查生成新挑战。

Load 不创建缺失 ConfigMap，要求非空 UID/RV，固定 namespace/name，有效阶段/登记数据；context 传给 Get。Register 必须提供本次读取获得的非空 expected UID/RV；重新 Get 后比对，任何对象 replacement/RV变化直接失败。已有 registration：只校验相同 keyDigest 及三份本次证明匹配登记，返回原状态，不执行 update、重置或覆盖；Initialized 不产生新 grant。无 registration：Pending +三Fresh证明生成登记，DeepCopy 保留原 metadata/labels，将 registration.json 写入，Update 时保留原 UID/RV。API Conflict 不自动 retry，不回退 create/patch；API 错误保留可识别类型，错误不回显 CM JSON 内容。返回 Update 对象必须重新验证 UID/name/namespace/cluster/registration，并与期望值严格相等；不能信任客户端未检查结果。

## Task 1：纯结构与签名登记

- [x] 测试和编译 stub，观察 behavioral RED 后 GREEN：真实 Ed25519 keypair + Reserved identities +各自随机挑战，登记必须包含准确 marker/keyDigest/仍 Pending。证据：`TestRegisterFreshVolumesRetainsPendingAndSignedIdentity` stub 编译成功后报 `three fresh Reserved proofs must register: invalid bootstrap registration`；实现后 focused suite exit 0。

```go
registration,err := RegisterFreshVolumes(c,keys,challenges,proofs)
if err != nil { t.Fatal(err) }
if registration.Cluster.Phase != Pending { t.Fatal("opened business gate") }
proofs[1].Nonce = challenges[0].Nonce
if _,err := RegisterFreshVolumes(c,keys,challenges,proofs); err == nil { t.Fatal("accepted replay") }
```

- [x] unknown/duplicate/null/trailing/wrong marker array lengths/phase/digest 验证，RegisteredProof 的换卷、空卷、换公钥、Pending Configured 和 Initialized Reserved/live endpoint/runID 拒绝矩阵。证据：真实签名表驱动测试；严格解析 round-trip 与 Reserved/Configured proof 测试也观察 stub behavioral RED 后 GREEN。

## Task 2：确定性 CAS

- [x] fake client 创建明确 UID/RV 的 ConfigMap，reactor 捕捉 update payload；未实现 stub 时行为 RED，再实现 GREEN。证据：`TestRegisterNamespaceBootstrapPinnedUpdate` stub 报 `fresh registration must update: invalid bootstrap registration`；实现后 reactor 明确断言 UID/RV/name/namespace、全部原 metadata、原 cluster.json 不变。

```go
state,err := RegisterNamespaceBootstrap(ctx,scoped,"isolated","identity","expected-uid","7",keys,challenges,proofs)
if err != nil { t.Fatal(err) }
if state.Cluster.Phase != Pending || updates != 1 { t.Fatal("invalid grant update") }
// reactor 必须检查传入 object.UID 和 resourceVersion，不靠 fake 的默认行为假设 CAS。
```

- [x] 重复 Job 已有登记0次 update、Initialized0次 update、更换对象UID/RV拒绝、Conflict返回不重试、缺CM不create、invalidresponse/mutatedregistration拒绝、cancelledGet、namespace/name替换拒绝。保留metadata，不删除其它release资源。证据：显式 fake reactors 和 action 断言只允许 get/update；API Conflict/NotFound/Forbidden 类型及 cause 可识别、错误正文不回显 CM JSON；Get/Update 传播调用 context，取消后的 API 返回不接受。

## Task 3：复审、证据与提交

- [ ] 规格复审通过后质量复审，修复 critical/important；gofmt、focused/full package、race、vet、lint、diff-check。实现 agent 证据：focused/full `go test ./internal/redisbootstrap -count=1`、`go test -race ./internal/redisbootstrap -count=1`、`go vet ./internal/redisbootstrap`、`golangci-lint run ./internal/redisbootstrap`（0 issues）、`git diff --check` 均 exit 0；等待主 agent 独立规格/质量复审和 fresh 验证，不提前勾选。
- [ ] 只记录确定性 fake API +真实签名结果，不能称实际 Kubernetes RBAC/CAS 已验收；不调用业务集群。主 agent fresh验证后提交精确文件。
- [x] 生产初始化命令、真实 INFO/角色/复制/epoch/WAIT 校验、phase Initialized CAS、包装器持久事务、Chart 和 API gate 仍是独立集成步骤。本批没有自动部署权限或生产完成声明。证据范围仅真实 Ed25519 签名 +确定性 fake client reactors；不声称 Kubernetes RBAC/真实 API server CAS/部署验收。
