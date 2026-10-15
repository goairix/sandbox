# Coherent Domain Read View Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox syntax for tracking.

**Goal:** 为后续业务读/复用/Destroy提供同revision owner/control等关联记录和一次只读变更复核。

**Architecture:** 独立native只读view，发现后一次coherent点读，self/origin封闭的四point MOD证据；无N常驻状态或旧Store adapter。

**Tech Stack:** Go1.25.6、现有etcd client3.6.14与原生三成员testfixture，无依赖修改。

**Spec:** docs/superpowers/specs/2026-10-15-etcd-coherent-read-view-design.md

## Global Constraints

- 只读metadata，≤2次读取、1次Confirm；无writes/lease/watch/Nmap/actor/自动无限重试，≤64nodes/256KiB nativewire。
- 不改manager/main/deploy/旧domain readers、snapshotcache、source/claim/archive/recovery、sharedfixture/脚本，不启用生产路径。
- view四point keys/MOD为private，同Backend/self约束；Records owned深复制，phase/health/expiry不授予operation许可。
- 完整operator identity/restore、point wire/header/key/lease/revision/canonical/关联校验，ABA failclosed。
- 源码/tests/docs/report/格式只apply_patch，不cat/heredoc/Python写这些文件、不gofmt-w；raw logs/JSON/binary可工具生成。
- 不读任何CLOSED OWN、无关Sentineldoc或旧已完成作者/reviewer；只跑新增有意义回归，不重跑既有通过suite或重建SBOM。
- 精确小提交：OWN/git-commit.lock fcntl LOCK_EX covering expectedbranch/HEAD/indexempty/add/exactstaged/diffcheck/commit/postempty。共享branch另有local恢复author，提交前通知Root协调HEAD，不能全局add/push。
- 两Task共享Git，先持.git/codex-etcd-commit.lock再持各自OWN锁；新Go定义在tool边界保持可编译，不stage/修改另一Task文件。Root提供最新HEAD，TaskBASE仍记录自己的首次plancommit。

## Review Focus

1. discovery读被错误当coherent证据，或者发现变更后悄悄跟随新sandbox。
2. 同值ABA、restore/cluster漂移和nested header不一致；RPC错误不能转success。
3. view/Records返回值变更及clone不能改变或伪造原证据。
4. 首次absence vs dangling reference，合法剩余fence不是notfound以外的损坏。
5. 最大合法domain字段转义与实际wire/有限请求数；不把metadata Confirm说成期限/健康准入。

---

### Task 1: Bounded coherent domain views

**Files:** Create internal/storage/state/etcd/domain_read_view.go、domain_read_view_types.go、domain_read_view_rpc.go、domain_read_view_test.go。只有确实需要才拆domain_read_view_rpc_test.go；不要求无用stub。原helpers可调用不可修改；新test直接用integrationBackend/faultKV/acquisitionInput等现有源码fixture，不能读旧OWN。真实集群可用scripts/test-etcd-state.sh限定-run新名称；不修改该脚本/compose或local恢复的脚本，实际server版本/原始Wait和精确ownedlabels清理须记录。

**Interfaces:** spec列出的ReadWorkspaceView/ReadSandboxView/ConfirmSandboxView和Records/Revision/ControlRevision；SandboxReadState四个domain record指针，内部view四key/revision固定数组。私有boundedpointRPC自行固定签名，不导出授权。

测试可在本OWN用apply_patch创建四服务image-only compose overlay，统一Root已恢复的精确etcd3.6.15 linuxarm64 digest5ed4e32061aa061f970d6500629213e8579ef17571dcf9789074966acb9afad1；不改sharedscript/源compose，不假tag其他版本。不读取输出真实业务配置，唯一project/labels与原始Docker Wait/清理记录保留。若原fixture现有server版本可直接用，也须如实记录，不能依靠无endpoint SKIP。

- [ ] **Step 1: RED与schema核对。** 新函数最小可编译拒绝入口后，真实同revision/ABA/copy回归证明未实现行为；禁止编译失败冒充RED。列最大合法四record/reply和有限节点数，不改变原有64KiB record cap。
- [ ] **Step 2: coherent read。** storageidentity/discovery/final4point同revision严格校验、field关联和返回owned records，新增focusedtests通过即小提交feat(etcd): add coherent bounded sandbox read views。
- [ ] **Step 3: confirmation与鲁棒性。** 同Backend/self/四MOD只读Txn，identity/restore漂移、ABA/取消/错误wire和copy边界；验证后小提交feat(etcd): confirm sandbox read views without state mutations。
- [ ] **Step 4: gate。** 只跑新增hostrace/真实三成员cases，vet/format/diffcheck，保留原始failed/success epochs/Go版本/源码binary绑定与精确fixture清理。每个通过case源码不变不重跑。OWN task-1-report.md短列commits/caps/结果与限制；Root一个fresh scopedreview。无性能/生产切换claim。
