# Enrolled Runtime Publication Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox syntax for tracking.

**Goal:** 真实 Supervisor 在用户准入前完成 node observer 登记，再将严格 v2 ready、登记材料与 runtime 发布原子绑定到原始 CreationClaim，供后续终止独立核验。

**Architecture:** 显式 immutable enrolled profile、新签名域、单原始 enrollment owner 与实际关闭等待；Backend 使用保留全部 Stage/claim/history pins 的私有64节点事务与固定只读仲裁。原有 legacy API/wire 保持，enrolled Backend 拒绝旧发布入口。

**Tech Stack:** Go1.25.6、Linux arm64/amd64、Ed25519、现有 etcd3.6.15 和 node observer；不升级依赖。

**Spec:** docs/superpowers/specs/2026-10-15-enrolled-runtime-publication-design.md

Final finite journal-install verification pauses the actual final trusted-clock observation after the local open gate was durably persisted. CloseAdmission must prevent installation; Close must cancel but still join the original borrower before key clear. Preserve old poisoned-journal semantics: cancellation may retain diagnostic open bytes, then actual cold OpenClosedJournal must persist CLOSED and refuse a fresh InstallActivation. The two new native modes reuse the existing fixture and do not replay positive/backend matrices or change Journal production code.

## Global Constraints

- 一个完整 Task、一个 fresh implementer 和唯一 fresh reviewer；内部验证后及时小步提交，不能等待全部完成才大提交。按既有授权连续执行，不重复询问继续。
- 新 profile 只支持实际 plain/no mount，拒绝 FUSE；不接生产 launcher/main/manifests，不重复 observer producer，不实现 termination/provider cleanup/release 或 Redis 切换。
- 保留全部 base4+claim24+history18+Stage guard3+receipt absence1，业务 absence4；commit54比较+9写+1失败读=64 literal nodes。namespace≤512、key≤1024；每个 native request/success/failure response 独立≤262144字节，写前完整预检。
- 两条 enrollment 记录各≤8192、combined≤16384，proof≤8192；严格 canonical/version2、Lease0、Create=Mod、同 commit revision。实际超界的 legacy activation 在任何 RPC 前拒绝。
- shared prepareMutation/commitPreparation/CommitStage/旧 record validators 不改；新私有域 driver 保留 guard Lease/receipt/unknown/cleanup 协议，紧凑失败分支不得交给旧 parser。
- fresh5秒 ready interval、原始 live claim、真实 root-certified identity/contract 与跨证书公钥全相等；不能以 caller bool、Pod annotation 或复制结果替代能力。
- 一条 Supervisor enrollment owner；Backend active owners1..32，共同 Close seal 后实际 join 才释放 socket/key/client。无锁跨 clock/IO/Close/Wait，无按已有 N 的 map/actor/prefix 发现。
- 只 apply_patch 修改源码/测试/文档/格式，不用 cat/heredoc/Python 写内容、不用 gofmt-w；raw JSON/log/binary/source archive/diff 可由工具生成。
- 不碰 Sentinel、不读 CLOSED OWN、不复用 accepted 作者/reviewer、不重放已验收整组；不读真实secret/完整Docker inspect、不在线访问 Redis/Kubernetes、不升级依赖或 push/deploy。
- Git mutation 先向 Root 申请 expected HEAD+exactpaths 窗口，再持 .git/codex-etcd-commit.lock→本 OPEN OWN/git-commit.lock，fcntl LOCK_EX；确认 branch/HEAD/空index/exactstaged/diffcheck/commit/postempty。
- nativefixture 总数≤2，每作者≤1；跨包编辑需与 observer 作者协调 GoList/compile/postSHA quiet window。保留实际原 child Cmd.Wait、attach/containerWait、源码/binary SHA 与 exactlabel 清理，失败不得改为 PASS。
- 实际 observer precursor 必须先验收，才可验收本 Task 的组合链路；前期独立协议/Backend/host 实现可以并行，不以未部署为阻塞或伪造实际成功。

## Review Focus

1. 不同证书分别有效但 runtime/publickey/contract/creation intent 不一致，以及 old Activate/Publish 对新 profile 的旁路。
2. CloseAdmission/Close 与 durable ACK/journal 安装交错；晚 ACK 不能重新开放，key clear 必须晚于真实 owner/transport join。
3. 未知 Grant/guard/commit 后，同 attempt 被重复使用或创建新 Stage；原始 reference 与只读冷仲裁必须保留。
4. 历史证书已过期但合法原始签名仍能诊断，restore/cluster/identity 变化仍不得用旧 receipt 判 committed。
5. 最大转义 wire 与64节点、两个分支响应；Backend.Close 阻塞期间登记保持有界、禁止新调用。

---

### Task 1: Trusted enrollment through atomic publication and cold lookup

**Files:**
- Create protocol: internal/runtime/controlprotocol/enrolled_runtime_publication.go, enrolled_runtime_publication_test.go。
- Narrow modify protocol: publication_types.go、publication_verify.go，仅 copied CertificateEvidence.PublicKey backing 与新 bridge 所需既有私有 primitive；旧验证语义不变。新 bridge/codec 尽量放新文件。
- Create supervisor: internal/runtime/controlrunner/supervisor_enrollment.go、supervisor_enrollment_test.go、supervisor_enrollment_native_linux_test.go。
- Narrow modify supervisor: supervisor.go、supervisor_types.go、supervisor_activation.go、supervisor_failure.go，仅 immutable constructor profile、实际 activation 分派和 Close/CloseAdmission/key lifetime 接点。
- Narrow credential lifetime seam: internal/runtime/controlprotocol/management_tls_types.go、management_tls.go；新增 management_tls_lifetime_test.go。Credential.Close seals/clears its sole copied private key under RWMutex; a new private credential-backed leaf signer uses bounded locked access without copying key bytes. Existing tlsOnlySigner/node-observer behavior stays unchanged. Supervisor calls Close after actual joins.
- Create etcd: internal/storage/state/etcd/runtime_enrolled_publication.go、runtime_enrolled_publication_types.go、runtime_enrolled_publication_records.go、runtime_enrolled_publication_stage.go、runtime_enrolled_publication_budget.go、runtime_enrolled_publication_read.go、runtime_enrolled_publication_test.go、runtime_enrolled_publication_fault_test.go、runtime_enrolled_publication_native_linux_test.go。
- Narrow modify etcd: client.go、creation_claim.go、runtime_publication.go、task_archive_delivery_types.go、task_archive_delivery.go，仅 immutable config、frozen attempt、legacy early refusal、same-mutex bounded active owner/Close 实际 join。
- Native fixture: 新 scripts/test-enrolled-runtime-publication.sh，优先复用 observer 已验收的有限真实 fixture/helper，不改其 producer 实现。其他必要文件先给 Root 精确职责裁决。

**Interfaces:**
- Produces NewEnrolledSupervisor(SupervisorOptions,NodeEnrollmentOptions) (*Supervisor,error)、(*Supervisor).Enrollment() (controlprotocol.EnrolledRuntimeEvidence,error)、EnrolledPublicationProof(context.Context,[]byte,controlprotocol.ClaimReference) ([]byte,error)；精确 profile/options/証据字段见 Spec。
- Consumes OpenNodeEnrollment/Enroll 的真实 UDS、root 验证的 activation/runtime/issuer/observer cert，genuine private s.key 与实际 Journal。构造者和 clock 不接受成功 bool 或 arbitrary signer。
- Produces Backend.PublishRuntimeEnrolled(context.Context,*CreationClaim,[]byte,controlprotocol.EnrolledRuntimeEvidence) (RuntimeEnrolledPublishResult,error)、LoadRuntimePublicationEnrolled(context.Context,WorkspaceIdentity,string) (*RuntimeEnrolledPublicationEntry,error)、ReadRuntimeEnrolledPublicationAttempt(context.Context,StageReference) (Outcome,error)。
- Consumes existing opaque CreationClaim、原 nine preparation KVs、current namespace/identity/restore、现有 controlled clock/trust 与 native checker。旧 Stage driver 不接扩展 mutation。
- Later termination 重新加载并比较 immutable enrollment，不消费本 Task copied boolean/reference 授权。

- [ ] **Step 1: Strict v2 wire and cross-certificate bridge RED→GREEN.** 新 TestEnrolledPublicationProtocol 验证 genuine key/newdomain，旧 verifier 拒绝 v2；分别合法但公钥/BootID/intent/claim/contract 不同仍拒绝；unknown/null/duplicate/noncanonical/oversize 全拒绝。先保留 RED，再实现 fixed codec/新 private verifier 和 copied key，host race GREEN 后小提交。
- [ ] **Step 2: Actual Supervisor enrollment owner RED→GREEN.** 新 TestEnrolledSupervisorAdmission/CloseAdmission/Close/Ready 验证构造 profile 不可降级，Activate/bootstrap 同路径，durable ACK 前用户执行拒绝；晚 ACK/安装不能重新开放；实际阻塞 socket/session IO 必须 join 后才能清 key。实现私有 owner 和真实 UDS/private signer，finite host/Linux 检查后提交，不导出任意 signing/key accessor。
- [ ] **Step 3: Fixed records and native owner/Stage.** 新 TestEnrolledPublicationRecords/Bounds 验证两条≤8192及同 commit 关系、54+9+1、最大 namespace/escaped records/每 native 分支 byte checker。实现 strict v2 records、MaxActive1..32、creation claim 上冻结原始 attempt、private guard Grant/commit/cleanup、same-mutex Backend.Close 登记。所有预检在 Grant 前，host GREEN 后提交。
- [ ] **Step 4: Failure and cold arbitration.** 新 TestEnrolledPublicationFaults/Read 验证每 history pin、claim loss、malformed header/cluster/response、leased/partial/mutated group、原始 reference 保留。紧凑失败响应专用 validator，再12节点 base-fenced fixed reads；lost ACK 重试零 Grant/Put/Delete/KeepAlive，absent Unknown，不伪造 abort。旧 API 新 profile 在 RPC 前拒绝。加入 Backend.Close 实际 blocked RPC join；GREEN 后提交。

  Cold locator refinement: v2 journal retains DispatchAttempt/BindingAttempt/MountAttemptLocator (`dispatch_attempt`/`binding_attempt`/`mount_attempt_locator`) from original preparation records, preserving uint8 `mount_attempt`. These only locate receipt keys; one coherent snapshot revalidates all locators and nine pinned KVs. Account actual expanded record bytes before Grant; no extra prefix/discovery RPC or comparison removal.
- [ ] **Step 5: Real native integration.** observer precursor 验收后，TestEnrolledSupervisorNative 与 TestEnrolledPublicationNative 在明确 owned Linux fixture 使用真实 namespace PID1 Supervisor、private key、observer UDS、native TLS etcd，检查 blocked durable ACK→零用户 child、放行后 genuine ready/真实受控用户执行、wrong peer/nonce/nodeboot 拒绝、Close 实际 join。撤销 creation claim 后不可 active；真实 commit ACK 丢失后冷读同 receipt，trace 无第二写；restore/cluster 不符拒绝。每实际源码 epoch 只跑新增有限组，不重放 observer 或 local-sync 旧矩阵；分批通过不称完整容量。

  Actual observer pending barrier is its second provider GET, followed by real durable ACK/ready/publication/user execution. Exact late-reply/Close/CloseAdmission scheduling uses a real UDS unit peer with fixed signed wire and only proves local lifetime/admission. Do not label that unit peer a physical observer or actual post-fsync pause. Reuse the frozen producer fixture helper; no UDS proxy or new producer hook. The same isolated TLS etcd fixture may run a second package-etcd test ELF with its own exact-prefix App identity for actual ACK drop/claim loss/cold reads; preserve independent source/binary manifests and every original child Cmd.Wait. Observer's read-only identity stays read-only.
- [ ] **Step 6: Scope gate and acceptance.** 只运行新增 host race/必要受影响兼容、affected vet、Linux 编译、gofmt-l/bash-n/scoped diffcheck；report 列源码epoch/native request与每分支大小/实际Wait/失败与限制。Root 生成只含本 Task commits 的唯一 review 包，fresh reviewer 一次完整 gate，同作者/reviewer 限定修复。Root 报告、验收提交、关闭 OWN 后继续终止 composer/provider cleanup/owner release；不结束 master branch。

Finite host command: /Users/dysodeng/go/go1.25.6/bin/go test -race ./internal/runtime/controlprotocol ./internal/runtime/controlrunner ./internal/storage/state/etcd -run '^(TestEnrolled)' -count=1；选出的 Linux-only native 不能在 Darwin skip 后被记为通过。native runner 必须记录实际 selector/零误skip、binary/source SHA、original waits 与 exact inventory。

## Preflight and scope

Spec decisions 覆盖 protocol→Supervisor→native Stage→历史 lookup→实际组合；同一 Task 的六步骤共用接口。两种 certificate 的 authority 不混淆，旧 validator 不扩域；prepareMutation 的14节点保留限制由新私有 driver 解决，compact Else 使用新 parser，CloseAdmission late ACK 与原始未知 attempt 各有实际测试。所有记录和当前/历史 guard 都在64内；实际 bytes 仍须实现预检及原生测试，不把设计计数当验收。当前 host Darwin，native fixture 受已检验隔离/capability 条件约束；没有真实证明则本 Task 不接受。生产部署/FUSE/终止/owner释放仍待后续，不重复申请用户已授权的 master 工作。
