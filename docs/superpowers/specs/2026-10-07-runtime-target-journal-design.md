# Protected runtime target journal 设计

这是已批准的 etcd 替代方案中，目标端持久状态的实现细化；执行意图单元已通过完整回归与集成审查，当前继续原授权范围内的可回滚实现，不做生产切换。

## 交付与边界

新增 `internal/runtime/controltarget`，提供真实本地文件持久化的 closed gate、不可变执行意图和有界历史查询。它是被可信 launcher 使用的被动存储，不能从日志或签名证据恢复执行能力。本单元不提供 set-open、执行、accepted/terminal 转换或按年龄 GC；后续认证目标端和实际 PID1 launcher 必须实现这些协议并验证进程状态。

首次安装只写 closed gate。打开既有日志时，校验原 binding 并先持久关闭当前 gate，再返回可用存储。相同 PodUID 的不同 BootID 不能接管旧日志；身份不匹配返回错误，保留全部数据。丢失 manifest、损坏记录、未完成持久化、未知文件或不安全权限不能自动重建身份或开放 gate。

整体阶段一至五、实际目标认证、可信时间、进程后代排空、FUSE 真实准备、任务与双层准入、生产 wiring、Redis 移除仍然必需完成。当前单元的编解码和文件测试不能替代这些验收。

## Exact 类型与接口

`JournalIdentity` 的 snake_case JSON 字段：Namespace/AuthorityID/Target/RestoreEpoch/SandboxID/WorkspaceHash string，Generation int64，Runtime controlprotocol.RuntimeReference。身份规则完全继承现有 controlprotocol：Namespace 绝对目录形式、至少三个非空合法 segment、最多512字节；AuthorityID/Target 与 runtime ID/UID/BootID 为1..128字节合法 UTF8 无控制字符；RestoreEpoch/SandboxID 为1..128字节合法 segment；WorkspaceHash 为小写64hex；Generation>0。所有字段必须明确存在。

`GateManifest`：Version uint32=1，Identity JournalIdentity，DataGateEpoch int64>0，GateState string（只允许 open/closed）。初始安装和本单元所有公开写操作只能得到 closed；decoder 能读取历史 open 以便安全关闭，不能凭它授予能力。

`ExecJournalRecord`：Version uint32=1，State string=unknown，Context controlprotocol.ExecStartContext，DescriptorDigest/TicketDigest string，NotBefore/NotAfter time.Time。完整 Context 与当前 journal identity/gate epoch 匹配；遵循现有 exec context 的全部字段、UUID、hash、UTC、positive revision/Lease 规则。两个时间为明确 UTC 非零值、NB<NA、区间≤30秒且 NA≤Context.ExpiresAt。ticket/descriptor digest 为小写64hex。记录不包含完整 ticket、argv/env/stdin/stdout 或文件内容。unknown 是没有物理终态证明的意图，不表示命令已经启动。

`JournalOptions`：Directory string（已存在的可信 canonical absolute parent 下的目标路径），Identity JournalIdentity，DataGateEpoch int64，ManagementUID uint32，MaxBytes int64（0→256MiB；显式预算范围64KiB..256MiB）。真实运行要求 UID 与当前管理身份相同；测试明确使用当前测试 UID，不能声称通过此项已具备 root/PID1 隔离。目录路径不是 API 用户输入。

公开 API：

- `CreateClosedJournal(ctx context.Context, options JournalOptions) (*Journal, error)`：只允许目标目录不存在；创建0700 root、commands 与0600 lock/manifest；不得覆盖现有目录。
- `OpenClosedJournal(ctx context.Context, options JournalOptions) (*Journal, error)`：要求已存在完整日志、exact identity 和 epoch；取得独占锁，分页校验/accounting，先 durable close，后返回。没有新启动能力。
- `(j *Journal) RecordUnknown(ctx context.Context, evidence controlprotocol.ExecStartEvidence) (*ExecJournalRecord, error)`：只消费 authentic opaque evidence，复制 Context/digests/window 并校验整个 identity 与当前 epoch；是诊断存储，不作当前有效期/Lease/gate-open授权。首次不可变写；同 command/full canonical record 返回已保存同一记录；任何摘要/context/window变化冲突，永远不能延长旧命令截止时间。
- `(j *Journal) Lookup(ctx context.Context, commandID string) (*ExecJournalRecord, error)`：单文件有界读取，absent→nil,nil，返回 owned value，仅 unknown history；不读取调用方指定路径。
- `(j *Journal) CloseGate(ctx context.Context, expectedEpoch int64) (GateManifest, error)`：exact current epoch，幂等 durable closed；不能递增 epoch 或开放 gate。回执只证明本地 closed 状态，不证明 accepted operations 已排空。
- `(j *Journal) Status() JournalStatus`：copied GateManifest、LogicalBytes/Records/TemporaryFiles counters、Warning/NewRecordsStopped/Poisoned/AccountingKnown/Closed flags。不是可用于授权的 live receipt。
- `(j *Journal) Close() error`：关 FD/释放 flock，不删除证据；双 Close 幂等。不是物理 End。

错误为可 errors.Is 匹配的 ErrInvalidConfiguration/ErrInvalidRecord/ErrIdentityMismatch/ErrConflict/ErrBusy/ErrCapacity/ErrJournalUnavailable/ErrClosed。系统错误保留原 cause。ctx nil 拒绝；所有公开写读方法同一 mutex；跨进程由实际 flock 拒绝第二 opener，不使用 PID/mkdir 锁冒充。

## 编解码与持久布局

manifest/command 每个文件最多8192字节；读前 stat 大小，再有界读取。strict nested schema 必须拒绝缺失、null、duplicate、大小写替代、unknown、trailing token、非法UTF8及 typed/time 非法值，失败不修改 destination。采用 canonical encoder、SetEscapeHTML(false)，精确 opaque 字符/时间参与同记录比较。签名证据由 controlprotocol 产生，不为 passive journal 新增 signer 或 Clock 调用。

布局固定 `gate.json`、`lock`、`commands/<commandUUID前两hex>/<commandUUID>.json`。UUID canonical lowercase 且非 nil，文件 bucket 必须匹配。目录0700、文件0600，owner exact ManagementUID；只允许 regular single-link 文件，不跟随 symlink。保留 root dirFD，以 `openat` + O_NOFOLLOW/CLOEXEC 操作；commands 和 bucket FD 同样 pin/校验。首次没有256个空 bucket，只按需创建。复用已有 `golang.org/x/sys/unix`，不引入新依赖。支持 Linux/macOS 测试；不支持系统明确拒绝配置，不退化成无锁模式。

持久写：exclusive temp file → complete bounded write → file fsync → 同目录 atomic rename → directory fsync。gate temp `.gate.<nonce>.tmp`；command temp `.<commandID>.<nonce>.tmp` 与 command 在同 bucket，nonce 为 canonical nonnil UUID。已存在记录不能被另一 request 覆盖。store mutex/flock 保证正常写者唯一；恶意同管理 UID 或 raw root 修改不在用户进程边界内，部署必须隔离该身份。

任何 write/fsync/rename 不确定或操作中 ctx 取消后，禁止新 admission：返回 error、设置 Poisoned，并保留现有/可能已经持久的全部 bytes。不能靠重试把 unknown 当作失败。可用历史 query/close 只读结果必须仍验证文件；CloseGate 的新成功只在 file+dir fsync 完成并检查 ctx 后返回。OS fsync 本身不能通用即时取消；使用受支持的本地持久文件系统，逐步前后检查 ctx，明确这是可用性限制，不能以此允许迟到启动。

重启不删除残留 temp 文件：验证布局/完整合法内容/binding，计入 TemporaryFiles/LogicalBytes；遇到不完整/畸形 temp 则 fail closed 并保留供恢复工具审计。gate.json 缺失绝不从 temp 推断初始化成功。此阶段不实现自动 repair/recovery 或物理 replay。归档/恢复协议后续必需定义。

## 容量与遍历

LogicalBytes 是实际支持文件内容长度之和（含 manifest、command 和 temp，不含文件系统 block/目录元数据）；不是实际磁盘容量保证。报告 root/FD/目录开销另行实测。默认256MiB，≥70% Warning，预计新增写使使用量≥85%时拒绝新的 unknown record。原有相同记录查询/同身份重试、CloseGate/Close 不被85%阈值拦截；仍可能因真实磁盘错误失败。最多65536个内容文件（manifest、command 和 temp；lock 无内容），超限返回 ErrCapacity。新 command 写入的预计文件数达到65536即拒绝，保留一个 gate temp 槽；防止大量极小文件耗尽 inode。unknown 无年龄删除。

启动按固定层级分页 Readdirnames(128)，只保留128个候选与当前读 buffer，不缓存全部 command。RecordUnknown/Lookup 为固定深度点操作，不遍历历史。每次 write 后有界更新 accounting；未知错误 poison 后不能按内存计数继续新写。打开既有 store 才做受 ctx 检查的分页校验/计量，工作量随该 runtime 自身日志历史增长；不能将它描述为全系统恒定恢复成本。

## 验收与下一消费者约束

实际 fsync/rename/重启/进程锁测试；故障注入必须围绕真实 file operation 同步执行，分别覆盖 write/file fsync/rename/dir fsync 错误和取消，证明没有 false close ack/新启动能力。子进程 hard-exit 后锁释放，另一 opener 校验 retained unknown、closed gate、完整 bytes；所有测试 worker release/cancel/≤5s join 先于 hook/fixture restore。真实 symlink、hardlink、wrong owner/mode/oversize/unknown filename/bucket mismatch、malformed temp/manifest 缺失、same ID different record、immutable window、容量临界和分页数量测试；0/1000 synthetic history 下点操作计数相同，但不是实际 Pod 或 QPS/SLO 验收。

后续 target verifier/physical launcher 必须在 authenticated sender、当前 active issuer、exact incarnation、实际 descriptor、受控时间、原 metadata authority 和 gate 校验后，使用日志记录且在 ack/launch 前持久化。签名、日志存在、CloseGate、Context 取消和 Lease 删除都不是实际终态。重启 unknown 不 replay，开放新 epoch 必须由合法 task+双层排空协议实现，无法证明时保持关闭。


## 补充的 exact 持久化与平台契约

创建路径的 immediate parent 必须已经存在、canonical absolute、无 symlink、owner exact ManagementUID、权限0700。fixture 使用自己0700 temporary parent并显式 EvalSymlinks 消除平台 `/var`别名，store 不静默跟随 caller 路径。root/bucket/commands 的新目录创建后，必须 fsync 新目录及其 containing directory，使路径持久化链完整。Create 最后还 fsync parent 才成功返回；部分创建失败保留证据，关闭已打开FD，不自动 rm。Open 要求 existing gate，验证后先 fsync durable closed；任何阶段失败返回 nil,error并释放自身lock/FD，旧证据保留。

JournalStatus 的具体字段为 Gate GateManifest、LogicalBytes int64、Records/TemporaryFiles uint64、Warning/NewRecordsStopped/Poisoned/AccountingKnown/Closed bool。所有状态和Close/读写同mutex。成功重新计量后AccountingKnown=true；任何不确定写/取消后Poisoned=true、AccountingKnown=false、NewRecordsStopped=true，计数只是最后已验证快照，不装作准确物理字节。Poisoned下 Lookup仍验证真实文件，可查询诊断；RecordUnknown/CloseGate返回ErrJournalUnavailable，不追加更多不确定temp。Close仍释放资源，第二次Close幂等nil；Closed后读写ErrClosed，Status返回最后 copied快照且Closed=true。Status不读取磁盘，不是授权证据。

百分比采用整数比较 used*100>=limit*70/85，预计新record字节达到85%就拒绝；先检查已有同ID记录，原记录不因capacity被拒绝。单次gate临时写还必须满足硬 MaxBytes/文件数预算；真实磁盘/硬限不能保证cleanup始终成功，失败保持关闭/不可用且不删unknown腾空间。文件 accounting在完整persist成功后更新；失败标记未知，必须cold reopen分页重算后才有已验证counter。Page大小128，JSON/record最大8192，JSON nesting≤8；目录深度固定，单次hotpath不 scan/relist。目录 inode/block分配与fixedFD开销不计入LogicalBytes，实测另报。

新增文件只在本package；支持 linux/darwin，unsupported平台constructor明确ErrInvalidConfiguration。不复用仅做词法路径限制的 storage.ScopedFS 为dirFD安全实现；不修改现有 storage/etcd、controlprotocol/Manager/config/image。已存在 golang.org/x/sys v0.47.0 足够，本单元无新dependency。

macOS host focused/race覆盖实际FS、flock、hardlink/symlink、ctx/IO故障与分页；需要真正chown的ownership cases明确 root-only。Task2独立gate前，controller在Linux UID0隔离fixture上实际跑owner/chown tests，不能以mock stat取代。最终Linux全package native gate0FAIL0SKIP；hostrace的root-only skip单独报告，不能声称全平台全部测试0skip。LinuxRoot fixture复用已经pinned/local存在的 gcr.io/etcd-development/etcd:v3.6.15@sha256:5ed4e32061aa061f970d6500629213e8579ef17571dcf9789074966acb9afad1（当前Docker arm64）。只将CGO_ENABLED=0/GOOS=linux test binary只读bind到container，entrypoint为该binary；network none、drop ALL仅加 CHOWN（用于异UID文件case）；只创建test-owned卷作/tmp，无host可写mount/凭证。实际store process restart与flock测试用该testbinary子进程。此fixture不运行etcd，不证明掉电/host故障，Linux native不是Linux race；hostrace与Linux原生证据分别保留。fixture生命周期controller owns，Task3交付可重复script、Root运行。

Root脚本固定project/name前缀sandbox-target-journal-test-<pid>-<unix>，创建container/volume都带sandbox.test.project及sandbox.test.fixture=runtime-target-journal标签；trap只移除该exact owned资源与自己mktemp scratch binary，保留实际test exit，不清理其他Docker项目。Linux镜像架构从实际image inspect取得（arm64或amd64），不得把host架构当daemon证据。纯Go test binary不提供CGO/race，编译命令明确 -c而非以crosscompile当测试通过。

## 存量与历史预算继承

本地journal只按实际内容占盘，不预分配256MiB/Pod；hotpath固定文件点操作，cold scan成本随该runtime自己的history增长，不能声称全部runtime重启恒定成本。idle target也有固定process/FD/disk元数据开销，实际PID1阶段必须测量并保持原用户资源配额。此单元0/1000合成历史不是1万/10万实际Pod，也不是p99/QPS。

etcd当前preGC creation/publication示例20KV、12769/16847/20945 rawvalue bytes包含中间证据，不是最终B_live。新增exec历史worst-valid record8126B：100exec/s×3600s仅record约2.72GiB，receipt/status/MVCC/WAL/replication另加；typical约2.7KiB也需要安全GC与背压。大量已有idle N、历史申请/command λ×retention、历史workspace U和unknown必须分别预算。Root完整目标端、GC、长跑容量验证继续必需。

## 自审结论

类型和公开API无未定义producer；闭门/unknown只作被动证据，不产生physical authority。strict codec、真实filesystem/ownership/persist failure、counter/bounded replay各有独立任务及测试。已决定create/fsync parent链、poison/AccountingKnown、Close/status同步、canonical parent、平台build和LinuxRoot实际ownership gate。首次closed安装、历史open先close、身份不匹配不重建，与整体安全协议一致。未来activate/accepted/terminal/GC/realPID1不是本单元接口，必须由紧接的合法target协议完成，不能因基础层通过而停止整体迁移。
