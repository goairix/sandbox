# 本地归档恢复执行接口准备

这是fresh `workspace_sync_recovery_execution_contract`的只读集成设计，未实施、未运行测试，不是恢复或生产验收。上一持久归档单元仍在R2审查修复。主程序仍Redis。本报告延续已批准master的正常故障恢复要求，避免再次开展存储选型或重复历史组件验证。

## 一个完整功能单元

下一Task交付冷bound archive在fresh claim下的本地regular-file恢复执行：历史producer认证查询、实际旧writer排除、新source/body、旧charged intent结算和checkpoint后继续剩余文件。public入口建议：

```go
func (b *Backend) RecoverTaskWorkspaceSyncLocal(
    ctx context.Context, c *TaskClaim, slot uint32,
    destination *controltransport.Destination,
    worker *launcher.KernelBoundary,
) (TaskWorkspaceSyncLocalRecoveryResult, error)
```

新claim必须来自本Backend并严格晚于旧producer claim。返回owned进度诊断和RegularFilesComplete，不返回source/释放权限。所有准备、执行、Close在wrapper内。现有原始API/validator保持严格，不填c.finalSyncDraft，不重建旧Prepared/Received/witness指针链。

## 建议文件与接口

以下新增文件位于internal/storage/state/etcd；正式spec/plan还需固定完整类型与最终签名。

| 文件 | 私有职责/接口 |
| --- | --- |
| task_archive_recovery_authority.go | pinTaskArchiveRecovery(ctx,c,manifest,quiescence)；authority local/guard |
| task_archive_recovery_query.go | authority.queryProducer(ctx,destination)，只返回认证历史receipt |
| task_workspace_sync_recovery.go | public wrapper、prepareTaskWorkspaceSyncRecovery、source.run/Close |
| task_workspace_sync_recovery_source.go | begin/end/next/openBody；独立recovery entry/body Read/Close |
| task_workspace_sync_recovery_intent.go | readCharged(ctx,slot)、reserveNext(ctx,body)；独立charge含immutable bytes/birth/current slot和one-shot状态 |
| task_workspace_sync_recovery_local_linux.go及unsupported | reconcileLocal(ctx,body,charge)，返回独立fresh completion |
| task_workspace_sync_recovery_checkpoint.go | restoreCheckpoint/settleLocal/recycleLocal；保留manifest旧TargetDigest |

现有fixed64 archive slots增加单owner字段；store.mu下seal/Add与Close/Wait连接，不增加按N增长的map。机械local syscall loop可从task_workspace_sync_local_put_linux.go抽出供严格原始put与fresh reconciliation共用；两者分别持有自己的guard/registration/one-shot与witness/completion。TAR bounded walking/hash/index可抽出私有cursor，返回value-only metadata；每个source自行seal entry/body。仅共享native wire/response transport，不合并授权比较或放宽旧validator。

## 权限和生命周期

1. 已charged槽在任何metadata/TAR/query IO前登记唯一recovery owner；同槽第二owner拒绝。重读canonical bound manifest/root/artifact，guarded reopen并重建有界index；cold诊断检查不是execution grant。
2. fresh PrepareAfterDualDrain后完整解码command/final-sync/issuer/quiescence，逐项对manifest seed exactbirth/revisions/bytes/digests/旧claim顺序/plan。旧claim canonical value仅作为metadata重建，不作为TaskClaim。每次真实effect保留当前完整claim fences和历史pins；不得重复拼入dual-drain比较超64nodes/256KiB。
3. query仅送原context/ticket digest，不送旧ticket/cert/quiescence payload；单ArchiveComplete、认证EOF、实际Session.Close成功、receipt归档事实匹配、传输前后native guard。历史发送window过期不重新授予发送authority。
4. 原始source/body/target注册和实际descriptor/postguard/settlement joins已经连接store.root.Close最后执行。新owner接入同一个生命周期；同一未替换inode锁的取得仅在这个合作同步IO边界排除旧writer。禁止detached writer/FD委派/mmap/AIO/io_uring，不使用savedPID或flock-alone证明。
5. 先以完整旧canonical Local重算manifest TargetDigest和checkpoint，再单独建立fresh KernelBoundary/current Local pin。稳定root路径/dev/inode/mount/filesystem/UID/GID和boot/PID/mount namespace必须连续；新incarnation/PID/starttime与旧writer不同。replacement root锁不能绕过旧writer。

## 继续与结算

TAR从头解析至checkpoint ordinal，比对path digest/body hash/BodyEnd/归档/plan；BodyEnd不能作为header seek。已完成文件不truncate。最多扫描配置固定recycling slots，一次有界点查，无文件actor/history集合。

Root ruling：允许在fresh current native权限、认证历史query、实际合作writer排除与stable target连续性下进行独立local repair；它不是重发原reserve或宣称旧put成功。限定同计划/path/body/hash/mode、exact oldintent bytes/birth/chargedslot；原IntentID保留，不设置旧acknowledged、不退款。只限同步regular-file，不扩展到cloud/delete等非幂等副作用。

fresh completion以domain-separated digest绑定old immutable intent、fresh claim/worker及实际body/file/fsync/join事实，CAS同generation slot到新terminal；再原子checkpoint+free+delete exactintent。occupied或旧terminal均须fresh物理重验证，不把opaque旧EvidenceDigest当authority。lost terminal/GC ACK只读仲裁，不重发该attempt。recovery checkpoint永久保留旧TargetDigest；fresh后续intent使用新claim/currentLocal和原archive/final-sync归属。若现有schema或64nodes不能容纳，正式设计须明确修订，不能静默新增attempt日志或放宽caps。

## 必要验收与边界

- producer window实际过期后的fresh claim历史query；history ABA/顺序/完整wire拒绝。
- write/fsync/close/postguard/terminal/GC中旧writer未join时同inode reopen失败；实际join后允许。
- truncate/partialwrite/fsync/terminal/GC lostACK后的重复恢复：charge/IntentID保留，不重发原attempt。
- 两文件与65文件继续，已完成文件不truncate，有界cursor/recycling；fresh worker稳定root成功，替换/namespace/boot/tamper拒绝。
- recovery admission/query/native/filesystem Close竞态，真实Wait/cleanup；actual worst-case50pins/64nodes/256KiB wire。

这只完成regular-file replay。目录、delete-extraneous、durable sync-complete、安全释放、cloud效果、实际独立worker supervision与生产接线仍需后续交付；不能把RegularFilesComplete等同完整workspace同步或迁移完成。
