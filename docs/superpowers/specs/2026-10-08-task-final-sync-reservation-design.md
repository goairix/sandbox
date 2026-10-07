# Task final-sync producer reservation

日期：2026-10-08；父方案为 2026-10-06-etcd-state-management-design.md，前置单元为已验收的 task-finalization-preflight。此规格细化已批准的永久 effect intent 要求，沿用当前功能分支和小步提交；不改变对象布局。

## 本单元交付

在真实最终同步开始之前，为一个 cleanup task 永久登记唯一 producer attempt。第一版每 task 一个不可覆盖的 `final-sync-attempt`，新 claimant 不得绕过旧未知效果并登记另一 producer。记录计划 mode/digest、原 task/current claim、destroy birth、历史 quiescence 的精确引用/revision 和 terminal receipt digest。mode 只能为 sync、fuse、none；none 是计划声明，绝不是没有远端效果的证明。

新增 `Backend.ReserveTaskFinalSync(ctx context.Context, c *TaskClaim, prepared *PreparedTaskQuiescence, destination *controltransport.Destination, in TaskFinalSyncPlan) (TaskFinalSyncReservationResult, error)`；plan 包含 Mode 和 Digest。result 包含诊断 Reference、Outcome，不返回执行能力、票据或成功结算。`Backend.LoadTaskFinalSyncAttempt(ctx context.Context, task TaskReference) (*TaskFinalSyncAttemptEntry, error)` 只加载当前 authority 内的持久归属，不能重建 sender。

Reference 包含 Task、AttemptID；Record 包含 Version、Task、Claim、DestroyingRevision、Quiescence、QuiescenceRevision、ReceiptDigest、Plan、AttemptID。Entry 包含 Reference、Record、Revision。生成 UUID 一次，保存于原 TaskClaim 的私有 draft；same original claim/plan/prepared 的重试只沿用原 UUID/wire。copied/foreign claim/prepared、changed plan、expired saved deadline 在任何写之前拒绝。固定期限不随 claim 续租延长。

## 流程和未知结果

1. 校验参数、原对象身份和严格 plan。调用已验收 RevalidateTaskQuiescence，实际重新查询两次 runtime 和一个 operations prefix；不复用 emptyRevision。
2. 在 c.mu 下检查 original live/deadline 和 immutable pins。创建或使用原私有 draft，保存固定 wire；先以当前 base/claim/pin fences 原生读取固定 reservation key。任何不同 record、不同原 claim 或相同 bytes 的新 revision 都拒绝；新 claimant 只可诊断旧记录。
3. 在发送 Txn 之前将 draft 的 writeAttempted 置位。原生 Txn 比较完整 taskEmptyComparisons 的53项以及 reservation CreateRevision==0；成功分支只 Put 一个永久记录，失败分支读 identity/restore/reservation。合计58 literal nodes，预算不得超过64，双方 protobuf 请求及保守响应必须经过 taskCheckWire。无新 Grant、KeepAlive、Revoke。
4. 写响应丢失时不重新发送该写，不生成新 ID。后续相同原 draft 可做只读仲裁：严格 immutable KV、固定 wire、native birth>claim birth、exact original claim/task/context 链一致才能诊断 committed；absent 永远是 unknown，不写 aborted，不推断 no-send。明确提交但原 claim/期限已失效可报告 committed 诊断，不能取得权限。
5. 已确认的原 native revision 被 pin；operator delete/re-put 即使 bytes 相同也拒绝。记录为 Lease0、create==mod、Version1、最大8192bytes，严格拒绝未知/重复字段、noncanonical 数据、错误 key/birth/context。历史 load 校验 structural linkage，不据 local draft/missing record猜测 effects。

这是不可撤销的 producer reservation，类似前序 quiescence attempt reservation，不是危险业务 Stage，也不写业务 control/owner 或伪造 committed Stage receipt。Outcome 只归于此单笔原生 metadata reservation。临时 RPC 失败可以使同一进程需要只读仲裁；进程重启后只剩诊断，后续恢复必须依具体远端证据处理，不自动创建新 producer。

## 必须验证

真实三成员 native fixture 上：成功持久登记、重复相同原调用不再 Put、新 claimant/不同 plan拒绝；53 fences + absence及58 nodes实际断言；每个成功的第一次调用实际执行新 dual-drain观察；写前receipt/reservation/control/restore/claim变化拒绝；commit真实落盘但response丢失后same draft只读确认；未发送/未提交却丢失响应后保持unknown且禁止再次写；same bytes重建native reservation拒绝；原claim撤销、saved deadline到期、caller取消后不生成新authority。无效果/签名/租约额外调用。host codec覆盖错误字段、key、birth、lease、wire和Mode。

## 不属于本单元的保证

没有可信 runtime 只读文件源、HTTP/provider发送、逐请求ledger、provider结算、FUSE task receipt、精确终止或安全释放。当前记录不是 worker OS身份/Boot存活证明。后续真正 producer必须绑定这些身份，在每一次请求发送前重新验证当前claim/pins并永久记录具体请求；remote unknown保持owner/pending。已有大量 sandbox 的容量、生产接线、分区调度、部署恢复、Redis移除仍待父方案阶段完成。本单元不重放旧物理gate。
