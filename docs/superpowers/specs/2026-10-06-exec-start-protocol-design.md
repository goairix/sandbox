# 原 operation 绑定的执行启动协议

本设计细化已批准 etcd phases1–5 的 command/effect 链路，沿用连续实施与小提交。只交付最终执行参数的不可变摘要和严格、短时、start-only 签名票据；紧接的 effect journal/launcher 单元才能产生实际执行权限。它不能从 public OperationReference 重建 capability，不调用 runtime，不将签名验证称为 target 已接受或终态。

## 方案选择与边界

选择 typed descriptor + role-specific start ticket，消费已有独立 issuer 身份。相比直接签任意 caller digest，可在接收端对实际 argv/stdin 等重新构造摘要；相比立即覆盖 exec/file/task/renew 的通用指令语言，先完成现有 exec/stream 共用的独立、可验证闭环，避免 role 混淆。文件 mutation、生命周期/task 指令和 accepted-only renewal 保留后续独立协议，绝不能借此 start wire 通用化。

任务1生成 immutable descriptor，任务2把其 digest 与完整 exact context 及保守时限绑定。受控 clock、当前原 capability、etcd intent 先提交、target durable active issuer/cert、closed gate、journal 防重、降权与所有后代排空均是后续必须实现的消费者条件。此处证据叫 ExecStartEvidence，绝非 execution permit。公开签发原语只生成字节，不能自行授予 etcd 或 physical 权限。已批准整体目标仍包括彻底移除 Redis、O(C) 而非 O(N) 的活动管理，以及真实存量规模验收。

## 最终执行参数

`ExecutionRequest` 为已解析的最终 payload，不接受未展开语言/code/defaults：
- Argv []string，1–256 项，argv[0] 非空；其余允许空字符串，每项 <=65536B。
- Env map[string]string，完整有效 child env（不得 implicit 继承父环境），0–256 项；key 1–256B、ASCII `[A-Za-z_][A-Za-z0-9_]*`，value <=65536B。
- UID/GID uint32，各 1..2147483647；root=0 拒绝。目标端仍必须拒绝专用管理 UID，并实施全部 credential/capability/FD 降权。
- WorkDir string，UTF-8、无 NUL、1–4096B、POSIX absolute 且 path.Clean(input)==input；不做静默默认/normalize。此校验不替代 ScopedFS/目录FD/target路径策略。
- TimeoutSeconds uint32，1..3600；Stdin []byte，<=1048576B，允许任意二进制、空/nil 同义。
- TTY bool、RequiresNetwork bool；二者都是实际执行参数，摘要必须包含。

所有 argv/env 字符串合法 UTF-8 且无 NUL；参数中的 shell metacharacter 作为原始字节，不清洗/拆分/执行。env sorted by ASCII key，nil/empty env 与 stdin 同义。规范 metadata 固定字段顺序 JSON struct：version=1, kind="exec", argv, env（非nil数组，元素 name/value）, uid, gid, work_dir, timeout_seconds, stdin_length(int64), stdin_digest(lowercase SHA256), tty, requires_network；所有字段必须参与 json.Marshal，无 omitempty。metadata <=65536B，不含 stdin 内容。descriptor digest = lowercase SHA256(`sandbox-exec-descriptor:v1\x00` 实际 NUL + canonical metadata)。输入 argv/env/stdin 拷贝，canonical/getter 全部拷贝；zero descriptor 不可用于 ticket。

公开接口：`NewExecutionDescriptor(ExecutionRequest)(ExecutionDescriptor,error)`；private fields 的 value evidence 有 `Digest()string`, `Canonical()[]byte`, `Request()ExecutionRequest`。错误始终返回 zero descriptor。无 wire decoder；目标未来必须从实际将执行的完整 payload 重建 descriptor，不得仅采用请求自述 digest。Go 类型零值不自动填默认；API adapter 后续显式解析默认和语言 wrapper。

## start ticket 的 exact context

`ExecStartContext` 是可比较 value struct，全部 snake_case tags、required 字段：
Namespace, AuthorityID, Target, RestoreEpoch string；IssuerCertificateID, IssuerCertificateDigest string；CommandID, OperationID, RequestID, OperationDigest string；SandboxID, WorkspaceHash string；Generation, DataGateEpoch, ControlRevision, AdmissionRevision, LeaseID int64；Runtime RuntimeReference；ExpiresAt time.Time。

binding 复用 validateBinding；issuer cert/command/operation ID canonical nonnil UUID；RequestID/SandboxID validID（与已有 Operation 一致，不要求 RequestID UUID）；两 digest/WorkspaceHash 是64 lowercase hex；五整数均>0；runtime exactID/UID/BootID 沿用 validRuntime；ExpiresAt 已知UTC非零。AdmissionRevision 是原 token/receipt 首次提交 revision，ControlRevision、LeaseID 都是 original operation 的值；不是从 target 或 wire 推导当前授权。所有 context 字段逐项严格相等，time 比较其 UTC instant。

`ExecStartTicketClaims {Version uint32; Purpose string; Context ExecStartContext; DescriptorDigest string; NotBefore,NotAfter time.Time}` 的 JSON tags version/purpose/context/descriptor_digest/not_before/not_after；固定 Version1、Purpose="operation_exec_start"；NotBefore/NotAfter UTC非零，NotBefore<NotAfter，interval<=30秒且 NotAfter<=Context.ExpiresAt。此30秒仅是原 operation 当前 Lease 的最大协议边界，不能替代后续从原 sent-time monotonic deadline 算更小的 remaining bound。不能用票据到达时间重新加30秒，也不能用 start ticket 在接受后续命跨业务到期。

票据签名 domain `sandbox-exec-start-ticket:v1\x00` 实际 NUL，独立于 certificate/ready domains。envelope = {claims,signature}，<=4096B，signature64B。独立 issuerWire 参数 <=4096B，由 pinned ManagementVerifier 当前 fresh 认证；不将 cert wire 内嵌入票据，不放 argv/stdin/stdout 到 etcd。strict required/unknown/duplicate/null/trailing/invalidUTF8/surrogate/integer/UTC Z 沿用 codec。

签发接口 `SignExecStartTicket(ed25519.PrivateKey,CommandIssuerIdentity,ExecStartTicketClaims)([]byte,error)`：复制并验证私钥与 issuer.PublicKey 完全匹配，issuer evidence 必须非零且结构完整；Context issuerID/digest 必须等于 supplied verified identity，票据 interval 包含于 issuer interval。签发不自行设置 context/时间或接受 root 私钥、不提供权威 clock；消费 identity 的可信调用层仍需再验活性及 current authority。

验证接口 `(*ManagementVerifier).VerifyExecStartTicket(ticketWire,issuerWire []byte,expected ExecStartContext,descriptor ExecutionDescriptor,now time.Time)(ExecStartEvidence,error)`：验证 expected 合法；fresh verify issuerWire、必须其 ID/digest 与 expected 一致；strict decode ticket、固定 purpose/interval、actual Context==expected，digest==actual opaque descriptor.Digest；interval 落在 issuer cert 内；实际 issuer delegate Ed25519Verify 固定 domain。保守Δ1秒：now-Δ>=NotBefore、now+Δ<NotAfter，因 NotAfter<=ExpiresAt 同时拒绝业务过期新启动。证书 fresh 与实际票据 fresh 都必须通过；zero/nil verifier、zero descriptor/evidence、错误 context 不能 panic 或返回 partial evidence。无 historical start 或 renew API。

ExecStartEvidence 私有 copied wire、digest、context、descriptorDigest、notBefore/notAfter；getters Wire()[]byte, Digest()string, Context()ExecStartContext, DescriptorDigest()string, NotBefore()/NotAfter()time.Time。它证明签名、payload/context/time match；不证明 issuer durable active、不证明 native Lease live、不证明 intent committed、gate open 或 target execution。

## 故障与规模要求

同 cert ID 不同 digest、同 runtime UID 不同 boot、任何 original revision/lease/gate/context/digest 改动均拒绝。仅凭 issuer identity 不能构造有意义的 zero ticket；测试要独立签无效 claims/换 domain，不让 signer 的 prevalidation 掩盖 verifier 缺陷。有效旧 certificate/birth/runtime receipt signature 不能变成 start authority。

这些纯函数无 RPC、Lease、goroutine/ticker/watch、env/default读取或 wall-clock fallback。每个实际请求处理 argv/env/stdin 是 O(payload)，不存在针对 idle N 的后台刷新。计量 descriptor metadata/ticket/issuer wire 的实际字节，声明不含 stdin/stdout 存储；effect history λ×retention、未知预算和证书激活引用由后续集成计入，当前测量不能证明总预算/真实容量通过。

## 后续强制接线条件

签 ticket 前将 original operation（parent/lost/olddeadline/whole fences/guard/token/receipt/Lease/CreateRevision）与 controlledUTC observation 重检；durable effect intent 必须 known committed 后才发 transport，未知时保留原 CommandID/ticket/digest 查询而不换 payload。target 验 active issuer exact digest/key、真实boot/closedgate、实际payloaddigest 和 controlledclock，持久记录 accepted/terminal。SafeEnd 的 terminal 回执/精确删 token 与原 Lease revoke、accepted-only renewal、关闭/排空均另有明确协议。现有新身份原语和此 wire 通过不允许提前切换 image/default Manager 或用户 raw entrypoint。

Self-review: context 参数使用 RequestID validID 而非 UUID 与现有 Operation 相容；所有 actual ExecRequest 选项 TTY/network/stdin 已绑定；stdin 明确1MiB与当前 API 上限一致，完整 args/env metadata 有公开 failclosed cap；UID0拒绝不宣称完成 Linux 权限；start-only 与后续 renew/historical 不混用；纯签名原语无运行时授权入口。
