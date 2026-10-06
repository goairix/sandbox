# 持续管理身份：独立签发者与精确 runtime 回执身份

这是已批准 `2026-10-06-etcd-state-management-design.md` 中执行 fencing 的细化单元。用户要求继续实施并逐功能提交；不重复请求执行批准。Operation 准入最终 gate 必须先通过，才能开始本单元源码实现。

## 目的与边界

初始 runtime publication certificate 的 NotAfter 受业务 expires_at 限制，不能用于过期后的清理，也不能在业务 TTL 延长后直接充当长期管理凭证。新增两个独立 root-certified 角色：API/worker 指令签发者，以及绑定 exact runtime incarnation 的回执签名者。后续 command/effect/task 协议消费这两类身份；本单元只提供严格签发与验证原语，不发送指令、不切换 Manager、不让签名证书替代 etcd capability。

不复用 publication/ready 的 wire、signature domain 或 evidence 类型。保持其现有接口与业务到期语义。部署可使用相同的 pinned authority roots，但委托私钥必须分角色保管；本单元禁止把 root 自身 public key 注册为其签发的 delegate。控制面 Backend 不持有 root private key。

## 身份与签名上下文

两类 claims 均为 Version=1，包含派生 RootKeyID、不可复用 CertificateID（canonical non-nil UUID）、显式 Role、Namespace、AuthorityID、Target、RestoreEpoch、PublicKey、NotBefore、NotAfter。PublicKey 为32字节 Ed25519 delegate key，签名64字节。TrustBinding 与现有 deployment 绑定结构相同，namespace<=512 bytes，opaque/segment各<=128 bytes，hash为64字节小写SHA256 hex；UUID 校验复用现有严格规则。

CommandIssuerCertificateClaims 的 Role 固定 `command_issuer`，绑定整个已登记 authority/target/restore，不能解释成 runtime 回执身份。RuntimeIdentityCertificateClaims 的 Role 固定 `runtime_receipt`，另包含 SandboxID、WorkspaceHash、Generation>0、Runtime ID/UID/BootID；验证必须逐项等于从权威状态得出的 RuntimeIdentityContext。没有 wildcard，也不接受以 Pod 名代替 UID/BootID。

签名分别使用 `sandbox-command-issuer-certificate:v1\x00` 和 `sandbox-runtime-identity-certificate:v1\x00`，其中最后的序列表示一个实际 NUL 字节。root key ID 是 root public key 的 SHA256 hex。签发函数复制并校验 private key 的完整 seed/public suffix，忽略调用者 RootKeyID 并派生正确值，复制 delegate key，然后校验全部 claims 后签名。certificate wire 使用现有稳定 typed JSON canonicalization，摘要是 normalized envelope 的 SHA256 hex。

## wire 与时间契约

两种完整 certificate envelope 各硬上限4096 bytes；所有 snake_case 字段必须出现，不用 omitempty。拒绝 unknown/duplicate/missing/null/trailing JSON、非整数 revision/version、无效UTF8与未配对 escaped surrogate、非UTC Z时间、非法 key/signature length、非canonical UUID、非法hash与空身份。使用现有 private strict codec/schema helpers，不改变 publication wire 的兼容行为。签名验证先严格解析/语义校验，再验 pinned root 和 context；任何失败不返回部分有效 evidence。

NotBefore/NotAfter 都是非零UTC且 NotBefore<NotAfter；持续身份不绑定业务 expires_at。纯协议 verifier 由调用者传入可信UTC observation，按已批准Δ=1秒要求 `now-Δ>=NotBefore` 且 `now+Δ<NotAfter`，不以本机时间兜底。实际签发服务与命令生产者稍后接入 constructor-controlled clock；API 请求中的 now 永不授权。证书有效并不绕过业务新准入期限、原 operation/task Lease、control revision或target closed gate。

本单元只提供 fresh certificate verification，不增加 historical-to-live-authority API。历史 terminal 回执的安全恢复仍需后续明确协议，不能用旧证书上的自述 timestamp 当作当前执行权限。

## 信任与 API

`NewManagementVerifier(TrustBinding, []ed25519.PublicKey)` 固定并复制1–2个部署 roots，按 key digest 索引，拒绝零个、超过两个或非法 roots/绑定；与 PublicationVerifier 独立。wire 不能携带可被直接信任的新root。verifier另拒绝delegate public key与任一当前pinned root相等，避免overlap轮换中把另一个root作为委托key。root overlap轮换由部署决定，移除旧root后旧root签名立即不能通过新verifier。委托角色不能用自己的key签另一类 root证书获得权限。

签发 API：`SignCommandIssuerCertificate(ed25519.PrivateKey, CommandIssuerCertificateClaims)` 和 `SignRuntimeIdentityCertificate(ed25519.PrivateKey, RuntimeIdentityCertificateClaims)`，均返回 `([]byte,error)`。

验证 API：`(*ManagementVerifier).VerifyCommandIssuerCertificate([]byte,time.Time)(CommandIssuerIdentity,error)`；`VerifyRuntimeIdentityCertificate([]byte,RuntimeIdentityContext,time.Time)(RuntimeReceiptIdentity,error)`。Evidence 内部字段私有，返回 Wire/PublicKey 时复制，另提供 Digest、CertificateID、NotBefore、NotAfter；runtime evidence 提供 Context 值对象。public evidence是认证归因，不是可从公网恢复的 operation/task capability。

本单元不声称撤销所有有效委托证书。delegate activation/rotation 必须在后续以 exact state-chain CAS 和 target持久 active CertificateID/key确认完成；新 root certificate 本身不证明 target 持有私钥、物理 incarnation 或旧delegate已隔离。collector不取得任何签发/执行角色。旧凭证隔离与灾备安装继续是整体必需验收。

## 验收

分别进行真实 Ed25519 sign/verify RED→GREEN。覆盖 plain/fuse 无关的 exact runtime身份、role/domain互换、publication birth wire拒绝、非root委托签发拒绝、unknown/removed root、restore/namespace/authority/target/workspace/generation/sandbox/UID/BootID错位、key/signature/field篡改、严格wire边界与复制/并发、Δ时间等号拒绝。证明ongoing runtime cert可在业务期限已过而自身仍有效时认证身份，但不增加命令授权 API。

本包纯函数测试不需要Docker fixture；跑 focused package/race/vet 和全仓 build。最终全分支审查检查与publication及etcd原capability的边界，不重复无变化etcd全故障suite；若新diff触及etcd源码则另确定覆盖范围。旧 Stage Grant-boundary copy建议保持明确延期。真实launcher/keycustody/mTLS、commands/effect/End/task/close/drain、Scheduler/GC/Redisremoval、巨大N容量均未因本单元完成而豁免。
