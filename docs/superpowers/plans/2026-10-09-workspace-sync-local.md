# Workspace Sync Local IO Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 使原始归档 body 在永久意图与真实 worker/root 约束下完成实际本地 regular-file IO。

**Architecture:** 两个顺序验收的小单元：先只读物理 target 与 native 写前门，再有界真实 IO 与进程内结算 witness。所有 permanent intent/slot 保留收费，不接生产 main、不退款。

**Tech Stack:** Go 1.25.6、Linux arm64 CGO0、openat2、原生 etcd v3 Txn、已有 archive/TLS/source fixture。

**Spec:** `docs/superpowers/specs/2026-10-09-workspace-sync-local-design.md`

## Global Constraints

- 基线 bc5dc9636532ca1977263ca231fe25a9d7dd3fb8；分支 codex/etcd-state-management。无关 Sentinel dirty 文件仅 filename status，不读、不改、不暂存。
- source/test/spec/plan/report/format 只使用 apply_patch；无 cat/heredoc/Python 写源码，无 gofmt -w。工具生成原始 logs/JSON metadata/brief/diff 可脚本写。
- 每个单元 RED/GREEN、覆盖变更的 host/native 证据、vet/Linux compile/format/diff、self-review 后及时 exact-path 小提交。Git mutation 全程 fcntl LOCK_EX 本计划 OWN/git-commit.lock，校验 branch/base、pre/post empty index、exact staged set。
- 原 50 历史 pins、完整 prefix、permanent budget 不放宽；64 helper nodes/64KiB record/256KiB request/reply；B4096..16384、slot512、key1024。无额外无预算 KV、退款、slot reuse。
- original body/worker/target only；unknown/arbitration/诊断 copy 无派发权。只有原始成功 ACK + postguard 可设置私有 acknowledged。
- 固定 32KiB scratch、4096 path bytes/128 depth；无 N map/每 sandbox 常驻 actor。取消必须物理等待 owner，未完成无 witness。
- native Linux CGO0 不是 race；host 不 bootstrap。tmpfs 不证明重启持久性；本单元不证明 safe-release、production wiring、云 provider 或容量。
- 不读/复用任何旧 CLOSED OWN；本计划唯一作者、独立 reviewer，无嵌套 agents、无 push/deploy/依赖变更。

## Review Focus

1. 已提交元数据的 unknown/ABA 被错误提升为 one-shot authority。
2. 原始 root FD 与路径、mount、UID/GID、worker seal 变化后继续写；hardlink/symlink/跨 mount。
3. mutating IO 先于原生 guard/owner registration；Close 因取消提前返回。
4. body/read/next 并发导致重复派发、丢 active owner 或无限锁等待。
5. 子目录、空 body、partial write、fsync/close 错误被描述成完整/持久 final sync。

### Task 1: 只读物理 local target 与原始 native 写前门

**Files:** create `internal/storage/state/etcd/task_workspace_sync_local_types.go`, `task_workspace_sync_local_linux.go`, `task_workspace_sync_local_unsupported.go`, `task_workspace_sync_local_guard.go`, `task_workspace_sync_local_test.go`, `task_workspace_sync_local_native_linux_test.go`; modify `task_workspace_sync_intent_types.go`, `task_workspace_sync_intent_records.go`, `task_workspace_sync_intent_admission.go`, `task_workspace_sync_source_types.go`; create `scripts/test-workspace-sync-local.sh`. 可按实际职责在这组新文件内组织；其他文件须先向 Root 请求 ruling。

**Interfaces:** consume原始 source/body、`launcher.BootstrapWorkspaceSyncWorker/ValidateCurrent/WorkspaceSyncWorkerReference`、原 budget。produce private `openTaskWorkspaceSyncLocalTarget(ctx context.Context, worker *launcher.KernelBoundary, plan TaskWorkspaceSyncPlan, root string) (*taskWorkspaceSyncLocalTarget,error)`，其 `Close() error`、`bindLocalPut(ctx, body, target) error`、`prepareLocalPut(ctx,body,target) (*taskWorkspaceSyncLocalPut,error)` 原始 one-shot IO owner；具体 receiver 为 source。Task 2 消费该 owner，不得消费 copied ref 或 Reader。

**Binding constraints (本 brief 独立携带):** 本任务 spec 已批准 master 下的实施裁决，不等待用户重新批准。基线 bc5dc96，当前分支 codex/etcd-state-management；只 apply_patch 修改源码/文档，锁 OWN/git-commit.lock 精确小提交；不碰 Sentinel 或 CLOSED OWN、不派子 agent、不 push/deploy。原 native pins/64 nodes/64KiB record/256KiB RPC/32KiB scratch/B4096..16384/slot512/key1024 不放宽。只有新 v2 原始 ACK 可授权，v1 和所有 unknown/arbitration/冷诊断一律不可派发。构造不修改 root。scope 当前任务不写 provider file；native fixture 可以明确使用测试构造的真实文件。

- [ ] Step 1: RED 纯记录验证（v1 不可带物理字段、v2 必須 local/完整 worker+root pin、canonical/越界拒绝）以及 original/复制/外来/已读/empty-complete/unknown 拒绝。测试 stub 只能观察调用边界，不伪造 native 成功或原始 worker。
- [ ] Step 2: 实现 target 原始 self/worker/plan/root FD、dev/ino/mountID/fsType/UID/GID 只读 pin；root路径最多4096，0700/worker ownership，fs whitelist ext4/xfs/btrfs/tmpfs，拒绝其余；Linux原始openat2 noSymlinks/noMagiclinks，root内noXdev；非Linux fail closed。root Close 有 seal/active wait，不泄漏 FD。
- [ ] Step 3: bind 原始未读 body 才建立 version2 local physical attribution；现 v1 行为不变，所有新增记录字节计入原 B。只原 reserve明确完整成功 ACK + postguard 设置私有 acknowledged。native只读 gate保留50pins+prefix+budget，成功point读intent/slot严格匹配原始 birth/value/charge，失败bounded诊断；实际 protobuf node/byte 记录并受旧上限。gate消耗原始 one-shot、登记 source owner，provider效果仍由Task2实现，不公开错误可调用的 bypass。
- [ ] Step 4: 新脚本实际 Linux CGO0 非 root/caps0/NNP/network none/pullnever pinned image，tmpfs工作目录。新 Linux测试文件的 TestMain 仅在显式 SANDBOX_WORKSPACE_SYNC_LOCAL_NATIVE=1 分支 bootstrap，起三个真实 /usr/local/bin/etcd loopback child，复用现有3members fixture，不改变旧TestMain/default路径。原始 server/attach/container 生命周期 join、失败保留句柄、所有 exact owned labels inventory归零。脚本可参考 tracked worker 脚本，不读其 OWN。固定最大资源与 deadline；只重点inspect，不dump env/credentials。
- [ ] Step 5: 真实 kernel/native cases：原始目标被记录；构造只读不创建文件；原目标/worker/plan mismatches；复制对象、symlink/权限/root变化；unknown/native pairedABA/slot损坏/当前pins失效全部不发效果。第一轮只证明 gate；不得称实际put已完成。保留原v1 intent targeted回归；host -race 纯测试与 native CGO0 分别报告，0 FAIL/native0SKIP，不屏蔽 WARN；vet/cross/format/diff。
- [ ] Step 6: exact-path source 小提交，完整报告 OWN/task-1-report.md + evidence.json（source working/Git hashes、rawlogsha/bytes、binary/source关联、命令exit、实际 leaf/skip/warning 数、cleanup及限制），self-review。Root 一次独立 review 与验收后 Task2 接续。

### Task 2: 实际 local regular-file 写入与原始结算 witness

**Files:** create `internal/storage/state/etcd/task_workspace_sync_local_put_linux.go`, `task_workspace_sync_local_put_unsupported.go`; modify Task1 `local_types/local_linux/local_guard/local_test/local_native_linux_test.go`、source_body.go/source_types.go 仅消费互斥封闭需要的接缝、`scripts/test-workspace-sync-local.sh` 仅新case；其他文件须 Root ruling。

**Interfaces:** consume Task1 private original IO owner；produce private source `putLocal(ctx context.Context, body *taskWorkspaceSyncBody, target *taskWorkspaceSyncLocalTarget) (*taskWorkspaceSyncLocalWitness,error)` 与原始 self/origin witness，仅诊断 syscall completion。一个 original body只派发一次，source/body/target Close 实际join该 owner。

**Binding constraints:** 阅读新 spec；基线使用Task1验收后的HEAD。原始ACK门/nativepins/旧RPC限额、固定32KiB/4096path/128depth、only apply_patch/OWN exact locked commits、无Sentinel/CLOSED OWN/子agents/push/deploy相同。永久intent/slot完全保留，无native terminal/退款；tmpfs不证明重启持久性。不能用Reader/callback成功或复制reference重建权能。

- [ ] Step 1: RED真实 Linux 用例覆盖空/多chunk/nested/覆盖，错误root、symlink、hardlink、specialfile拒绝；host只纯 validation/unsupported。
- [ ] Step 2: Task1门在首mutating syscall前；scope rootFD、每个parent原始worker owned/private0700/noXdev/nosymlink，缺失parent可bounded mkdir+fsync，打开/create exact regular worker文件nlink1。不引入 staging/rename 布局；截断/覆盖 partial失败仍charged。新建mode按archive；最终确认文件身份。
- [ ] Step 3: 固定32KiB流式复制原始body，保留 source nativeguard/integrity。解决put已持source begin与body.Read嵌套的互斥接缝，不删guard、不持mu跨IO/RPC。每mutating阶段检查原worker/target/current权限。实际完整bytes/hash、file fsync+Close、modifiedparent fsync、postguard后才创建私有original witness。任何不确定/失败无witness/无重发/无退款。
- [ ] Step 4: owner注册先于IO，source/target/body/claim/Backend Close封闭取消后真实wait；真实读写完成处限定pause seam验证Close确实等owner，不能伪造syscall结果或无限挂fixture。IO错误和bodyhash破坏无success；slot保留、repeat拒绝、原始进程内witness不可复制/冷恢复。
- [ ] Step 5: host -race scoped pure/source接缝覆盖；真实LinuxCGO0原始worker+3membernative+TLS归档+物理文件逐项证明，报告实际leaf数/zeroSKIP/WARN与tmpfs限制，实际返回bytes/files/modes/hash/FD/Close证据；vet/Linuxcompile/gofmt-diff/bashsyntax/diff，不复测未触及全量HA/容量。
- [ ] Step 6: exact-path小提交与OWN/task-2-report.md/evidence-task2.json、自审；Root独立review/根审计/更新tracked进度验收。此次2-unit plan完成可封闭自身OWN，整体迁移仍未完成。
