# Sentinel 身份挑战传输实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use subagent-driven-development or executing-plans, task-by-task. 不创建 worktree。只有主 agent 提交；测试不操作业务集群。

**Goal:** 将先前仅有 HMAC 示意的身份检查变为可测试的、固定成员 Ed25519 签名和新鲜挑战 HTTP 协议。

**Architecture:** 库只处理协议和受限传输；可信本地 ObservationProvider 负责读取 PVC 和本地认证 INFO。服务端不接受远端指定身份/角色的签名请求。固定公钥客户端验证 nonce、clusterID、成员、marker、配置摘要和当前 Redis run_id；此批不把库存在误称为完整 Sentinel 引导完成。

**Tech Stack:** Go 1.25、crypto/ed25519、crypto/rand、net/http、httptest、filippo.io/edwards25519 v1.2.0（Go 派生、维护中的群运算库）、现有 redisbootstrap 类型。

---

## 文件与契约

- 新建 `internal/redisbootstrap/challenge.go`：协议验证、签名、公钥集摘要、随机 nonce/session。
- 新建 `internal/redisbootstrap/challenge_http.go`：固定 `/v1/identity` POST、请求/响应大小及超时/并发限制、禁止代理/重定向的固定成员客户端。
- 新建 `internal/redisbootstrap/challenge_test.go`、`challenge_http_test.go`：真实签名和 httptest 传输回归。
- 已有 HMAC readiness 示意函数不改、不作为新协议 fallback。

```go
type ProofPurpose string
const InventoryProof ProofPurpose = "inventory"
const LiveProof ProofPurpose = "live"
type IdentityChallenge struct { Version int; Purpose ProofPurpose; Nonce string }
type LocalObservation struct {
    Volume VolumeState
    Snapshot *PersistentConfigSnapshot
    ConfigDigest string
    RunID string
}
type IdentityProof struct {
    Version int
    Purpose ProofPurpose
    Nonce, Session, ClusterID string
    Member Member
    Observation LocalObservation
    Signature string
}
type ObservationProvider func(context.Context, ProofPurpose) (LocalObservation, error)
func NewIdentityChallenge(ProofPurpose) (IdentityChallenge, error)
func NewProofSession() (string, error)
func SignIdentityProof(ed25519.PrivateKey, ClusterState, Member, string, IdentityChallenge, LocalObservation) (IdentityProof, error)
func VerifyIdentityProof(ed25519.PublicKey, ClusterState, Member, IdentityChallenge, IdentityProof, *VolumeIdentity, *AuthenticatedEndpoint) error
func PublicKeySetDigest([3]ed25519.PublicKey) (string, error)
func NewIdentityHandler(ClusterState, Member, string, ed25519.PrivateKey, ObservationProvider) (http.Handler, error)
func FetchIdentityProof(context.Context, ClusterState, Member, ed25519.PublicKey, IdentityChallenge, *VolumeIdentity, *AuthenticatedEndpoint) (IdentityProof, error)
```

新导出结构使用明确的 camelCase JSON 标签；嵌入现有结构保留其原有 wire 字段。请求字段必须恰好 version/purpose/nonce，响应、嵌套结构拒绝 unknown/duplicate/case variant/missing 和必需字段的 null；仅 Identity/Persisted/Snapshot 可选指针允许 null，再由 observation 状态约束验证。nonce/session 为 64 小写 hex 字符，version=1，签名为 128 小写 hex 字符；run_id 为 40 小写 hex 字符。域分离 `sandbox/redisbootstrap/identity-proof/v1\x00`，签名固定结构 JSON（Signature 清空）。每个成员对应唯一公钥，拒绝重复公钥。session 是签名绑定的固定服务进程随机标识；当前验证接口没有 expected-session 入参，不宣称验证外部已知 session，重放防护由调用者独立新鲜 nonce 提供。

真正空卷必须 `Empty=true`、Identity/Persisted/Snapshot=nil、digest/runID 空。Reserved 必须有匹配本地的 Reserved identity、Empty=false、无 persisted/snapshot/digest/runID。Configured 必须有匹配本地的 Configured identity、Persisted 和 Snapshot.State 完全相等；Snapshot.SentinelID 合法，CurrentEpoch>=SentinelEpoch，configDigest 为 64 小写 hex。role/monitor 必须符合现有解析器约束。inventory runID 必须为空；live 只允许 Configured 并要求合法 runID。

签名函数是可信本地接口，不证明调用者实际完成 PVC/INFO 检查；HTTP 请求只传 challenge，本地 provider 返回 observation，服务固定本地 cluster/member/session。inventory 的 expected identity 可空用于首次登记；登记后必须传预期身份，且不能验证 empty 为既有 marker。live 必须传 Configured 预期身份和 freshly authenticated endpoint；不允许传 nil 绕过 runID 绑定。

## Task 1：签名和结构验证

- [x] 添加测试和临时 stub，运行 `go test ./internal/redisbootstrap -run TestIdentityProof -count=1`，观察缺失签名/拒绝逻辑的 behavioral RED。
- [x] 完成最小实现，再运行同命令，预期 PASS。
- [x] 先添加 nonce/purpose/member/session/cluster/marker/digest/epoch/role/snapshot/runID/signature tampering 回归，再实现缺少的校验。验证旧 nonce、旧进程、旧 marker、错误公钥、重复公钥及 live 缺 endpoint 均拒绝。
- [x] 库返回错误不包含 token、私钥、Redis 密码或配置原文。

测试必须使用真实生成的 Ed25519 keypair（或确定性的测试 seed），直接修改签名响应，并断言错误而不是只检查字符串：

```go
proof, err := SignIdentityProof(private, c, local, session, challenge, observation)
if err != nil { t.Fatal(err) }
if err := VerifyIdentityProof(public, c, local, challenge, proof, &identity, &current); err != nil { t.Fatal(err) }
next, err := NewIdentityChallenge(LiveProof)
if err != nil { t.Fatal(err) }
if err := VerifyIdentityProof(public, c, local, next, proof, &identity, &current); err == nil { t.Fatal("accepted replay") }
```

## Task 2：受限真实 HTTP

- [x] 添加 httptest 端到端回归与 stub，观察 behavioral RED 后实现 handler。
- [x] handler 只接受固定 POST 路径，无 query，不超过 2 KiB 请求，拒绝 Content-Encoding、非 JSON、unknown/duplicate/required-null/trailing JSON，不调用 provider 处理这些非法请求；响应不超过 16 KiB。并发最多 8，超时 2 秒，忙时立即拒绝，不无限排队。provider 必须遵守 context，不声称可以强杀不合作的 provider。
- [x] handler 私钥复制到自身，固定 session；错误响应只返回常量类别，不回显输入或 provider error。GET/其它 path 拒绝。成功签名设置 no-store。
- [x] 固定客户端 URL `http://<validated fixed member DNS>:18080/v1/identity`，禁止重定向、环境代理、压缩、连接复用和自动重试；Dial/response/header/total 超时有界（整体 3 秒），按 16 KiB 上限完整读取，严格 decode 然后 verify。生产接口不能接收 arbitrary URL 或 http.Client。
- [x] httptest 可测试内部受限 client helper；通过受控本地 Dial mapping 测试内部 transport 的固定 DNS:18080 地址；公开入口测试取消及非法 DNS，不给生产新增测试绕过字段。
- [x] 覆盖服务关闭、阻塞 provider context 取消、客户端取消、3xx、巨大响应、重复/未知字段、异常嵌套字段、签名篡改、错误 expected marker、旧 nonce；真实 provider 调用次数证明非法请求未到签名提供者。

## Task 3：两级复审与验证

- [ ] 先独立规格复审，再代码质量复审；修复全部 important/critical。
- [x] `gofmt`、`go test ./internal/redisbootstrap -count=1 -cover`、`go test -race ./internal/redisbootstrap -count=1`、`go vet ./internal/redisbootstrap`、增量 lint、`git diff --check`。
- [ ] 主 agent 复跑验证后提交精确文件，不推送。不用单元测试宣称 PVC snapshot、namespace 注册 CAS、生产 sidecar wiring、Sentinel 冷恢复已通过；这些需要下一段独立集成计划与真实隔离实例。

## 完整目标后续顺序（未完成项，不是部署许可）

### 本批协议验证证据（2026-09-13）

- Core RED：临时 stub 可编译，inventory/live round-trip 均因 `not implemented` 失败；扩充 tamper/observation/key 回归后仍失败，再实现 GREEN。
- HTTP RED：临时 handler/client stub 可编译；round-trip `not implemented`，provider 未启动，owned transport 缺失，再实现 GREEN。
- `go test ./internal/redisbootstrap -count=1 -coverprofile=/tmp/sentinel-challenge.cover -timeout=30s` PASS，整个当前 package 90.9% statements（包含同期 local PVC/parser 文件）；本批 sign/verify/observation/key-digest 100%，strict JSON 95.2–100%，handler 85.7%。随机源失败及不可能构造的 Marshal/固定结构响应超限防御分支未人为注入。
- `go test -race ./internal/redisbootstrap -count=1 -timeout=30s` PASS；`go vet ./internal/redisbootstrap` PASS；`golangci-lint run ./internal/redisbootstrap --timeout=2m` 0 issues；`git diff --check` PASS。
- Provider 取消测试真实通过请求 context；最初 slow-body 测试 fixture 未读取请求 body，造成 httptest Close 等待，已先完整读取请求再阻塞响应，重新运行 PASS。生产没有新增 goroutine 或测试绕过字段。
- Handler 实际连接上设置 request read deadline；生产 native server 仍需 ReadHeaderTimeout/连接预算等包装。本批不宣称测试了 namespace CAS、native sidecar wiring、集群冷恢复或 AppArmor。
- 规格复审 P2 测试前置条件修复：非法请求矩阵原先无条件添加空 Content-Encoding，提前触发 header 拒绝；现在仅非空 encoding 用例设置此头，先用合法请求证明 providerCount=1，再逐项证明非法 JSON/大小用例不增加次数。另保留独立的空 encoding header 拒绝。此次只加强现有行为的覆盖，不称为新 production behavioral RED。修复后 focused/package/race/vet/lint/diff-check 均 PASS；最新整个 package coverage 91.3%，handler 95.2%。
- 质量复审 P1 实际伪造 RED：identity 公钥 `01`+31 个零搭配 R=identity/S=0，无私钥即可通过 Go Ed25519 Verify；旧库 VerifyIdentityProof 和 PublicKeySetDigest 均接受，新增测试先观察失败。GREEN 使用维护的群库先解码并比较 canonical bytes、拒绝 identity，再要求 `[8^-1]([8]P)==P`，拒绝纯 small-order 和 mixed-torsion/non-prime-subgroup 公钥；已覆盖曲线非法编码、noncanonical y、negative identity、order2 和全部七个非 identity order8 torsion 点及 generator+各 torsion 的混合点。正常真实 Ed25519 生成公钥保持通过。
- 质量复审 P2 私钥实际 RED：两个真实 keypair 的 seed/public halves 混合后，旧 SignIdentityProof/NewIdentityHandler 均接受，测试先观察失败。GREEN 按 `ed25519.NewKeyFromSeed(key[:32])` 重建并 constant-time 比较完整 64 字节，不一致构造在调用 provider 前拒绝。
- 安全修复后 focused/package/race/vet/lint/diff-check 均 PASS；当前整个 package coverage 90.4%（同期本地 PVC 代码变动影响整体值），本批 public/private key validation、Sign/Verify、observation/key digest 均 100%，handler 95.2%。生产 native HTTP server 必须配置 WriteTimeout、ReadHeaderTimeout 和连接预算；2 秒 provider context 不等于强杀不合作 provider 或强行中断任意 ResponseWriter.Write。

### 后续集成

1. 本地 PVC 安全读取和可中断 Reserved→Configured 配置事务。
2. 固定 namespace ConfigMap 注册 marker/公钥摘要/seed grant 的 resourceVersion CAS；仅全新三份 Reserved 空数据证明授权，重复/缺失/换卷 fail-closed。
3. 生产包装器最终 exec、初始化 Job 真实角色/复制/epoch/成员+有写 offset 的 barrier WAIT、API Initialized gate。
4. Chart 原生 sidecar/Parallel StatefulSet/硬反亲和/PDB/PVC/Secret/NetworkPolicy/中文 values/统一 API/drain 配置及预算。
5. 真实生产协议隔离 Redis 启动、中断、failover、非0主冷恢复、状态丢失、同名 replacement 矩阵。
6. AppArmor 专用测试节点和用户构建镜像的内核 enforce/mount/flush/unmount 验收。当前业务节点 profile 加载仍未获授权。
