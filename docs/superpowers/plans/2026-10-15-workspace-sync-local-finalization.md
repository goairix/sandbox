# Workspace Local Whole-Sync Finalization Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox syntax for tracking.

**Goal:** 完成本地归档的目录、类型替换、策略清理、目录权限恢复及持久完整同步完成标记，为后续安全释放提供可独立核验的前置记录。

**Architecture:** 一个完整功能单元复用已验收的fresh-claim regular recovery；新增私有full模式、单槽v3 metadata pass和固定两条进度记录。先封住所有写入口，再持久发布完成；完成诊断不能授权运行时关闭或owner释放。

**Tech Stack:** Go1.25.6、Linux arm64/CGO0、x/sys/unix、现有固定三成员etcd3.6.15 fixture；不升级依赖。

**Spec:** docs/superpowers/specs/2026-10-15-workspace-sync-local-finalization-design.md

## Global Constraints

- 一个Task端到端交付，内部达到验证要求立即小步提交；不按文件建立额外review任务，不等全部完成才大提交。
- 原regular-only API、v1/v2 wire/validator、33字节index、S=1..64、固定64 archive slots及I+4M保持；旧regular GC仍61+3=64。
- 完整50基础pins与operations/budget pins不减；新GC恰64nodes，全部native请求和两分支响应≤256KiB，逐次写前预检。
- 新progress≤16384字节、complete≤8192字节，Lease0，逐点读取；无每目录/每文件key，无N map/actor或全prefix发现。
- 同task对称准入在同store.mu下完成；不得持store/lifecycle/native锁跨实际IO/query/join；登记保持到actual Close/Wait。
- ledger单4096字节路径base64和≤128紧凑祖先帧，单32768字节Getdents scratch，固定归档pass；核算metadata克隆和遍历scratch的allocation liveness，不能宣称RSS上界。
- 完成前seal不可逆、所有效果实际join、target actual Close、readonly postguards；不等待自身导致死锁；任何lost ACK后同attempt只读仲裁。
- 只local同boot/mountns/unchanged recovery root与升级后合作writer；不证明cloud/reboot/powerloss，不接main，不释放owner，不删除Redis。
- 所有源码/测试/文档/报告/格式只apply_patch；无cat/heredoc/Python写内容、无gofmt-w。raw/JSON/binary/sourcearchive/brief/diff允许工具生成。
- 不读CLOSED OWN、不复用已验收作者/reviewer、不碰无关Sentinel计划；只新增及实际受影响的有限兼容回归，不重放已验收矩阵。
- 任意Git写先持.git/codex-etcd-commit.lock再持本OPEN OWN/git-commit.lock，fcntl LOCK_EX，检查branch/expected HEAD/空index/精确paths/staged diffcheck/postempty；先向Root申请提交窗口。
- 无push/deploy/在线Redis或Kubernetes操作/secret或完整Docker inspect输出；保持失败与原始Wait缺口，不重构证据为PASS。

## Review Focus

1. registerRecovery/reserveRecovery两方向竞争、quarantined seed登记及old source在完成标记后的再入。
2. S1已占旧put时目录冲突修复的授权范围、excluded后代和DeleteExtraneous=false保护。
3. 0000/0555目录及保留excluded祖先临时权限在ledger丢ACK/Close/新claim后的恢复。
4. effects-done/complete发布与监控、自身join、actual FD Close、claim失效间的时序。
5. 深128层/大fanout/最大转义名时固定内存、固定归档pass和64nodes/256KiB边界。

---

### Task 1: Complete local whole-sync finalization and crash continuation

**Files:** 全部相对internal/storage/state/etcd，精确名称及职责见Spec25与Binding clarifications。
- Create: task_workspace_sync_local_finalization.go、task_workspace_sync_local_finalization_types.go、task_workspace_sync_local_finalization_records.go、task_workspace_sync_local_finalization_progress.go、task_workspace_sync_local_finalization_metadata_intent.go、task_workspace_sync_local_finalization_completion.go。
- Create: task_workspace_sync_local_metadata_linux.go、task_workspace_sync_local_metadata_unsupported.go、task_workspace_sync_local_walk_linux.go、task_workspace_sync_local_directory_permissions_linux.go、task_workspace_sync_local_archive_order.go。
- Modify: task_archive_receive_types.go、task_archive_recovery_store.go、task_archive_delivery.go、task_workspace_sync_recovery_source.go、task_workspace_sync_source.go、task_workspace_sync_recovery_types.go、task_workspace_sync_recovery_local_linux.go、task_workspace_sync_recovery_intent.go、task_workspace_sync_recovery_checkpoint.go；仅Spec要求接点。task_archive_delivery.go在reserve成功且原spool已登记后、交付source前，以当前claim逐点拒绝progress/complete；拒绝时实际join/Close，不能仅在reserve前检查。
- Test: create task_workspace_sync_local_finalization_test.go、task_workspace_sync_local_finalization_native_linux_test.go；故障/ledger/walk测试可按职责拆同prefix测试文件，列入report；仅必要修改task_archive_delivery_native_test.go和scripts/test-workspace-sync-local.sh fixture/selectors。
- Do not modify task_archive_tar.go、旧task_workspace_sync_local_io_linux.go目录验收或旧v1/v2 validator。

**Interfaces:**
- Produces Backend.FinalizeTaskWorkspaceSyncLocal(ctx context.Context,c *TaskClaim,slot uint32,destination *controltransport.Destination,worker *launcher.KernelBoundary) (TaskWorkspaceSyncLocalFinalizationResult,error)。result/reference精确字段见Spec Binding；private reader不公开authority。
- Consumes当前prepareTaskWorkspaceSyncRecovery/registerRecovery/regular checkpoint及history/query guard；新增private模式/strict dispatcher，普通恢复路径语义保持。
- Producesprogress regular→metadata→effects-done→sealed、单槽local_metadata_pass v3、immutable complete及私有只读重验；后续release自己重建current pins并比较exact key，不消费boolean授权。
- Private bindLocalFinalizationTarget(ctx,target)在原始source/target实际登记和native权限下完成无regular-body的received→bound，不分配put槽；纯目录测试必须走真实原始绑定和Close后cold恢复。所有mutable resume先CAS新progress Mod；sameclaim regular resume拒绝至genuine newclaim，completed marker仍零写。

- [x] **Step 1: Focused RED and budget/admission skeleton.** 新测试TestTaskWorkspaceSyncLocalFinalizationRecords验证strict canonical/Lease0/immutable、最大base64ledger和native nodes/bytes；TestTaskWorkspaceSyncLocalFinalizationAdmission验证两个准入方向及quarantined seed。记录旧源码下feature RED，补最小可编译新模式/record和对称准入，host/race验证后第一笔小提交。
- [x] **Step 2: Full regular phase and S1 repair.** 实现private DFS-order验证、progress regular、necessary ancestor/type修复及全新repair evidence domain；只修exact intended path，excluded冲突破坏前拒绝。真实native测试TestTaskWorkspaceSyncLocalFinalizationNativeRegular覆盖file↔dir、S1已占旧put、无regular/emptydirs、旧checkpoint prefix不重写；focused GREEN后独立提交。
- [x] **Step 3: One-slot metadata pass and policy pruning.** 实现v3 reserve/recover/terminal/GC及phase先行dispatch；流式pass A、Getdents bottom-up pruning、excluded retained-original-mode持久ledger、streaming transcript；每mutatingsyscall前guard，inode/dev/uid/mount/hardlink检查。新增NativePolicy、NativeLedger验证deleteflag/excluded/0000祖先freshclaim恢复；GREEN后独立提交。
- [x] **Step 4: Final modes, seal and immutable completion.** 实现固定DFS pass B/postorder modes、fsync/actual FD Close、不可逆effect seal和readonly postguard，再发布sealed+complete。新增NativeCompletion验证effects-done恢复、完成后zero physical/native writes、旧source/regular-only拒绝marker、caller diagnostics不可转authority；GREEN后独立提交。
- [x] **Step 5: Fault, concurrency and resource boundaries.** 新NativeFaults覆盖mkdir/unlink/rmdir/chmod/fsync/Close中断和reserve/progress/terminal/GC/complete lostACK，只读仲裁/新source恢复；NativeJoin覆盖blocked真实IO/Close/metadata和claim/backendClose；NativeBounds覆盖depth128/fanout/最大名/S1/S64/固定passes/节点64/256KiB和liveness；NativeRefusal覆盖nonDFS/symlink/hardlink/mount/rootABA/leasedmarkers。按≤512MiB fixture分组，最多同时两个nativefixture，保留每组actual Cmd.Wait、exactlabels+cleanup及source/binary SHA。按源码epoch有限验证，通过及时提交，禁止整批旧套件重跑。
- [x] **Step 6: Self-review and one fresh task gate.** 最终仅跑新增hostrace和必要受影响compatibility、affectedvet、Linux编译、gofmt-l/bash-n/diffcheck；同源码不重复native。写OWN task-1-report.md，逐commit/epoch列TDD RED/GREEN、raw commands/results/actualWait、source/binary hashes、wire和allocation表、明确未实现范围。Root生成只含本Task全部commit的review包派fresh reviewer；同author/reviewer完成限定修复轮次。Root验收记录后关闭OWN并继续runtime drain/safe release，不结束master分支。

测试十类覆盖按Spec26(a–j)逐项映射至以上named selectors；测试体现真实物理效果、native fences和actual joins，不能仅镜像私有实现。尚未编写的函数与字段在新测试中允许首先编译RED，但至少保留一个行为RED证明已存在准入缺口；不为可逆文档变化补测试。

## Acceptance

2026-10-15：Task1 范围验收完成，五笔源码提交 `44673b4/0488e3c/3b1e4e6/30b5fc9/ba7a3dc`。独立初审 I1 的 fresh-claim adoption 缺口已由同 author 修复、真实延迟 GC 的 RED→GREEN 验证、同 reviewer 限定复审通过，剩余 C0/I0/M1；M1 是保留的 etcd 关闭告警。上述步骤完成按实际范围解释：S64 仅单 task partition，foreign UID/mount 与额外 backendClose/claim-loss 阻塞排列未重新执行；失败 full-cell fixture 与历史 epoch 均不变为当前全矩阵/容量 PASS。详细证据及支持边界见 `docs/superpowers/reports/2026-10-15-workspace-sync-local-finalization-verification.md`。继续运行时物理关闭及安全释放，主方案尚未完成。
