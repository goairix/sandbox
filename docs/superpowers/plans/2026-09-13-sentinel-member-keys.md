# Sentinel 成员密钥 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use executing-plans or subagent-driven-development. 不创建 worktree，不调用 Kubernetes API、不输出真实私钥。

**Goal:** 为三个 sidecar 生成独立密钥和 public-only 核验配置，供安装前 Secret 创建命令复用。

**Architecture:** 创建 immutable Secret 对象但不提交到集群。三个私钥 seed 分别以固定 Pod 名作为 data key，Chart 通过 subPathExpr 只挂载本 Pod 的 seed，Job/API 只挂载 public-keys.json。公钥集摘要在 namespace 注册后禁止变化。

**Tech Stack:** Go crypto/ed25519、crypto/rand、corev1.Secret、已有严格公钥点验证。

---

只新建 `internal/redisbootstrap/member_keys.go`、`member_keys_test.go`。

```go
func GenerateIdentitySecret(namespace,statefulSetName string) (*corev1.Secret,error)
func ParseMemberPublicKeys([]byte) ([3]ed25519.PublicKey,error)
func ParseMemberPrivateSeed([]byte,[3]ed25519.PublicKey,int) (ed25519.PrivateKey,error)
```

namespace 为合法 DNS label；StatefulSet 名为合法 DNS label、最长54，确保 `<sts>-headless` 和 `<sts>-<ordinal>` 都是合法 DNS label。Secret 名 `<sts>-identity`，Namespace 固定，Type=Opaque、Immutable=true；Data 只有 `<sts>-0/1/2` 的各自32bytes seed 及 `public-keys.json`（恰3个64lowerhex公钥字符串 JSON array）。用crypto/rand生成，公钥集验证长度、唯一、canonical/prime-subgroup/nonidentity；不自动替换或轮换已有 Secret。

PublicKeys 文件不超过1KiB、合法UTF8、恰好三项 string、不接受null/trailing/对象或混型；每项必须64lowerhex并通过 PublicKeySetDigest。PrivateSeed 只接受32rawbytes，ordinal0..2、公钥集有效，NewKeyFromSeed 后 constant-time 比较该 ordinal 的公钥；不能接受其它 ordinal 的 seed、base64字符串或64byte mixed privatekey。不做密码派生、不把 Redis 密码当私钥。

- [x] 写生成/解析 roundtrip与临时stub，运行 `go test ./internal/redisbootstrap -run TestMemberKeys -count=1`，观察行为RED。

```go
secret,err:=GenerateIdentitySecret("isolated","redis-sentinel")
if err!=nil || secret==nil || !*secret.Immutable { t.Fatal("missing immutable secret") }
keys,err:=ParseMemberPublicKeys(secret.Data["public-keys.json"])
if err!=nil { t.Fatal(err) }
for i:=range 3 {
    private,err:=ParseMemberPrivateSeed(secret.Data[fmt.Sprintf("redis-sentinel-%d",i)],keys,i)
    if err!=nil || !bytes.Equal(private.Public().(ed25519.PublicKey),keys[i]) { t.Fatal("mismatched member") }
}
```

- [x] GREEN最小实现；再加拒绝与随机性覆盖：namespace/name过长/IP/illegal、missing/null/duplicate/noncanonical/weakpublickeys/oversize、跨ordinalseed、badlen/invalidordinal；两次生成公钥不相同，错误不含seed内容，不打印Secret JSON。
- [ ] focused/package/race/vet/lint、独立规格后质量复审；主 agent 提交精确文件。
- [ ] 不把对象生成或单测当部署完成；命令/Secret只读挂载/真实sidecar以及初始化仍须集成验收。

## 执行证据

- roundtrip stub 缺 immutable Secret 的真实行为 RED → GREEN；malformed/随机性/跨成员拒绝矩阵后 focused PASS 1.421s。
- 独立规格复审通过，另一次独立 focused PASS 0.409s。
- 独立质量复审通过；reviewer 的 focused/race/vet 均通过。主 agent 提交前运行整体验证。
