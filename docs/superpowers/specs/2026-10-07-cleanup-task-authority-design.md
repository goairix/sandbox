# 清理任务的永久意图与原生租约能力

这是已经批准的 `2026-10-06-etcd-state-management-design.md` 中销毁与 TaskRepository 的实施分解。继续在 `codex/etcd-state-management` 开发，小块验证、评审后及时提交；不推送、合并或部署。重点是已有沙盒数 N：只有实际领取的任务拥有 Lease，不为普通空闲沙盒新增客户端、goroutine、timer、ticker 或 Watch。

## 本单元结果

实现三个相连的元数据边界：严格的永久 cleanup task/intent 读取；与 BeginOperation 使用相同 control revision 的原子销毁准入关闭；原任务 Lease 下不可重建的 claim/guard、续租、释放及保守 checkpoint。这个结果允许后续 runtime 屏障协议领取合法任务，当前不调用 runtime，不释放 owner，不删除 control/index，也不声称已完成双侧排空或远端写入结算。

已有认证执行单元已完成真实 PID1、执行、续期及关闭验收。其普通 Close/本地 receipt 不是本单元的 TaskClaim，也不是 remote End。本单元只提供元数据管理权；后续 task-authenticated CloseData/CloseAll、目标持久回执、操作 prefix 排空、final sync/flush、精确终止、remote settled/fenced 与最终安全释放均仍为强制工作。

## 选择与边界

采用独立 TaskClaim，而不复用创建专用 CreationClaim：创建 bundle 固定要求 publishing/pending，不能授权 destroying 工作。永久任务与 claim 分开，不把 control/owner 附 Lease，也不使用共享 worker session Lease 掩盖单个任务失活。三个元数据阶段沿用现有 Stage 的固定 attempt、guard 与 commit/abort 仲裁，避免事务超时后读 absent 就重建或补偿。

本单元先支持已发布 active sandbox 的手动 destroy；publishing 失败恢复、exclusive/cleanup_pending 恢复、TTL due 触发及 due/dirty index 将在对应生命周期/调度单元接入。不能把本单元 API 用来提前接管仍有未知外部效果的任务。新的 claim 可接续永久意图的元数据管理，但不会重发外部命令、重开 gate 或获得旧 claim 的运行能力。

## 永久记录

所有 key 均在当前 Namespace/cell，partition 来自 workspace hash 首字节。使用：

- `p/<partition>/tasks/<canonical-task-UUID>`：不可改写的 TaskRecord。
- `p/<partition>/cleanup-intents/<canonical-task-UUID>`：不可改写的 CleanupIntentRecord。
- `p/<partition>/sandboxes/<sandboxID>/cleanup-task`：不可改写的 TaskLinkRecord，阻止一个 runtime 并行创建两个 cleanup 意图。
- `p/<partition>/tasks/<taskID>/claim`：当前 task Lease 下的固定 claim。
- `p/<partition>/tasks/<taskID>/guards/<claimID>`：同一 Lease 下不可重建的固定 guard。
- `p/<partition>/tasks/<taskID>/checkpoint`：永久元数据 checkpoint，不能承载实际输出或自行证明物理阶段。

TaskReference 包含 Namespace、RestoreEpoch、TaskID、Partition、SandboxID，仅供定位/诊断。TaskRecord 固定 version1、kind=`cleanup`，绑定 WorkspaceHash、CreationIntentID、Generation、DataGateEpoch、完整 RuntimeReference、SnapshotReference、ExpiresAt 与销毁前 ControlRevision。CleanupIntentRecord 绑定同一 TaskReference 和同一 tuple；TaskLinkRecord 绑定完整 TaskReference。所有记录必须严格 UTF-8/JSON，拒绝未知、重复、遗漏字段、null、尾随 JSON、错误租约/身份/版本与大小超限。TaskRecord/intent 各4096字节，link2048字节；这三个不可改写的永久记录 Lease0、CreateRevision>0、ModRevision==CreateRevision。checkpoint 同为永久 Lease0，但可由合法 CAS 更新，因此 CreateRevision>0、ModRevision>=CreateRevision，硬上限4096字节。decode 失败不改写调用者目标。LoadTask 只返回拥有的副本，不返回 live capability。

## 原子销毁关闭

`PrepareDestroy(ctx, BeginDestroyInput{SandboxID, RequestID, ExpectedControlRevision}, ttl) (*Stage, TaskReference, error)` 读取原五点 placement/control/owner/fence/runtime-index 权威链，要求 active、精确当前已绑定 runtime。ExpectedControlRevision>0 且匹配读取的 control，调用者不能按时间猜测销毁新版 control。生成一次新的 task UUID，通过 `beginStageWithBuilder` 写入 StageAttemptLocator，以便诊断未知提交。

同一 Stage 事务比较原五点各 value/ModRevision/Lease0、必要 immutable CreateRevision、当前 restore identity、原 stage guard、receipt 不存在，且 task/intent/link/checkpoint 都不存在。一次写入 control.phase=destroying、固定 task、固定 cleanup intent、固定 link、初始 pending checkpoint 与 Stage committed receipt。保留 runtime、gate epoch、expires、snapshot 和 owner/fence/index，不能删除任何 operation token。

prepared Stage 不是已关闭证据；CommitStage 只有 committed 才确认该元数据事务。丢响应按原 StageReference 仲裁；absent 不得作为 aborted。BeginOperation 与 destroy 比较同一 control revision，因此事务排序决定先后：先入场者留下原 token，先销毁者使旧入场失败；后续新 BeginOperation 必须拒绝。destroy committed 只关闭 etcd 准入，不能证明目标已关闭。

## Claim 与 checkpoint

`ClaimTask(ctx, ref, workerID, ttl) (*TaskClaim, error)` 先线性读取任务/intent/link/checkpoint 与原五点链；要求永久 task、intent、link 共同创建，初始 checkpoint 与它们共同创建，control 的 destroying revision 为该创建 revision，元组与 owner/fence/index 一致。存在既有 claim 时拒绝，不采用现成 Lease。TaskClaim 为 origin/self sealed，保存原上下文、原 Lease、原 claim/guard 精确 value、CreateRevision 与所有永久 fences；公开 Reference 只复制诊断。claim ID 为内部新的 canonical UUID，claim/guard 同一成功事务创建、附同一原 Lease、不可改写。

ttl 仅接受(0,24h]，向上取整秒；校验 grant/header/TTL/Lease/cluster/成功响应结构。以发送 Grant/KeepAliveOnce 前单调时间加服务端 TTL 计算保守本地截止；结果未知、过期、上下文取消、fence 丢失后 local capability 永久 lost。RenewTaskClaim 先验证所有原 fences 和原 claim/guard value、LeaseValue、CreateRevision、ModRevision，再 KeepAliveOnce 原 Lease；不 Grant 替代 Lease。ReleaseTaskClaim 标 lost 并仅 Revoke 原 Lease，保留任务/意图/owner/control/checkpoint；Revoke 未知显式报告。

checkpoint 只支持 `pending` 与 `needs_reconciliation`。记录 version1、TaskReference、最后归因的 ClaimID（初始空）、DetailDigest（初始空，后续必须64位小写SHA256）、StageAttemptLocator。它记录元数据工作进度或不确定性，不记录 closed/quiesced/synced/terminated/committed 等物理成功状态。后续物理协议必须另行定义不可伪造 evidence，不能把这里的字符串提升为安全释放证明。

`PrepareTaskCheckpoint(ctx, claim, TaskCheckpointInput{StageID, ExpectedRevision, State, DetailDigest}, ttl) (*Stage, error)` 只写这个任务的固定 checkpoint；ExpectedRevision 必须>0并精确 CAS，重读最新 checkpoint 后允许合法新 attempt。始终比较原永久 fences，以及 claim/guard 的固定 value、LeaseValue、CreateRevision、ModRevision。新的 Stage 有独立 attempt guard/receipt，但原 task guard 必须仍有效。lost/copy/foreign/zero capability 与原 context 取消先拒绝；排队中的原 Stage 仍必须由服务端原 task guard 拒绝，取消本身不是外部撤回证明。未知 checkpoint 提交以原 Stage 仲裁，不据读取 absent 重发。

TaskClaim 固定 fences 仅包含 task/intent/link 及 placement/control/owner/fence/runtime-index，不把可变 checkpoint 的初始 ModRevision 固定在 capability 内。每次 checkpoint 单独读取、验证创建 revision 仍等于原任务创建 revision、完整 reference/attempt 与有效状态，再 CAS 调用者的 ExpectedRevision；允许同一仍活跃 claim 连续两个合法更新。新 claim 读取历史 checkpoint，只获得新的元数据管理权；历史 ClaimID/状态不授予副作用权限。预计最多32个永久 fence 比较、8个 claim/guard 比较及 checkpoint CAS，须实际用现有64-operation预算验证，超限在任何 Grant 前拒绝。

## 检验与成本

真实三成员 etcd 验证，不用内存模型证明 Lease/Txn：严格记录故障；同 control revision 的 begin/destroy 两个顺序与并发；丢响应/迟到 commit 与 resolver；claim 竞争、Lease revoke/自然到期、同值改写/Lease迁移/同值重建、restore/owner/control/runtime fences；旧 claim 及其已经 prepared 的 checkpoint Stage 不可复活；checkpoint CAS冲突/未知提交、永久意图保留。宿主 race 只描述宿主，metadata fixture 不认证 Linux PID1/FUSE 或生产 mTLS/RBAC。

Lease 数和续租工作随被领取的任务增长，不随 N 增长。永久 task/intent/link/checkpoint 有字节和历史增长成本；不新增按年龄 GC、永久未决删除或假完成。生产共享调度、due/dirty、背压/安全 GC 在后续单元实现。每个有意义、通过覆盖检查并自审的切片即提交，独立 task review 和一次完整单元 review 保留原始范围及每项未认证边界。
