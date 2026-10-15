# Local Runtime Safe Release Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox syntax for tracking.

**Goal:** 接通真实本地同步完成、独立物理终止、精确 provider 清理和64节点原子 owner 释放，并支持物理退出后的冷恢复诊断。

**Architecture:** 一个完整功能 Task。复用已验收来源，新建 bounded Backend owner、固定原始命令/七槽清理状态及具体 Kubernetes one-request client；结果不来自 caller bool。保留所有当前/历史原生屏障，不把子组件测试当生产或容量验收。

**Tech Stack:** Go1.25.6、现有 etcd3.6.15/client-go/xsys/Ed25519、Linux arm64/amd64，无新依赖。

**Spec:** docs/superpowers/specs/2026-10-16-local-runtime-safe-release-design.md

## Global Constraints

- 仅 ordinary Kubernetes/local sync/enrolled plain no-mount；拒绝 legacy/Docker/FUSE/cloud/none/未知资源合同。main、生产manifest、Redis切换不在本单元，仍属master后续。
- 固定50 baseline+whole operations-prefix1；strict local complete4。仅封闭/join后的三条live sync-budget pins不再保留；不删其他现有pin。
- reserve含两enrollment MOD与provider MOD共60节点；terminal guard/CAS60，带provider MOD61；final release60比较+3delete+1put=64。每条实际事务必须自行列节点，request/EACH branch≤262144，namespace≤512/key≤1024，不提高限额。
- termination16KiB、provider record16KiB、immutable owner receipt8KiB，固定三个Task键；provider七effect slots，无每attempt新键或caller程序。unknown原归属不覆写、不重发、不按TTL清理。
- Backend active1..32、Timeout正值≤30s；concrete provider active1..32、request≤5s、body≤262144。无锁跨clock/IO/Close/Wait，原操作实际join后再释放引用/额度；published TLS containers immutable。
- 不使用旧 bool fencer、Pod404/EOF/selfACK作为物理死亡证明；不消费copied diagnostics作为权限；不修改已验收producer/wire/validator来绕过预算。
- 不读CLOSED OWN、不复用accepted作者/reviewer、不碰Sentinel、不在线访问真实Redis/Kubernetes、secret/完整inspect；不升级依赖、不push/deploy。
- 只apply_patch写源码/测试/文档/格式；raw JSON/log/binary/sourcearchive/diff/brief例外。禁止cat/heredoc/Python内容写入和gofmt-w。
- 每次Git先Root expectedHEAD+exactpaths窗口，再common→OPEN OWN fcntl锁，branch/HEAD/initialempty/exactstaged/diffcheck/postempty；达到小步验证条件及时提交，不等整Task完成。
- 每作者≤1/总≤2nativefixtures；GoList→compile→postfilename/SHA保持实际依赖closure quiet。保留原Cmd.Wait/attach/containerWait、binary/sourceSHA和exactcleanup，不补造历史Wait。
- 本组合Task一个fresh implementer、一次唯一fresh完整Taskreview，fix同pair限定，不重放已验收旧矩阵。第3步provider具体客户端在6160c36接口固定后，改由独立并行依赖计划 `2026-10-16-task-runtime-cleanup-client.md` 的作者独占实现/验证和唯一审查，组合作者不重复写这些文件；Root协调Git和实际编译closure quiet。前置 enrolled-publication Task验收后才能开始本Task产品实现。

## Review Focus

1. 原 runtime 已死、old claim/issuer已成为历史时，恢复不能要求它再次提供活响应，也不能把旧claim恢复成sender。
2. 同值put/lease/recreate的enrollment依赖变化与provider UID替换，所有既有fence不丢、无误删新对象。
3. TLS/HTTP/client-go隐式retry/redirect与超时仍运行的原请求，实际wire只发一次、真实join才释放。
4. 每个native/API ACK gap与fresh claim、原命令unknown不可覆写/重发；owner absence不等于release。
5. 原子release之后new owner已获取，cold resolver仍验证原receipt，不依赖已经删除的owner/control/index或ClaimTask重建。

---

### Task 1: Local completion through exact runtime cleanup and atomic release

**Create Backend files:**
- `internal/storage/state/etcd/task_runtime_termination_types.go`：Options/issuer/diagnostic results、fixed private owner与attempt。
- `task_runtime_termination_records.go`：复用sharedintent，strict provider state/历史bindings。
- `task_runtime_termination_authority.go`：独立cold prerequisite/authentication、完整50pins、complete/两enrollment MOD。
- `task_runtime_termination.go`：公开TerminateTaskRuntimeLocal、reserve/sign/send/query/terminated/provider顺序。
- `task_runtime_termination_local_binding.go`：窄public BindTaskWorkspaceArchiveLocal原始received→readonlytarget→原bind→实际Close编排，仅回slot诊断，支持空目录公开fullpositive；不修改旧source/store/validator逻辑。
- `task_runtime_termination_provider.go`：七slot native写前意图、具体client调用、readonly reconciliation。
- `task_runtime_termination_read.go`：LoadTaskRuntimeTermination、旧Current与freshclaim恢复。
- `task_owner_release.go`、`task_owner_release_records.go`、`task_owner_release_read.go`：64节点release及不重建claim的冷receipt。
- new `task_runtime_termination_test.go`、`task_runtime_termination_native_linux_test.go`、`task_owner_release_test.go`、`task_owner_release_native_linux_test.go`。

上述 `task_*` 文件均位于 `internal/storage/state/etcd/`。实现者可按单一职责分拆这些新文件，但额外旧文件接点先给Root具体原因；不创建可任意拼比较器/动作的框架。

**Narrow modify:** etcd `client.go`、`task_claim.go`、`task_archive_delivery_types.go`、`task_archive_delivery.go`，仅显式配置/原始attempt字段/共用Close seal和实际join。复用既有private固定codec，不修改已验收local finalization/observer/publication产品逻辑；必要新接点先Root裁决。

**Provider dependency (独立作者独占):** `internal/runtime/kubernetes/task_runtime_cleanup.go`、`task_runtime_cleanup_transport.go`、`task_runtime_cleanup_records.go`、`task_runtime_cleanup_test.go`。接口已在6160c36固定；具体实现和独立验收由并行依赖计划交付，组合作者只消费其opaque descriptor/observations，不复制实现。严格复用ownership validator，不调用bundled cleanup。组合作者仍拥有新真实组合fixture `internal/runtime/controlrunner/local_runtime_release_native_linux_test.go` 和 `scripts/test-local-runtime-safe-release.sh`，只复用既有有限helper。

**Interfaces:**
- Backend四个public方法、LocalRuntimeReleaseOptions/固定issuer、concrete provider构造/Prepare/Descriptor/Observe/Apply/Close的精确签名与边界见Spec2–5/17–20。
- `TaskRuntimeCleanupStep` 固定七值：install-retain-finalizer、delete-pod、delete-network-policy-logical、delete-network-policy-runtime、delete-cilium-policy-logical、delete-cilium-policy-runtime、remove-retain-finalizer；相同对象去重。Binding使用Spec18具名身份字段和runtime triple，descriptor/observations为copied bounded diagnostics，不能输入kernel结果。
- 消费 actual complete、current opaque TaskClaim、verified enrolled publication、TerminationDestination、真实Pod/Node/policy point API。只读entry/result不授予效果。

- [ ] **Step1 strict codec/owner/config RED→GREEN并小提交。** 新 `TestLocalRuntimeReleaseRecords/Profile/Owners/Bounds` 拒绝nil/typednil/profile降级、unknown/null/duplicate/超界/phase篡改，要求原始Current不可改、七slots固定、MaxActive/Close实际join。先保留RED；实现类型/codec/同mutexowner，不先扩generic Stage。
- [ ] **Step2 retained completion与完整native authority。** 新 `TestLocalRuntimeReleaseAuthorityNative` 独立加载真正complete和历史，拒绝fake/copied结果、foreign root/generation/runtime/node/restore/claim；两enrollment key各同值put/lease/delete-recreate均使reserve拒绝且零外部效果。列实际60/61和wire上界，限定GREEN后提交。
- [x] **Step3 接受独立并行provider dependency。** `2026-10-16-task-runtime-cleanup-client.md` 交付具体one-request client及有限HTTP/TLS拒绝/重试/真实Close测试。三个小实现提交及e2aa59e审查修复已范围验收，原host限制如实保留，见 `2026-10-16-task-runtime-cleanup-client-verification.md`。组合作者不重复执行组件矩阵；Step5和Step7仍必须验证写前意图、真实顺序及整链路，不能以独立client测试替代。
- [ ] **Step4 termination组合与cold恢复。** 新 `TestLocalRuntimeTerminationNative` 用当前任务/原archive issuer reserve/sign/唯一send，observer真实proof后持久terminated；每个native/send ACK gap同attempt只读，禁止resignal。fresh claimant处理已退出runtime时不调用旧runtime RPC，保持原Current与unknown归属；无proof则pending。GREEN后提交。
- [ ] **Step5 七槽精确cleanup。** 新 `TestLocalRuntimeCleanupNative` 对每个真实API mutation写前意图+fresh nativeguard，原UID/RV与policyownership精确；finalizer/DELETE不确定、替换对象、其他finalizer、unsupportedmount等均不release。完成最后artifact观测才CAS cleanup-complete并pin实际provider MOD；每crashgap有限验证，GREEN后提交。
- [ ] **Step6 原子release与冷诊断。** 新 `TestLocalOwnerReleaseNative` 证明实际60compares+3delete+1put，任务/fence/history保留；未cleanup-complete拒绝。原release lostACK、新owner已获取后LoadTaskOwnerRelease仍确认原receipt，absent不算success，不调用ClaimTask、不重发删除。最大namespace/record/protobuf请求和两分支独立预检GREEN后提交。
- [ ] **Step7 real full-chain positive。** 新 `TestLocalRuntimeReleaseNative` 通过public APIs组合 genuine enrolled Supervisor、真实archive/空目录local finalization、真实observer pidfd/cgroup、TLS etcd与精确HTTP provider，直到owner释放/新owner后冷读；记录原全部child/IOWait。不能seedcomplete/使用callerproofbool或只跑metadata。有限fixture能力缺失要报告真实限制，不假PASS。
- [ ] **Step8 final affectedgate/selfreview。** 新 `^TestLocalRuntime` / `^TestTaskRuntimeCleanup` 有限hostrace、affectedvet、Linux compile、gofmt-l/bash-n/scoped fullBASEdiffcheck；不重放旧矩阵。报告逐commit/epoch/selector/实际node-byte/Wait/sourcebinary/失败与限制，并列40KiB新payload非MVCC/RSS容量结论。GREEN即小提交，不将整Task压成一次。
- [ ] **Step9 sole Taskreview/验收。** Root生成只含本Task commits的完整diff给唯一freshreviewer，统一findings、同pair限定fix。完成后跟进生产接线/其他profile/调度/FUSE/容量部署，不把本单元称master结束。

## Root self-review / execution handoff

这是既定master安全回收的一个完整可拒绝单元；Step1–6是连贯源码提交，不能各开独立review任务。provider state采用固定MOD时必须先独立严格认证原value/envelope，原50pins/complete4/operations1不变；actualreserve含provider MOD是60而非旧草案59。Finalrelease不添加未预算providercompare，因为其digest已在exact cleanup-complete termination中，后续效果准入永久封闭。coldread不借用c.owner/control。若具体源接口/最大wire出现矛盾，向Root提出精确裁决并继续独立部分，不擅自弱化。
