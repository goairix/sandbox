# 原始 workspace sync checkpoint 与 GC 原子提交验收

源码提交 `7fd87c1de9de5d790d8568d522b344e29a296175`（compatible schema）、`a483a7b1fe6fe6c7aba00bcd8acea225aeada12a`（原子GC）、`e398f41a7e5d01c28df81f6c804f601cac920af7`（限定测试）。原始成功local文件回收时，在已有task checkpoint中持久进度，与free slot/intent删除同一事务提交，消除了先回收后记进度的窗口。主程序仍Redis；本次没有冷恢复、完整manager恢复、sync-complete或owner安全释放。

v1保留独立六字段wire及原解码策略；v2 mandatory固定15字段、canonical/digest/数值/native envelope严格验证，4096B以内，owned诊断copy。claim bundle验证原task birth/initial attempt；legacy Prepare拒绝降级，已准备的旧stage仍受旧checkpoint generation CAS限制。原始witness/draft/settlement登记与source/body/target生命周期保持，nativeMu在physical postguard前释放。

GC保留全部50历史pins、prefix/budget：61 comparisons＋free Put/intent Delete/checkpoint Put三writes，空failure分支，共64nodes。unknown在新鲜guard下同点观察slot/intent/checkpoint，严格匹配原Create、nextVersion、canonical值和共同ModRevision，不重发mutation或以之后复用结果升级原权限。不增加key/N actor，不提高wire预算。

## 验证及源码世代

- host真实RED：合法v2被旧decoder以ErrCorruptRecord拒绝；native真实RED：实际首文件GC成功后checkpoint仍v1，无同步进度。原失败与退出/cleanup保留。
- schema host race8实际叶，protocol host race13实际叶，最终限定host race14实际叶PASS，零FAIL/SKIP；这些集合重叠，不累加。初次最大wire测试因fixture operations prefix非法失败，修fixture后通过；生产校验未改。vet/Linuxarm64CGO0 cross/format/bash/diff均exit0。
- native成功覆盖39不同实际叶：core两文件/65文件2叶；较大批次先通过14叶；未完成的23叶分五固定小批通过，均零SKIP。原较大批次另有23FAIL，不能称完整一轮全绿。覆盖checkpointmissing/corrupt/foreign/reconciliation/CAS冲突、迟到旧stage拒绝、unknown lost/malformed/escaped/reused、claim/restore/root变更、source/body/target在新增读取、GC RPC与postguard的实际关闭等待。
- S1单原始source实际65文件验证checkpoint Create保持task birth、Version2..66、ordinal3..67、bodyend及GC/free同MOD，最终256slots/0intents和65物理文件。诊断payload独立copy、legacy零stage grant拒绝和新claim读取v2已实际验证。
- **core两/65文件是7fd87c1＋当时未提交协议快照**，不是clean a483。a483前随后增加checkpointPayload Version2/Local非空防御；原reserve和settlement前提已保证这些条件。后续14＋23native叶运行a483最终生产源码；e398仅改变tests/selector。最终cross绑定e398全部source/test字节，不将65文件说成最终二进制重新运行。
- 最大合法checkpoint样本3002B；实际worst GC64nodes/request94759B/reply204B/47B，第65节点拒绝；54cmp单点读取58nodes、三点unknown读取60nodes，最大回复24421B。所有原限额/pins保留。

## 审查与独立审计

唯一fresh reviewer完整三提交单遍审查并定点检查原登记/Close/native锁、strict匿名wire和loader/旧CAS。**SpecCompliant、Missing0/Extra0、C0/I0/M1、Approved**；Minor是失败批次/告警的证明范围限制，无源码阻断项，不重跑套件。

Root审核354个稳定artifact的bytes/SHA256、全部host/native原始leaf、12条当前working/Git源码绑定、9个LinuxCGO0二进制及最终231个**目标包**选中编译输入。231为该包Go/Test/Cgo/XTest选择，不是全依赖SBOM或host全部输入。后续六native批次162条production-source绑定与最终源码一致。Root在内存用a483版本移除已说明的防御前提，得到core checkpoint源码5757B/SHA `6343b9f3cf03bbdd5a0c795693442e99502b008710e350116577ee5d24659b78`，与原始编译捕获相符，无文件或测试修改。RED早于扩展compiler/Git记录，只有原hash/binary/前后match；不补造完整来源证明。

8个原环境24个真实child join、完整三server日志、8个原attach Wait、退出与cleanup已核对；Root使用脚本实际project+fixture labels重新独立查询24次container/volume/network，全部exit0且stdout/stderr为空。没有重新运行作者测试。

作者报告SHA256 `ac9a1db2122dbaa9ff1c85631e48552c4ef78ca1eec1290a5dd6864e84091ad9`；manifest SHA256 `4f6d6985b70a8908c0c70d054822e76de4a358d9ee4f264767f3bcb3c1562dff`。初manifest因来源说明修订，先保留原字节（SHA `efc869604d6381612f6ba30c092431c9d93c66540d4e6b82ecab440e037742bb`）再替换；最终排除live ledger/生成输出/自身，Root完整核对通过。封闭本OWN后不得重读/重派已完成单元，本报告保留结论。

## 限制与后续

原较大native批次14PASS/23FAIL：legacy初始化GuardExpired/leaderchanged，原local1实际Wait为signal:killed，随后22用例依赖已退出child而失败。**SIGKILL原因UNKNOWN**，OOMKilled=false不排除子进程OOM；没有原cgroup证据，不推断资源容量，也未提高资源或声称修复。五固定小批补齐功能覆盖，不能替代可靠性/容量证明。

八环境JSON总INFO2790/WARN488/ERROR92，其中client WARN12，server WARN476/ERROR92；失败及配置、health、peer、leadership、真实关闭记录全部保留，不统称无告警。native CGO0非race；tmpfs fsync非断电持久性；postguard hook在actual check之后，不证明整个syscall期间状态。65文件实际耗时不是性能基准。

checkpoint只是诊断与后续恢复输入，BodyEnd不是TAR随机resume地址；归档持久manifest、旧writer物理排除、新claim/worker权限和实际恢复仍需实现。研究见 `2026-10-09-workspace-sync-recovery-preparation.md`，它没有实现验收。之后安全释放、生产接线、分区/FUSE/collector、大N性能容量、部署恢复及Redis移除仍未完成。
