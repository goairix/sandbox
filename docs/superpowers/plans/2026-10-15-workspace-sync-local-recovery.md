# Workspace Local Recovery Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox syntax for tracking.

**Goal:** 一个功能单元完成cold bound归档在fresh claim下安全继续local regular-file同步。

**Architecture:** 公共执行wrapper持有独立私有recovery owner/source/body/charge/completion，复用已有native schema、target/cursor/local机械IO；checkpoint保留旧归属，新效果由新权限约束。

**Tech Stack:** Go1.25.6、Linux arm64 CGO0/x/sys/unix、三成员etcd3.6.15，无依赖升级。

**Spec:** docs/superpowers/specs/2026-10-15-workspace-sync-local-recovery-design.md

## Global Constraints

- Linux recovery+recycling opt-in、只cold bound local；旧API/validator不放宽，不填旧draft或重造Prepared/Received/witness。
- 固定64 archive slots每槽1owner，既有Jobs/disk/I+4M收费不放大，不退款/不retire；S1..64逐点扫描，无N map/actor/prefix全扫。
- store.mu/archiveLife.mu/nativeMu不跨物理IO/query/join；metadata gate登记/cancel/实际Wait，root锁最后关闭；无PID/flock-alone或replacement-root授权。
- 完整50pins、≤64nodes/256KiB Txn/64KiB record；无新KV/attempt logs/编码变更；checkpoint旧TargetDigest，fresh claim/worker另pin。
- 只同步regular-file核验/repair；目录/delete/sync-complete/cloud/owner release/main/deploy/Redis删除另有后续，不冒充完成。
- 源码/测试/文档/报告/格式只apply_patch；不cat/heredoc/Python写上述内容、不gofmt-w；工具可生成raw/JSON/binary/brief/diff。
- 不读任何CLOSED OWN或无关Sentineldoc，不复用旧已完成作者/reviewer，不重跑已验收单元；仅新增/真实受影响兼容回归，源码未变不重复。
- OPEN OWN/git-commit.lock fcntl LOCK_EX覆盖expectedbranch/HEAD/空index/精确path/add/stagedcheck/commit/postempty；及时小提交，无push/deploy。
- 两独立Task共享branch时先持.git/codex-etcd-commit.lock，再持各自OPEN OWN锁，提交前与Root协调HEAD；源码保持跨tool边界可编译，不stage另一Task文件。

## Review Focus

1. 冷manifest只诊断，不能因结构匹配而重建旧发送/执行authority。
2. 同inode锁/所有原writer工作实际join前后，以及root replacement、namespace/boot改变。
3. lost terminal/GC ACK、同slot跨task/代复用，旧checkpoint claim更换与frozen TargetDigest。
4. currentclaim/BackendClose在metadata/query/读取/写入/fsync/postguard/native之间失效，无登记空窗。
5. 大归档/65文件时有界内存、cursor单调和native wire上限，无额外whole-archive-per-file复制/扫描。

---

### Task 1: Fresh-claim local regular-file recovery execution

**Files (etcd paths relative to internal/storage/state/etcd):**
- Create task_archive_recovery_authority.go、task_archive_recovery_query.go、task_workspace_sync_recovery.go、task_workspace_sync_recovery_types.go、task_workspace_sync_recovery_source.go、task_workspace_sync_recovery_intent.go、task_workspace_sync_recovery_checkpoint.go、task_workspace_sync_recovery_local_linux.go、task_workspace_sync_recovery_local_unsupported.go；分别负责历史pins、认证query、wrapper、私有owners、cursor/body、charge、terminal/GC、物理local执行。
- 可Create task_workspace_sync_local_io_linux.go、task_workspace_sync_archive_cursor.go、task_workspace_sync_native_wire.go作为所需机械共用段；未需要则不创建，不要求空stub。
- Modify task_archive_recovery_types.go/store.go、task_archive_spool.go：fixed slot登记和Close integration；task_workspace_sync_local_put_linux.go/types.go、task_workspace_sync_source_cursor.go/body.go：只提取必要机械executor/cursor，不改原sealvalidator。
- Modify task_workspace_sync_source_types.go/source.go仅安装机械共用cursor字段与初始化，不改原verify/validator/单次body门。
- 允许为共用native transport提取task_workspace_sync_recycling_settlement.go/gc.go中的非授权wire/response段；预算/原授权/原checkpoint规则保持。
- Create task_workspace_sync_recovery_test.go及task_workspace_sync_recovery_native_linux_test.go；必要时可拆受控fault/close测试文件。Modify task_archive_delivery_native_test.go仅fixture历史query/freshBackend设置；scripts/test-workspace-sync-local.sh只加本单元selectors。需要其他接点先报Root ruling而非静默扩大。

**Interfaces:**
- Public RecoverTaskWorkspaceSyncLocal(ctx,c,slot,destination,worker) (TaskWorkspaceSyncLocalRecoveryResult,error)，result为Checkpoint *TaskWorkspaceSyncCheckpoint、RegularFilesComplete bool、CompletedFiles/CompletedBytes uint64。
- pinTaskArchiveRecovery(ctx,c,manifest,*PreparedTaskQuiescence) (*taskArchiveRecoveryAuthority,error)；authority.local/guard(ctx) error；authority.queryProducer(ctx,destination) (controlprotocol.TaskWorkspaceArchiveReceiptEvidence,error)。
- prepareTaskWorkspaceSyncRecovery(ctx,c,slot,destination,worker) (*taskWorkspaceSyncRecoverySource,error)；source.begin/end/next/openBody/run/Close，body.Read/Close；类型独立于旧source/body/draft/witness。
- source.readCharged(ctx,slot) (*taskWorkspaceSyncRecoveryCharge,error)、reserveNext(ctx,body) (*taskWorkspaceSyncRecoveryCharge,error)、reconcileLocal(ctx,body,charge) (*taskWorkspaceSyncRecoveryCompletion,error)、restoreCheckpoint(ctx) error、settleLocal(ctx,charge,completion) error、recycleLocal(ctx,charge,completion) (Outcome,error)。
- 可抽私有executeWorkspaceSyncLocalPut(ctx,target,file,read,guard,completed)返回bounded物理结果；机械file/result类型作者固定并说明，不形成公共authority。

- [x] **Step 1: RED与接点核对。** 新cold bound archive调用在旧代码下拒绝/无法继续，明确记录feature RED而非编译失败。列实际50pin组装和worst wire预算、旧claim/新checkpoint关系、同时live对象≤I+4M；不另做泛化研究或全依赖统计。
- [x] **Step 2: fresh history/query/owner。** 实现fixed注册/metadata/TAR guard、newclaim双排空/完整历史及historicalquery的单complete/authEOF/实际Close。focused新host/native guard测试通过即提交feat(etcd): authenticate cold archive recovery under fresh claims。
- [x] **Step 3: distinct cursor/target与继续。** 新source/body、stable target/currentworker、旧checkpoint replay验证、机械cursor/local executor共用；不truncate已完成文件，不重建旧指针。用真实新fixture验证冷重启两文件，并仅回归抽取后受影响原始local行为；提交feat(etcd): resume bounded local archives with fresh recovery owners。
- [x] **Step 4: charged reconciliation/terminal/GC。** exact旧intent/generation、freshlocalrepair及domain completion、terminal只读unknown仲裁、独立checkpointbuilder atomicGC。新增截断/partialwrite/fsync/terminal/GC丢ACK/repeatcrash和65文件continuation，按源码epoch有限组验证；提交feat(etcd): reconcile charged local intents and advance recovery progress。
- [x] **Step 5: 故障/Close/边界。** 真实oldwriter锁阻挡及全部descriptor/postguard/GC实际join、新root/boot/namespace/tamper拒绝、claim失效/BackendClose、actualnative50pins/64nodes/256KiB检查。必要测试单独连贯提交，不留到最后一个大commit。
- [x] **Step 6: 验证与一次独立gate。** 新hostrace、Linux本单元native/有限必要compatibility、affectedvet/cross/gofmt-l/bash-n/diffcheck。保留本单元raw/实际Wait/labelcleanup/sourceSHA/binary和失败；同代码不重复。OWN task-1-report.md短列commits、范围/限制、每epoch结果及schema/liveness/wire；稳定必要artifact SHA排除自身/liveledger/report/review。不创建新的统计框架。Root派一fresh scopedreview，实际缺陷在同author/reviewer循环内修正。

2026-10-15范围验收：七笔源码提交、唯一fresh review及R1/I3限定闭环，见reports/2026-10-15-workspace-sync-local-recovery-verification.md。历史失败原始Wait缺口保持披露，不作为PASS；只接受本地普通文件冷恢复，后续目录/删除/完整sync/安全释放继续。

此plan为master实施细化，完成继续目录/删除/完整同步/安全释放与生产迁移；不结束分支或推送。
