# Workspace Sync Recycling Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Each Task has one fresh implementer and one independent reviewer.

**Goal:** 原始本地成功写入形成持久 terminal，并以永久 generation slot 支持连续复用。

**Architecture:** 顺序3个可独立验收提交：先 profile/schema；再原始 reserve/gate 适配；最后 witness terminal/GC。旧 profile 默认保持；未知/失败保守收费。

**Spec:** `docs/superpowers/specs/2026-10-09-workspace-sync-recycling-design.md`

**Tech:** Go1.25.6、native etcd v3、Linux arm64 CGO0、现有私有原始 local owner。

## Global Constraints

- 基线3fcbf1304fd695d51712c65014ae3681bf3f0bee、branch codex/etcd-state-management。master已批准实施范围，任务边界由Root裁决，不反复要求用户批准。
- source/test/docs/format仅apply_patch；原始工具logs/JSON/brief/diff可工具生成。无关Sentinel仅filename status；不读/复用CLOSED OWN。无nested agents/push/deploy/依赖升级/生产main切换。
- 唯一新OWN、ledger首行plan身份、preflight表、Root rulings。每Task RED/GREEN和覆盖变更的验证、自审、及时exact-path小提交，Git mutation全程OWN/git-commit.lock fcntl LOCK_EX，branch/expectedHEAD/preemptyindex/exactstaged/check/commit/postempty。
- 保留全部50历史pins+prefix+budget、64helpernodes、256KiB请求/回复、64KiBrecord、slot512、intentB4096..16384、key1024、32KiB scratch；不加无预算KV/Nmap/常驻sandbox actor。每次mutation实际protobuf preflight，限额冲突先向Root报告。
- profile2 slot永不删除；native revision+value fencing；未知mutation只读，不重发效果/退款。原始 worker/body/target/witness only，诊断不是authority。
- 生命周期Close先封闭后真实join；mutex不跨RPC/IO，nativeMu不跨physicalpostguard。nativeCGO0非race；tmpfs非断电持久性，分批不称整轮全绿。
- 每Task报告OWN/task-N-report.md及evidence-taskN.json，rawartifact字节/hash/命令exit/leafskipwarning/clientserver分类/workingGitbinarysource关联/cleanup。Root只审核证据，不重复作者套件。全部plan完成仅封闭自身OWN；整个迁移仍继续。

## Review Focus

旧absence事务ABA、slot delete或freeValue-only、generation版本溢出、unknown换候选/重复write、witness复制/冷metadata被升级、Close登记竞态、wire节点超限或删除历史pins、永久收费/正常恢复被误称完成。

### Task 1: 独立 recycling profile 与严格 generation-slot schema

**Files:** create `internal/storage/state/etcd/task_workspace_sync_recycling_types.go`, `task_workspace_sync_recycling_records.go`, `task_workspace_sync_recycling_budget.go`, `task_workspace_sync_recycling_test.go`; modify `client.go`, `task_workspace_sync_intent_types.go`, `task_workspace_sync_intent_budget.go`, `task_workspace_sync_intent_admission.go`, `task_workspace_sync_intent_read.go` only profile discrimination/failclosed；create `task_workspace_sync_recycling_integration_test.go` for existing real-etcd fixture operator metadata. 不改变nativeLinux脚本/worker/localIO/生产程序；其他文件先Root ruling。

**Interfaces:** new Options pointer互斥旧pointer；private budget pin profile(1/2)、独立 budgetRecord Validate2、pinTaskWorkspaceSyncRecyclingBudget(ctx,options)。`taskWorkspaceSyncRecyclingSlotRecord`严格9fields及state validation；encode/decode canonical slot接受nativepermanent mutable版本；key builder `workspaceSyncRecyclingKeys(ref)` returns intent/slot independentprofilepaths；旧reserve和旧load在profile2上显式ErrInvalidConfiguration，直到Task2适配。record字段及路径精确见spec，不能擅自profile自初始化。

**Binding context:** 已批准master实施裁决；Task1范围只配置/严格编码，无slot mutations或provider效果。cwd /Users/dysodeng/project/go/cloud/sandbox；基线planningcommit由Root dispatch给出。onlyapply_patch、newOWN exactlockedcommit、noSentinel/CLOSEDOWN/nestedagents。Go /Users/dysodeng/go/go1.25.6/bin/go；hostrace/realetcd环境能力分别报告。

- [x] Step 1: RED：pointer互斥/非法options、legacycanonical保持；3state mandatory/null/dup/unknown/非法组合/512越界/nativeLease或revision不合法/slot上下界/不同budget partition key。不用复制Validate逻辑作为测试oracle。
- [x] Step 2: 新profile record/schema/独立paths与logicalbound，legacy3fieldoptions不加字段；private pin加profile并旧literal改keyed。严格bounded decoder拒绝非canonical，无不受限分配。
- [x] Step 3: New拥有optionscopy、validate互斥、只读immutable配置2 pin、错误version/值/lease/missing拒绝且无writes。profile2旧reserve/load failclosed；nil/legacy不变。不用公开非功能执行入口。
- [x] Step 4: 真实native operator预置config后New读取/pin，缺失/错误/变化拒绝并验证key inventory无初始化；旧profile targeted回归。host-race新pure cases；real-etcd unavailable如实报告，不伪称通过。
- [x] Step 5: scoped test/race、vet包、Linuxarm64CGO0及Darwincompile、gofmt-diff/bash若无脚本不需bash、exactdiff。相关旧profile回归一次；不重跑全migration/HA/容量。
- [x] Step 6: exact-path coherent source commit与完整OWN报告/evidence、自审。Root独立review/证据audit/acceptance提交后进入Task2。

### Task 2: 原始 generation reserve 与 local gate 适配

**Files:** create `task_workspace_sync_recycling_admission.go`, `task_workspace_sync_recycling_read.go`, `task_workspace_sync_recycling_guard.go`, `task_workspace_sync_recycling_native_linux_test.go` in etcd directory；modifyTask1types/records/test、intent_admission/read、local_guard、local_types、test-only `task_archive_delivery_native_test.go` budget选择工厂、`scripts/test-workspace-sync-local.sh` fixed recycling modes及operator预置测试接缝。其他先Root ruling。

**Interfaces:** 原 reservePutIntent profile dispatch→private reserveRecyclingPutIntent；draft captures permanent free create/mod/version/value beforeRPC；只originalACKpostguard设置acknowledged。local gate适配profile2 occupiedslot，旧profile完全原样；Load选择backend explicitprofile读复制diagnostics，nilbackend保留legacylookup不推断authority。

- [x] Step 1: RED实际S1 operator预建256slots，原始localbody reserve/gate/put及foreign/malformed/missingfree/knownoccupied拒绝；hostprotocolwire64nodes/worstB边界；legacy targeted回归。
- [x] Step 2: 新profile free point-read→bounded候选nativeTxnfence Mod/Value→2Put；未知固定候选只读，不重发/换槽/授权；失败fresh完整quota guard才检查下一候选。所有原history/restore/prefix/budgetpins保持。
- [x] Step 3: profile2 occupied strictCreate/Version/knownACKMod/value/intentbirth gate；原body一次消费和physicalpostguard保持。read arbitration只是诊断。真实crossBackend并发S1只一winner；captured oldfreeTxn在另一次generation变化后拒绝，测试native实际事务非fakeclient。
- [x] Step 4: 实际wire节点/bytes/requestresponses严格预检；originalchild/control/attachwait/inventory安全复用已跟踪脚本，实现模式必须有上限/原句柄，不能读旧OWN。
- [x] Step 5: scoped hostrace/nativeLinux实际leaf0SKIP、旧v1/knownACK/unknown/localIO接缝回归；vet/cross/format/bash/exactdiff；失败/rawWARN/clientserver证据如实保留，不反复全轮setupdeadline重试。
- [x] Step 6: exact-path source小提交、OWNtask2完整报告manifest、自审；Root一次独立review与audit，trackedacceptance后Task3。

### Task 3: 原始 witness 持久 terminal 与原生有预算 GC

**Files:** create `task_workspace_sync_recycling_settlement.go`, `task_workspace_sync_recycling_gc.go`, `task_workspace_sync_recycling_settlement_test.go`；modify recyclingtypes/records/guard/native_linux_test、localtypes/local_guard/intent_types仅originaldraft→put/settlement唯一指针绑定、source_types/body/cursor仅ownership互斥及profile2派发后GC明确完成前保留currentbody需要、script仅fixedcases。其他先Root ruling。

**Interfaces:** private `settleLocal(ctx,witness) (*taskWorkspaceSyncLocalSettlement,error)` 与 `recycleLocal(ctx,settlement) (Outcome,error)`；originalsettlement自引用/origin/draft与terminalnativepin，wire/evidencecodec及具体fields按spec。oneoriginalbody留到GC结果；后续body.Close/Next可继续，不取消wholeSource。

- [ ] Step 1: RED原始success/witnesscopy/unfinishedjoin/foreignbody/knownACKmissing/failedIO拒绝；持久terminal然后free+delete，原native状态检查。旧slotValue/Mod捕获后真实延迟Txn不能破坏后一代。
- [ ] Step 2: settle重新注册current source/body/target ops及contexts，在不复用cancelputctx前提下freshcurrentnative/worker/rootguard；完整originalcount/hash/devino与intent符合；64nodesTxnonePutterminal；attemptonce、unknown只能原始guarded诊断相同version/intent，不能二次效果。
- [ ] Step 3: GC originalknownterminal64nodesPutfree+exactDeleteintent、成功原ACK及delete1检查；unknown仅固定same-txnfree/absence/version/Create/MOD验证，已复用保持unknown。currenthistory/restore/claim改变保守保留收费，无TTL退款或coldauthority。
- [ ] Step 4: S1至少65个实际连续local文件→terminal→GC，验证activekeybound/nativegeneration增长/bytesfsync结果；unknownreserve/terminal/GC、crossBackend新代、Restore/claim/root变化、Close同时physicalwait原RPC/postguard；合法错误/未知仍charged。
- [ ] Step 5: 实际protobuf worstB/node/reply检查与scopedrace/native0SKIP、vet/cross/format/bash/exactdiff；使用original3children/logblocks/Wait/inventory证据，tmpfs不证明crash；未实现cursor/normalrecovery/saferelease/生产性能明确列出。
- [ ] Step 6: source及时小提交和OWNtask3报告manifest；Root独立review/audit/tracked验收更新整体状态，封闭本OWN。wholemaster未完成不进入branchfinish/push、不重读本OWN；下一步有预算checkpoint/实际安全释放与生产接线。
