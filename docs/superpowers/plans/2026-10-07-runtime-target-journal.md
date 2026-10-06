# Protected runtime target journal Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 为可信 target 提供实际受保护本地 closed gate、不可变 unknown exec intent 和有界历史/容量计量，历史不能恢复执行能力。

**Architecture:** Task1负责strict有界格式；Task2负责真实Unix目录FD、ownership/flock/fsync、闭门构造与分页accounting；Task3在该protected store上提供点写/读取unknown意图、错误恢复边界和可重复LinuxRoot脚本。每task验证、自审、立即smallcommit和独立spec/qualitygate；最终一次integratedreview与host/Linux gates。

**Tech Stack:** Go、既有controlprotocol与golang.org/x/sys/unix v0.47.0、真实macOS/Linux文件系统，复用既有pinned/local etctest image作为隔离LinuxRoot testbinary容器。无新dependency/production image改动。

**Spec:** docs/superpowers/specs/2026-10-07-runtime-target-journal-design.md。前单元source1af14b2、review397880b、permanentdocs7f1248d/8a71e73，完整finalreview/verification已保存。

## Global Constraints

- journal是被动诊断存储；只有closed和unknown公开写，不提供set-open/执行/accepted/terminal/GC，不从history/crypto evidence重建launchcap；整体phases1–5继续必需。
- manifest/command/temp最多8192字节、strictJSON nesting≤8、所有字段explicit/nested/UTF8/duplicate/null/unknown/trailing/type/time校验；Version1、ExecJournalRecord.State=unknown、start≤30秒，不存ticket/argv/env/stdin/stdout/full files。
- root/immediateparent/commands/bucket owner exact ManagementUID、0700，files0600/regular/single-link；canonical absolute path，不跟随symlink；root dirFD、openat O_NOFOLLOW/CLOEXEC、lifetime flock。ManagementUID等于实际管理UID，tests明确当前UID。
- complete write→filefsync→atomicrename→directoryfsync；创建目录fsync新目录+containing directory，Create最终fsyncparent。persist不确定/ctx取消poison且AccountingKnown=false，不删证据、不报告falseclose；OSfsync不承诺即时可取消。
- default256MiB，显式64KiB..256MiB；70%warn，预计新record≥85%拒绝，已有sameIDretry/Lookup/Close不被85%拦截。最多65536内容文件，新command预计达到65536拒绝保留gate temp槽；page128、hotpath无historyscan/cache。
- onlylinux/darwin，unsupportedconstructor明确ErrInvalidConfiguration；无etcd连接/watch/Lease或idle timer/goroutine；不改controlprotocol/etcd/Manager/config/legacyruntime/image、不移除仍在用Redis、不push/merge/deploy、不碰Sentinel编辑/用户配置凭证。
- LinuxRoot真实ownership fixture由controllerowns，networknone、binaryreadonlybind、test-ownedvolume、dropALL仅CHOWN；reuse gcr.io/etcd-development/etcd:v3.6.15@sha256:5ed4e32061aa061f970d6500629213e8579ef17571dcf9789074966acb9afad1。worker只编译testbinary/申请Root执行，不自行管理Docker生命周期；新异步测试release/cancel/≤5sjoin先于hookrestore/cleanup。

## Review Focus

1. pathname/symlink/hardlink/owner变更不能逃逸固定protecteddirFD或读取管理路径；真实FS反例归Task2。
2. ctx/IO错误发生于rename或dirfsync之后可能已经持久，不能falseack/重试危险行为；阶段故障与poison归Task2/3。
3. restart保留旧open、missingmanifest、identity/BootID变化、invalidtemp时不能默认为open或重建authority；cold构造归Task2。
4. 大量历史/极小文件/临界容量不能形成全量map或扫描hotpath，也不能阻断已有sameID查询；pagedaccountingTask2，point/capacityTask3。
5. evidence复制/同ID不同context/window/ticketdigest、已过期历史不能成为新cap或延长旧记录；strictcodecTask1，RecordUnknownTask3。

## Task 1: strict journal identity, manifest and command wire

**Files:** Create internal/runtime/controltarget/journal_types.go、journal_codec.go、journal_codec_test.go。必要时只同责任journal_validation.go；不写filesystem/constructor/physicalconsumer。

**Interfaces:** exactspec JournalIdentity/GateManifest/ExecJournalRecord/JournalOptions/JournalStatus与sentinelerrors；生产JournalIdentity.Validate()、GateManifest.Validate()、ExecJournalRecord.Validate()；private encodeGateManifest(GateManifest)([]byte,error)、decodeGateManifest([]byte,*GateManifest)error、encodeExecJournalRecord(ExecJournalRecord)([]byte,error)、decodeExecJournalRecord([]byte,*ExecJournalRecord)error。复制spec全部exact类型/JSON字段/time约束到taskcontext；Journal receiver留Task2。Task3消费ownedscalarrecord并比较完整canonical bytes。

- [x] 写TestJournalCodec/Identity，验证真实validmanifest/record、每个typed/nestedfield、canonicalzero/nilUUID/hash、UTC/window/businessE、open历史decoder但producerclosed差异；8192/8193 lexical bound、depth9、duplicate/missing/null/unknown/trailing/case/UTF8，失败dst保持原值，encoded无ticket/payload；max合法typed字段编码和时间长度报告。
- [x] `go test ./internal/runtime/controltarget -run '^TestJournal(Codec|Identity)' -count=1 -v` 在可编译stub下实际行为RED；编译/fixture错误不算RED。实现strict局部替换codec，无新公共全局validator/改既有protocol。
- [x] 相同focusGREEN+race、pkgvet/gofmt/diff/selfreview后立即coherent小commit。报告真实RED/错误/结果/字节、无filesystem或authority行为；独立spec+qualitygate后Task2。

## Task 2: protected Unix journal, durable closed gate and bounded recovery

**Files:** Create internal/runtime/controltarget/journal.go、journal_files_unix.go、journal_files_unsupported.go、journal_files_test.go、journal_gate_test.go；按责任可加journal_accounting.go/对应tests。platform文件显式linux||darwin buildtag及unsupported互补。NoRecordUnknown/Lookup producer（私有读写command primitives允许为Task3准备）。

**Interfaces:** 定义Journal私有mutex/FDs/currentmanifest/verifiedcounters/poison/closed；exactspec CreateClosedJournal/OpenClosedJournal/CloseGate/Status/Close。消费Task1codec/types，产生private read/write command point primitives和caller/closed/poison checks；实际helper签名在worker报告中明确供Task3，不让Task3猜。命名/字段/公开签名以spec为准，内部IOhooks只在真实syscall周围同步注入，无fake persistedstore。

- [ ] 写TestJournalProtectedFiles/Gate/Recovery：existingtarget拒绝覆盖、parentcanonical/0700/currentUID、root/childsymlink与hardlink/singlelink/unsafepermissions、actualflock第二opener/childhardexit释放、closedmanifest持久重读、historicalopen先durableclose、exactidentity/generation/BootID/epoch mismatch、missingmanifest不得从temp重建、malformed/validtemp保留与计量；新worker须真实subprocess+Fatal路径release/join。
- [ ] 写TestJournalPersistenceFaults：actualfile写/flush/rename/dirfsync前后故障和ctxcancel，falseclose不返回，possiblecommittedbytes保留，Poisoned/AccountingKnown语义及Close/idempotent/Closedpostchecks；新目录fsync链/parentfsync实际wrapper计数。TestJournalPagedAccounting真实1000files，Readdirnames每批≤128、无全量map/cachedrecords、root/record/temp字节及文件count准确。
- [ ] `go test ./internal/runtime/controltarget -run '^TestJournal(ProtectedFiles|Gate|Recovery|PersistenceFaults|PagedAccounting)' -count=1 -v` compilingbehaviorRED后minimalUnix实现。root-only owner/chown case在host明确skip/报告，不能mock stat代替。完整focusGREEN+race/pkgvet/gofmt/diff，按protectedIO/close/recovery coherent slices自审即smallcommits。
- [ ] workerDONE前交付LinuxCGO0 testbinary build命令与root-onlyselector，由controller exact ownedLinuxRoot fixture实际执行TestJournalProtectedFiles全selector并保存0SKIP/0FAIL证据（源相同；修改后重新必要selector）。完整实际report含平台skip/真实IO错误/workerjoin/FSrestart边界；独立gate后Task3。

## Task 3: immutable diagnostic exec intents and fixed point costs

**Files:** Create internal/runtime/controltarget/journal_exec.go、journal_exec_test.go、journal_exec_fault_test.go、journal_capacity_test.go、scripts/test-runtime-target-journal.sh；仅必要同package private helper edits，不改既有外部模块/镜像。

**Interfaces:** exactspec Journal.RecordUnknown/Lookup；消费Task2实际privatepointIO、状态mutex/checks/accounting及Task1strictcodec；input为controlprotocol.ExecStartEvidence，inputWire仅校验canonicaldigest/evidence一致性不持久化，完整Context/digest/windowowned保存。无Clock/provider/etcd/launch，不因历史过期否认diagnosticrecord。LinuxRootscript按spec项目label/trap/exactimage/binary/架构/volume约束。

- [ ] 写TestJournalRecordUnknown/Lookup：真实controlprotocol签名verify得到opaqueevidence再传入；zero/forgedunavailable、wrongjournalbinding/generation/runtime/BootID/gateepoch拒绝；firstwrite/samecanonicalretry、sameID不同fullcontext/digest/window冲突、input/copiedresultmutation不影响persistedfile；historicaltime不freshverify且永不cap；absent仅nil,nil、corrupt/oversize不能当absent。
- [ ] 写TestJournalExecPersistenceFaults：actualwrite/fsync/rename/dirfsync迟到cancel/error保留possiblecommit、poison拒绝再写、Lookupstrict可查；coldreopen仍closed/unknown且sameID不覆盖/延长；query/Close/Status并发与真实workerboundedFatal regression（若同步hooks0workers则明确不适用，无需虚假新增goroutine）。
- [ ] TestJournalCapacity/FixedPointCost：真实64KiB预算靠validrecords达到70/85/硬限，sameIDretry/query/close仍可用，newrecord拒绝且不删unknown；filecountlimit与boundedpaged统计。0/1000synthetichistory下aftercoldsetup reset实际pointIOcounter，准确read/write/fsync/rename/dirscancounts及recordbytes；no actuallargeN/QPS/p99claim。
- [ ] 编译behaviorRED→各slicefocusGREEN/race/pkgvet/gofmt/diff/selfreview即smallcommit；Root脚本shellsyntax检查+实际LinuxRoot全package run。report保留所有真实错误/skip与源冻结证据，独立spec+qualitygate。
- [ ] controller最终host全package/race、LinuxRoot全package0SKIP、全仓test/vet/build/scoped diff及一次fullNEWdelta+affectedintegrationreview；ONEfinalfixwave/scoped复核若必要，全部残留/rulings/permanent证据先保存。完成中间journal后继续认证target/实际PID1执行/双drain/tasks/scheduler/collector/GC/nativewiring/完整Redis移除，不停在前置层。

## 自审

| task或接口共享 | 产生/消费与一致性 |
| --- | --- |
| 1→2 | strictgate/record bytecodec+types由真实文件使用；public Journal留2，不出现未定义receiver。 |
| 2→3 | 真实pointIO/锁/counters/poison报告exact接口供3，避免跨任务猜测；公开unknown producer留3。 |
| Task1 | decoder支持historicalopen仅为Task2close，所有public写不open；全部context/time/8192/depth8规则有typed+lexical反例。 |
| Task2 | realUnixFD/fsync/ownership与close-only契约一致；root-only真实Linux gate由controller补足，hostskip不冒充通过。 |
| Task3 | authopaqueevidence写诊断unknown，gateclosed仍可记录历史但不产生authority；完整canonicalrecord阻止window变更，fixedpoint counter排除coldscan。 |

ReviewFocus五项分别由上述明确tests覆盖。正式spec已明确accounting未知、硬预算/cleanup可用性、platform/mode、目录fsync链、Status/Close同步，无待审批的外部动作。沿原批准架构与连续实施授权推进，所有判断错误代价在本单元ledger保存。
