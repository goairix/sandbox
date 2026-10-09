# 有额度的 workspace archive 恢复存储

这是已批准 Redis→etcd 总迁移中的 manager 恢复前置单元。用户要求持续实施、加快速度、小步验证提交；已有 sandbox 数量 N 是主要容量维度。本单元提供持久归档和原始归属，尚不执行冷恢复、不证明旧 writer 已退出，主程序仍使用 Redis。它接续 `0a8f9fff865cc84f00ac2a028e6ac089f410b8a3`，不重做已经验收的 checkpoint/GC。

## 范围和兼容

- 新增 `Options.TaskWorkspaceArchiveRecovery *TaskWorkspaceArchiveRecoveryOptions`；新 options 嵌入现有 `TaskWorkspaceArchiveSpoolOptions`，复用 Directory/Jobs/JobBytes/TotalBytes/Entries/JobIndexBytes/TotalIndexBytes。
- 与 `TaskWorkspaceArchiveSpool` 互斥，要求 `TaskWorkspaceSyncRecyclingBudget != nil`；Linux 支持，其他平台拒绝此 profile。nil 和原始随机 spool/delete-on-Close 流程保持现有行为。
- `New` 先复制 recovery 及关联 options，再验证自有副本，之后才可做文件操作。无依赖升级、main/配置/deployment 接线、新 etcd KV、常驻 goroutine、按 N 增长的 task map 或公共 authority API。
- 同一 recovery spool 第一次 local binding 持久后，后续文件只能使用相同完整 plan 和 canonical local attribution，不能换 target。

## 固定槽及额度

私有固定 `[64]taskArchiveRecoverySlot`，有效槽为 `[0, Jobs)`，`1≤Jobs≤64`。名称仅 `slot-00.tar`、`slot-00.json`、`slot-00.next` 到 `slot-63.*`；最大 192 个文件。目录绝对、clean、长度≤4096。没有 store marker，空目录产生随机 StoreID，所有有效占用槽必须属于同一 StoreID 和配置摘要。

manifest 上限 `M=65536` 字节，旧 `.json` 与新 `.next` 预留 `2M`。Recovery 的 JobBytes 是每槽完整文件内容额度，TAR 上限 `A=JobBytes−2M≥1536`。原 JobBytes≤`1<<40` 保持。每占用槽在任何创建、读入或归档传输前预扣完整 JobBytes；TotalBytes 不足拒绝，不能依实际长度或年龄退款。文件内容峰值≤TotalBytes 不代表文件系统分配块、日志或完整 RSS。

原 TAR index `I=33×nextPow2(2×Entries)`，Entries 上限 1,000,000 保持。Recovery 每槽内存预留 `I+4M`，要求 JobIndexBytes≥此值，按实际预留值计入 TotalIndexBytes。4M 用于有界输入/输出、typed metadata/owned copies；实现必须列明编码/解码的同时存活缓冲与字符串上界，禁止任意 Marshal/ReadFile 后再检查。metadata 更新全 store 串行；不得使排队更新额外分配 M 缓冲。固定目录扫描最多193个名字，额外缓存≤约49KiB；已有 TAR/JSON 库 scratch 和 Go allocator 不纳入完整 RSS 声明。全部加减乘法先防溢出。

live-owner map 仍只存已收费的原始 owner，长度≤Jobs。冷槽固定数组只有诊断数据，无 claim、consumer 或 witness。冷启动先有界分类/恢复收费，再验证完整 archive，最后开放新申请。

## 私有 manifest 与 codec

私有 `taskArchiveRecoveryManifest` 包括 version、slot、state、StoreID、limits digest、root/artifact 身份、history seed、可选 received、可选 binding。state 单向 `pending→received→bound`。

`taskArchiveRecoverySeed` 仅从当前已验证的原始 TaskClaim/archive draft 构造：TaskReference、task birth/digest、原 claim identifiers/revisions、final-sync attempt/revision/digest、archive command revision/digest、issuer revision/digest、ticket digest。保存供以后派生/点查的固定角色标识，不复制50条 keys/values、certificate、ticket bytes、credentials 或嵌套 archive Context。诊断数据不能替代以后 immutable history 与新鲜 receipt 验证。

received 保存实际完整验证的 SHA256、bytes、entries；binding 保存完整 plan wire、原 canonical local attribution 及与 checkpoint 一致的 TargetDigest。plan wire 显式含 Provider/StorageIdentityHash/Bucket/Prefix/Hash/Partition/Exclude/DeleteExtraneous/Digest，不直接 Marshal 私有 WorkspaceIdentity。解码后重建并核对 plan identity/digest；不是旧 execution authority。

codec 固定字段顺序、无 map/RawMessage，输出采用 `SetEscapeHTML(false)` 的编码语义，有界 writer/reader。可用只遍历此固定wire的有界writer消除标准encoder完整scratch，但合法最大wire须与标准JSON encoder的EscapeHTML(false)输出逐字节对照；不扩成通用JSON框架。拒绝重复/未知/缺失/null字段、非canonical数值/编码、尾随内容、超深结构，重新编码必须逐字节一致。pending 尚无 artifact identity 的合法初始化窗口需显式 schema；状态相关必需字段必须完整，不用零值猜测。

合法 path 禁止控制字符和反斜杠；quote 和 U+2028/U+2029 扩张≤2倍 UTF-8字节。exclusion 内容≤32768B、256个数组框架≤769B、local Root内容≤8192B、workspace内容≤3200B；固定 key/TaskReference/claim/worker/root数值和摘要 envelope 必须逐字段证明≤8192B，总上界≤53121B。最终 schema 需合法最大编码实测及逐字段上界，不拿普通示例替代。不改变 etcd 64KiB record、256KiB transaction、64 nodes、全部50 history pins、现有 checkpoint 和 GC事务。

## 原始写入流程

1. 在 `ReceiveTaskWorkspaceArchive` reserve前，从原 claim/draft构造 seed；recovery走私有 `reserveRecovery(ctx, seed)`，legacy仍走原reserve。
2. admission锁下预扣固定槽和全部额度，释放锁后写 pending `.next`→fsync文件→rename `.json`→fsync root。然后 EXCL创建TAR，捕获身份，再提交带artifact身份的pending；只有后一次durable成功才允许Open/Send/DATA。
3. `r.receive`成功返回意味着原Session.Close实际完成；持久received hook放在 delivery层，完成原currentGuard、文件身份和已验证TAR信息检查后fsync TAR，再提交received。不能在receive内部提前出现的witness处封存。
4. 在 `reservePutIntent` 已持 `s.begin`、`body.active`、`body.localTarget.begin` 登记后，紧邻 `reserveRecyclingPutIntent` 前调用私有 `sealOriginalBinding(ctx, source, body)`；核对原received/spool/source链，保存完整plan/local pin。durable bound和实际source/target postguard均成功后才允许reserve RPC，随后才可能本地效果。再次申请只接受相同冻结binding。
5. 编码/文件/rename/fsync失败、cancel或结果不明禁止进一步效果，不降级、不删除、不重建ACK。后续guard失败不删除durable received/bound，它们仍只是历史。

store.mu、archiveLife.mu和原nativeMu不跨文件IO、transport、RPC或实际join。专用metadata串行gate可跨metadata IO，但所有等待和IO都须有原spool/source/body/target生命周期登记并可取消；Close先封入口再cancel、等待实际owners/metadata/consumer完成。特别注意delivery-layer received seal发生在r.receive的ownerDone之后，必须额外登记，不能借已结束的receive登记覆盖它。
原始recovery初始化在释放短生命周期同步前封闭attempt，同prepared并发者须在任何收费/IO前拒绝。只有明确未收费的背压失败可重新尝试，已收费的不确定初始化失败保持封闭；私有reserve可用closed原spool+error表达已收费，只作状态判断，不授予文件或执行权限。初始化通过store active/ownerDone接入Close实际join，禁止新无界登记表或退款。

原spool接收ctx含SessionNotAfter截止时间；它只约束接收/admission/received封存。完整接收后的consumer/binding不能受旧发送会话截止时间撤权，必须独立受当前source/body/target/claim和spool实际Close封入口、取消及join约束。新profile必须实测旧会话真实过期而当前claim继续有效时的首次绑定和后续写入，不重跑旧已验收profile。

## 文件、冷打开和关闭

root以NOFOLLOW/DIRECTORY/CLOEXEC打开，UID当前用户、0700、dev/ino及目录路径与descriptor一致；持非阻塞排他flock。FD相对Openat，文件必须0600/currentUID/regular/nlink1，核对name与descriptor dev/ino。保存root文件系统/mount身份以及boot/namespace诊断；不兼容身份进入unsupported/quarantine，不授权执行。只读open不能follow symlink；所有失败关闭本单元实际持有的FD，不推断成功unlink。

扫描读取最多193个名字；未知名/越界槽/过多名使整个New拒绝admission并保留文件。已知槽的corrupt/orphan/`.next`残留先按一个完整槽收费，分类quarantine；其他确认空槽仍可用于新原始任务。`.next`绝不提升为当前manifest，不按内容/mtime挑较新版本，不自动清除。有效槽StoreID/config不一致不能混用。quota不足拒绝并保留。received/bound重开后严格检查身份、长度、完整SHA和TAR/index/entries，仅产生owned diagnostic copy。

私有 `coldSnapshot(slot)` 只能返回有界诊断copy，不能返回TAR capability、Prepared/source/witness/claim或可传入原Prepare的对象。没有本单元内retire API。Close保持原seal/cancel/实际join顺序，仅关闭FD/index，最后释放root锁；持久文件和job/disk收费不退款。EOF/checkpoint/冷manifest均不授予删除权限。

flock仅排斥遵守此锁的store进程；独立旧worker、root rename或继承FD不能据此证明旧物理writer排除。冷消费者必须留到下一单元，由新claim＋当前archive receipt查询＋旧writer精确join/fence验证后建立。

## 验收

- codec最大合法字段/状态及unknown、duplicate、missing、null、oversize/depth/非canonical拒绝；真正的峰值quota/算术溢出；64槽满拒绝第65槽。
- 实际文件tamper、mode、hardlink、symlink、rename、unknown/out-of-range名字、root/config身份变化；charged quarantine与空槽admission。
- 父进程保留真实exec.Cmd并每条路径实际Wait：持锁旧子进程拒绝重开，退出后只诊断重开；在pending、artifact身份提交、TAR fsync、received rename/root fsync、bound提交及首次reserve前窗口SIGKILL，重开无旧authority、不退款、不自动提升next。
- 新profile实际原始archive→两个file local流程验证首次reserve之前manifest已bound；跨文件不同target拒绝；首次seal失败零target效果/零reserve；source/target/Backend Close在实际metadata IO/postguard期间等待。
- 保留原始host race/native Linux三成员fixture JSON/raw、源SHA/实际testbinary、真实child Wait/精确fixture-label清理记录。只验证本单元，禁止重跑已验收单元。tmpfs/fsync和SIGKILL不证明断电持久性；故障注入失败与后续PASS分开记录。

本单元接受后仍缺完整恢复authority、旧writer排除、cursor/reconciliation、producer/传输恢复、完整同步/安全释放、production/partition/FUSE/容量/deployment/Redis移除。不能标成生产迁移完成或性能提升。
