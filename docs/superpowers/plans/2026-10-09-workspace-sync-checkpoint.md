# Workspace Sync Checkpoint Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. 一个fresh implementer完成完整功能单元，独立fresh reviewer审核；不创建重复/嵌套任务。

**Goal:** 把原始local成功文件进度与 generation GC 在已有task checkpoint中原子提交。

**Architecture:** compatible v1 + strict v2 checkpoint；原始GC native61cmp+3write/failureempty=64nodes；unknown同点绑定原slot与checkpoint generation。一个可验收单元，内部及时提交schema和协议改动，避免重复跨单元交接。

**Spec:** `docs/superpowers/specs/2026-10-09-workspace-sync-checkpoint-design.md`

**Tech:** Go1.25.6、etcdv3、Linuxarm64CGO0/native3member，hostrace。

## Global Constraints

- 用户已批准master阶段1–5且继续，小步设计是执行细化；baseline17fb66ec16e2f3d40cf2731b653bcd263036903e，branch codex/etcd-state-management；只当前newOWN，所有CLOSEDOWN禁止读取/复用/redispatch，Sentinel仅filename status。
- source/test/docs/format只apply_patch，raw日志/JSON/brief/diff可工具生成。Git mutations持有OWN/git-commit.lock fcntl LOCK_EX：expectedHEAD/branch/preemptyindex/exactpaths/stagedcheck/commit/postempty。无push/deploy/依赖升级/main接线/nestedagents。
- 全50历史pins+prefix+budget保持，实际64nodes/256KiBrequestreply/4096Bcheckpoint/512Bslot/B4096..16384intent/1024Bkey；无新KV/Nmap/actor/capraise。未知仅原始readonly，不resend/冷授权。
- mutex不跨RPC/IO，nativeMu不跨physicalpostguard；source/body/target登记包含新RPC并Close实际join。native非race/tmpfs非powerloss/分批非容量性能，所有rawfailure/skipwarning分类保留。
- OWN/task-1-report.md及evidence-task1.json包含命令exit/真实leaf/WARN/workingGitcompiled/provenance/controllerchildWait/exactcleanup与稳定artifacts bytesSHA；manifest不包括自己/仍写入的generatorlog。Root只audit不重跑作者套件；达到提交条件及时小提交。

## Review Focus

v1strict/canonical兼容；v2checkpoint被legacy覆盖/降级；GC先释放后写进度；wire超64而删pins；冷metadata重建权限；checkpoint同值ABA/nativeoverflow；unknown换generation或重复效果；新pointread与Close登记竞态；把BodyEnd当TAR随机resume地址。

### Task 1: 原始 GC 原子持久 checkpoint

**Files:** create `internal/storage/state/etcd/task_workspace_sync_checkpoint_records.go`, `task_workspace_sync_checkpoint.go`, `task_workspace_sync_checkpoint_test.go`；modify `task_records.go`, `task_claim_records.go`, `task_checkpoint.go`, `task_read.go`仅compatibledecoder/loader/deepcopy/legacyreject，`task_workspace_sync_recycling_gc.go`, `task_workspace_sync_recycling_settlement.go`, `task_workspace_sync_recycling_types.go`仅originalstate及sharednativefailurepolicy，`task_workspace_sync_recycling_native_linux_test.go`、`scripts/test-workspace-sync-local.sh`仅限定checkpointcases。必要legacytest文件先Root裁决。无需改productionmain/provider/ownerrelease/source cold恢复。

**Interfaces:** exporteddiagnostics `TaskWorkspaceSyncCheckpoint`/`TaskCheckpointRecord.WorkspaceSync *TaskWorkspaceSyncCheckpoint`，精确15fields见spec；strictv1wire保持、v2wiremandatoryworkspace_sync；既有LoadTaskCheckpoint读取owneddeepcopy；private original GC捕获checkpoint generation→atomiccommit→readonlysamepointarbitration，既有recycleLocal签名及nextgate保持。复用既有sharedPut/header/wire/nativeguard，内部选择emptyfailure，不重复校验helper。

**Binding context:** currentcode+trackedacceptancereports可读，CLOSEDOWN不可读；cwd /Users/dysodeng/project/go/cloud/sandbox，Go /Users/dysodeng/go/go1.25.6/bin/go；freshONEauthor/freshONEreviewer由Root派发。

- [ ] Step 1: RED既有原始两文件GC无checkpoint，新增实际断言；host v1byte兼容/v2mandatorynullunknowndup/canonicallimits/invaliddigest/nativegeneration/64nodeworstcase。只focusedRED，不重放已经验收的套件。
- [ ] Step 2: strictboundedv2wire及v1compat、deepcopy诊断、claimbundlevalidated读与legacyPrepare拒绝v2；hostrace相关cases与已有checkpoint/claim/records必要回归，通过后第一笔连贯source小提交。
- [ ] Step 3: originalsettlement checkpointpin/newvalue/attemptonce、54pinpointread、GC61cmp+3write/failureempty，sharednativefailurepolicy；ACK3responses和unknownslotintentcheckpoint同点generation校验，保持originalpointer/lifetimes/currentguards。
- [ ] Step 4: 真实两文件/65文件checkpoint+GC同MOD/ordinal/bound；checkpoint冲突/corrupt/missing/foreign/needs_reconciliation/unknownlostmalformedlate/reused、restoreclaimroot与Close新RPC/postguard，固定最少必要批次。若现有测试期待reply2，狭窄升级oracle，不重新完整49/154套件。
- [ ] Step 5: hostrace scoped、nativeactual0SKIP、vet/cross/format/bash/exactdiff；保留失败/ENV告警、compiler真正选中输入与二进制源码绑定、原始childWait/controller/cleanup。自审原子性/budget/lifetime/schema，及时第二笔功能或限定测试提交，不累积一个巨型提交。
- [ ] Step 6: OWN完整report/稳定manifest，Root独立review/fix≤3原authorreviewer、rawaudit与trackedacceptance，封闭此OWN；master未完成继续正常恢复/安全释放，禁止branchfinish/push/重复执行已完成任务。
