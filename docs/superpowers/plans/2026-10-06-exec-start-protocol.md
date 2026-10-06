# 最终执行参数与 start ticket Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox syntax for tracking.

**Goal:** 固定真实 exec payload 摘要及原 operation/exact runtime 有界 start-only 签名证据，为后续 effect/launcher 接线提供原语。

**Architecture:** Task1 immutable typed descriptor；Task2 使用现有 ManagementVerifier 与 strict codec 验证独立 issuer delegate start signature。只改新文件，不改 publication、management身份、etcd、Manager/config。不能作为独立执行 permit。

**Tech Stack:** Go stdlib SHA256/Ed25519/JSON/path/sort/time；现有 testify 与 controlprotocol helpers。

**Spec:** `docs/superpowers/specs/2026-10-06-exec-start-protocol-design.md`，当前已授权整体架构的窄依赖细化，连续 SDD/小提交。

## Global Constraints

- Task1 domain `sandbox-exec-descriptor:v1\x00`；Task2 domain `sandbox-exec-start-ticket:v1\x00`，实际 NUL。Version1，purpose=operation_exec_start；不提供 renewal/historical/file/task 通用授权。
- argv1–256、argv[0]非空、每项<=65536B；env0–256、key1–256B ASCII `[A-Za-z_][A-Za-z0-9_]*`、value<=65536B；字符串 UTF8/无NUL。UID/GID1..2147483647，WorkDir1..4096B POSIX absolute clean；timeout1..3600s；stdin<=1048576B binary；TTY/network都参与摘要。
- canonical JSON metadata 固定字段顺序 version,kind,argv,env,uid,gid,work_dir,timeout_seconds,stdin_length,stdin_digest,tty,requires_network；kind=exec；env按key排序非nil name/value数组，argv非nil；stdin不内嵌；metadata<=65536B。nil/empty env/stdin同义，getter/input全部深拷贝，错误 zero evidence。
- Context 完整 exact binding/certID+digest/CommandID/OperationID/RequestID/OperationDigest/SandboxID/WorkspaceHash/Generation/DataGateEpoch/ControlRevision/AdmissionRevision/LeaseID/Runtime/ExpiresAt。cert/Command/Operation UUID；Request/Sandbox validID；hash64hex；五int64>0，UTC expiry。
- ticket<=4096B；strict required snake_case/duplicate/unknown/null/trailing/invalidUTF8/surrogate/int/UTC Z；signature64B，privatekey consistency/copy，匹配 issuer delegate；fresh pinned issuer verify + fixed actual delegate signature。
- Δ1s now-Δ>=NotBefore、now+Δ<NotAfter；UTC非零有序、interval<=30s、NotAfter<=businessExpires；ticket interval须包含于issuer cert。纯协议now来自可信层，无fallback。
- 只归因签名/payload/context/time，不证明durableactive/capability/intent/gate/terminal。无RPC/Lease/ticker/后台goroutine/新dependency/Manager/image切换；保留Sentinel编辑、不读.env或生产凭证、无push/merge/deploy。
- TDD实际behaviorRED、focusedGREEN、affectedfullpackage once、自审即本任务commit；随后独立spec+qualityreview。必要修复单独commit。

## Review Focus

1. 同 issuer cert ID 但 digest/key 不同，或 valid runtime/birth 角色换成 issuer/start，不能跨角色通过（Task2）。
2. 同 op/public context 下 payload 任一真实选项，包括 stdin二进制、TTY/network，改变必须拒绝；不采用自述 digest（Task1/2）。
3. admission/control revision、Lease、gate、BootID 任一变动拒绝；业务过期后不能用尚有效 issuer cert 开新操作（Task2）。
4. 输入/getter alias、nil/empty canonical 与 env顺序、超限 boundary 不影响已创建 descriptor 或证据（Task1/2）。
5. malformed wire、等号时间、zero/nil、wrong private key/domain 独立签名输入，不能 panic 或返回partial evidence（Task2）。

## Task 1：immutable canonical exec descriptor

**Files:** Create `internal/runtime/controlprotocol/execution_descriptor.go`, `execution_descriptor_test.go`。

**Interfaces:** `ExecutionRequest {Argv []string;Env map[string]string;UID,GID uint32;WorkDir string;TimeoutSeconds uint32;Stdin []byte;TTY,RequiresNetwork bool}`；`NewExecutionDescriptor(ExecutionRequest)(ExecutionDescriptor,error)`，private fields 的 value evidence getters `Digest()string`, `Canonical()[]byte`, `Request()ExecutionRequest`。Task2消费 nonzero descriptor.Digest，不能构造其私有状态。只使用 stdlib，不加 runtime package dependency。

所有实际值与 canonical规则取 GlobalConstraints/spec；metadata json.Marshal 专用 struct，不用 map 序列化，不在 canonical 内放 stdin。hash actualstdin length/content，不接受caller digest。copy须在有效返回前完成；getter各自重新copy。不做default/envinherit/pathnormalize/shell处理。

- [ ] 写 `TestExecutionDescriptorCanonical`：独立建 expected canonical JSON/域 SHA256，nil/emptyenv/stdin同义，map插入顺序同义，二进制stdin bytes精确；`TestExecutionDescriptorBindsPayload`逐个argv/env/UID/GID/WorkDir/timeout/stdin/TTY/network改变digest；`TestExecutionDescriptorCopies`修改源与getter后原值及digest不变、并发getter。
- [ ] 写 `TestExecutionDescriptorRejects` 表：zero/too many args/env、emptyargv0、oversize/NUL/UTF8/key/uid/gid/dirtyrelativepath/timeout/over1MiBstdin/>64KiBmetadata，全部error+zero证据；合法等号边界（含1MiB stdin与可在64KiBmetadata内实现的边界）与空后续arg合法。limit fixture按实际marshal尺寸计算，不误称65535B为超限。
- [ ] `go test ./internal/runtime/controlprotocol -run TestExecutionDescriptor -count=1` 观察实际行为RED（可先只声明error stub），随后最小实现。
- [ ] focused GREEN，再 `go test -race ./internal/runtime/controlprotocol -run TestExecutionDescriptor -count=1`、一次 `go test ./internal/runtime/controlprotocol -count=1`、package vet/gofmt/diffcheck；自审并报告真实canonical字节样本、全拷贝getter/noidleN成本。
- [ ] 仅stage两个文件立即commit `feat(controlprotocol): bind execution descriptors to actual payload`；完整report给controller，独立spec+quality通过再Task2。

## Task 2：strict original-operation start ticket

**Files:** Create `internal/runtime/controlprotocol/exec_start_types.go`, `exec_start.go`, `exec_start_test.go`；不改Task1/management/publication文件，复用现有strictcodec。

**Interfaces:** types/getters及fullcontext严格遵照spec；`SignExecStartTicket(ed25519.PrivateKey,CommandIssuerIdentity,ExecStartTicketClaims)([]byte,error)`；`(*ManagementVerifier).VerifyExecStartTicket(ticketWire,issuerWire []byte,expected ExecStartContext,descriptor ExecutionDescriptor,now time.Time)(ExecStartEvidence,error)`。

Context的所有字段有requiredsnakecase tag，作为claims.context nested固定schema，Runtime沿用runtimeSchema；Claims fixed version/purpose/context/descriptor_digest/not_before/not_after。envelope onlyclaims/signature。同instant UTC时间比较不凭Time.location指针identity。Signer不能修改context/derive时间；验证suppliedverifiedissuer非零、私钥匹配、issuerID/digest与interval，key复制 consistency。Verifier重新fresh验证issuerwire，不信历史identity，先strictclaim/context并对全部expectedcontext比较、actualdescriptor.digest，后实际Ed25519Verify固定domain；保守nowwindow。evidence规范copiedwire/digest/context/descriptorDigest/times，错误zero。

- [ ] 写 `TestExecStartTicket`真实root->issuer->delegate->ticket roundtrip，payload/context/时间getters和wire复制并发；输出issuer/ticket/metadataactualsize，不存stdout/stdin。`TestExecStartTicketRejects`逐一expected字段差异与payload选项差异、nil/zero/privatekeymalformed/不匹配、fresh cert过期/ removedroot、Δ等号/非UTC/业务E/30s/issuerinterval、overwire/strictJSON malformed 与无partial evidence。
- [ ] `TestExecStartRoleSeparation` 独立root/delegate重新签合法或非法claims，crossdomain/certificate roles/相同certID不同key/digest/startpurpose伪renew拒绝；不是仅依赖signer prevalidation来测试verifier。所有ReviewFocus在本task明确落test。
- [ ] `go test ./internal/runtime/controlprotocol -run 'TestExecStart' -count=1` 观察behaviorRED，最小实现后focusedGREEN。
- [ ] 一次完整 `go test -race ./internal/runtime/controlprotocol -count=1`、packagevet/gofmt/diffcheck，自审即stage本task3文件commit `feat(controlprotocol): authenticate bounded operation exec starts`；独立spec+qualityreview。
- [ ] controller冻结head，全仓test（无fixtureexplicit）/vet/build必要并行checks；etcd源未变不重复nativefaultsuite，保留216PASS实际native证据。完整branchrangefinalreview、一次发现fixwave+scoped复核，逐项保留rulings/declined/未完overall要求；然后继续effect intent接线。

## Self-review

Task1 producer与Task2 consumer API逐字一致；RequestID遵循已有operation的validID；五int64正值均测试，UTC边界与CertID/digest双绑定明确。stdin1MiB贴近当前API，argv/envmetadata64KiB是协议公开failclosed limit，后续adapter需清楚返回错误。WorkDir lexical不冒充ScopedFS；root拒绝不冒充管理UID隔离。Task2未声明pureidentity变cap；effect/currentClock/targetactive/lease/gate/drain缺一都不能部署切换。两task共享ExecutionDescriptor只读接口，第二task不改第一文件，两个task各自是可拒绝的独立原语增量。
