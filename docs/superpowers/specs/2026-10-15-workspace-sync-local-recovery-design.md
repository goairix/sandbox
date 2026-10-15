# 新权限下本地普通文件恢复执行

此为用户已批准etcd master正常故障恢复的实施细化，接续2026-10-15持久归档验收与2026-10-09恢复接口准备；不再选型，不重新授权旧指针图。一个功能单元交付冷bound归档→fresh claim→认证历史query→同步writer排除→旧charge结算/checkpoint→剩余regular files。main仍Redis，完整目录/delete/sync-complete/安全释放及生产切换另有明确后续。

## 公共边界

```go
func (b *Backend) RecoverTaskWorkspaceSyncLocal(ctx context.Context,
    c *TaskClaim, slot uint32, destination *controltransport.Destination,
    worker *launcher.KernelBoundary) (TaskWorkspaceSyncLocalRecoveryResult, error)
```

结果为owned诊断：Checkpoint *TaskWorkspaceSyncCheckpoint、RegularFilesComplete bool、CompletedFiles/CompletedBytes uint64。两计数只含本次调用实际核验并完成持久checkpoint的文件/字节；错误时可返回已完成进度，不能宣称全量成功。无可执行source/witness/claim句柄。只支持Linux、recovery+recycling profile、冷bound local plan；nil/错误slot/其他provider/pending/received/quarantine fail closed，无写入。旧public API和原始validator不放宽。

## 归档和生命周期

- store中既有64固定槽各至多一个recovery owner，登记在store.mu下并与Close封闭/Add/Wait共门；无按已有sandbox数增长的map/actor。slot原磁盘/内存已收费，不重新增加job或退款，不新增retire接口。
- 登记先于metadata/TAR/query/native/target IO。metadata gate下重读canonical manifest、store/root/artifact身份，重新有界打开TAR并验证长度/SHA/结构/index。已收费额度I+4M不放大，等待者不分配M buffers；执行owner/entry/body/result仅有界当前项。
- 同一未替换root inode的实际锁只有在原source/body/target/native/metadata/settlement/descriptor的同步IO登记全部join后释放时，才构成合作writer排除。新owner接同一seal/cancel/join/root-close-last顺序。禁止savedPID/JSON身份证明退出，禁止另锁replacement root绕过旧writer；没有FD委派、detached writer、mmap/AIO/io_uring能力。
- store.mu/archiveLife.mu/nativeMu不跨物理IO、query或join；metadata gate可跨有限metadata IO且须登记/可取消。所有失败路径关闭真实descriptor、等待实际工作，不以ctx取消当退出。

## 当前权限与历史查询

- c必须是本Backend真实ClaimTask产物、当前有效、同task/common birth，CreateRevision严格晚于producer原claim及archive command；恢复不得借旧claim续权或填c.finalSyncDraft。
- PrepareAfterDualDrain取得当前新鲜双排空前置；完整解码immutable archive command/final-sync/quiescence/issuer。核对manifest全部seed的taskbirth、原claim canonical bytes/digest、revision/value/digest、原archive ticket/context、原plan和旧claim顺序。旧claim canonical value只作metadata，不构造旧TaskClaim。
- current claim完整fences与六历史pins保留，具体组装以现有50pins为准，原生wire逐次预检≤64nodes/256KiB；不重复拼dual-drain比较，不增加etcd key或attempt log，不修改原slot/intent/checkpoint编码。
- query只送原context/ticketDigest，旧ticket/cert/quiescence payload为nil。要求单ArchiveComplete、认证EOF、实际Session.Close成功、当前有效runtime certificate和精确归档receipt事实匹配；前后有界native guard。历史窗口过期不验证为当前发送权限，也不重发DATA/原命令。query/Close不明不授予执行。
- 新鲜current权限、claim失效/期限/Backend.Close贯穿source/body/target/terminal/GC；只允许活跃job的有界monitor，无idle sandbox常驻循环。current guard后不得释放登记再执行物理副作用。

## 稳定目标与进度

- 完整旧canonical Local重算manifest TargetDigest，并核对冻结plan/所有归档事实。用当前真实KernelBoundary经现有openTaskWorkspaceSyncLocalTarget建立新pin；root路径/dev/inode/mount/filesystem/name/UID/GID及boot/PID/mount namespace需连续。新incarnation/PID/starttime不等于旧worker是允许的，禁止把旧digest替换为新worker摘要。
- cursor从TAR起点走到checkpoint ordinal；核对pathDigest/bodySHA/BodyEnd/intent birth和全部context。BodyEnd不是header seek；不truncate已checkpoint文件。strict同context和单调ordinal/BodyEnd不放宽。
- checkpoint可属于更旧恢复claim：common birth/初始attempt/context严格一致，旧KV ModRevision须早于fresh claim CreateRevision；本次后续点必须属于当前claim。使用独立恢复builder，不修改captureWorkspaceSyncCheckpoint原规则。
- 目录header可验证并走过，父目录沿现有regular put需求安全建立；不应用空目录/delete-extraneous，也不产生完整sync/task终态。EOF时所有regular文件已checkpoint才RegularFilesComplete=true；仍无owner释放许可。

## 遗留charge与新副作用

- 最多逐点扫描配置S∈[1,64]的固定partition recycling slots，每次只保留当前charge，不扫描已有sandbox、etcd任意prefix或建立全文件集合。其他task槽保留收费。匹配本task的旧intent须核对exact immutable bytes/birth、slot generation/MOD、旧claim顺序、同archive/plan/path/body/hash/mode及稳定target；歧义拒绝。
- old IntentID/birth保留，不重发原reserve，不设置旧acknowledged，不复造旧draft/witness，不根据当前文件已存在猜旧put成功。
- fresh native权限、认证query及实际writer排除允许独立local repair：同path/body/hash/mode下重新核验/fsync或安全修复同步regular file。复用机械scoped-open/copy/hash/fsync/Close代码；原与恢复分别登记/guard/one-shot并生成不同私有完成对象。
- domain-separated recovery completion digest绑定old immutable intent、fresh claim/worker、实际file/body/fsync/descriptor join。CAS同charged generation的occupied或terminal为fresh terminal，始终保留原attempt outcome unknown；opaque旧digest不是authority，每次重启都重新物理核验。
- terminal后原子checkpoint+free slot+delete exactintent；保留50pins、≤64nodes/256KiB。lost ACK对当前attempt只读仲裁，不重传；unknown保持charge，失败不退款。新剩余文件intent使用现有v2 schema、当前claim/currentLocal及原archive/final-sync归属，checkpoint始终旧TargetDigest。
- 不复制整段原生txn授权或local syscall loop，不引入通用capability框架；只抽出私有mechanical cursor/local executor/native transport必要共用段，旧原始validator不变。

## 验收与完整需求边界

真实三成员etcd/Linux非特权实际worker：冷bound+fresh claim、真实producer窗口过期后的query、旧权限/ABA/窗口/多帧/EOF/Close错误拒绝；旧writer在write/fsync/close/postguard/terminal/GC未join阻挡同inode重开；truncate/partialwrite/fsync/terminal/GC lostACK反复恢复；两文件和65文件continuation且已完成文件无truncate；target/root/boot/namespace/tamper拒绝；Close各登记窗口实际join；worst-case protobuf50pins/64nodes/256KiB。

保留每源epoch原始raw/failedrun、实际Cmd.Wait、精确fixture清理；只跑新增与真实受影响兼容测试，已通过且源码未变不重复。固定512MiB fixture分有限组，失败原样保留；不把分组PASS或tmpfs SIGKILL当容量/断电验收。不新建全依赖SBOM/统计框架。完整producer/transfer/主机丢失、runtime supervisor重启、独立writer部署、目录/delete/sync-complete/cloud、最终release均不因此完成。
