# 冷建 runtime quota 的安全归还

本单元落实已批准 etcd 迁移中的持久配额生命周期。前置完整冷建 Create/Resume 单元须先验收；本文件在其复核期间准备，不授权提前开始第二个实现任务。用户已要求连续推进、小步提交和定期汇报，沿用现有设计与执行方式。

## 行为与信任边界

冷建成功结算 preparation 后，runtime quota 仍被原 runtime 占用。新增 `Backend.SettleColdRuntimeQuota(ctx, workspace, intentID, taskLocator)`：taskLocator 只定位原清理任务，不能充当退款能力。Backend 必须重新认证原 publication、preparation settlement、原精确 runtime 的安全终止、provider cleanup 和 workspace owner release；全部匹配后，以独立 fresh Stage 原子归还原 quota slot。公开 receipt DTO、HTTP 404、取消、失租、超时、裸删除 runtime index 或“未发送”诊断均不构成退款依据。

复用原私有 `readLocalOwnerRelease` 和 `readLocalRuntimeHistory` 的完整签名与留存记录校验。release receipt 必须不可变，绑定 TaskBirth、原 claim、原 runtime/UID/BootID、workspace/generation、Node tuple、cleanup digest 和三项已删除键的原 stamps；其归属必须与冷建 publication、reservation 一致。不是重新查询当前 workspace owner，也不触碰随后创建的新 owner。

原 `ReleaseTaskWorkspaceOwnerLocal` 保持60 compares +4 writes =64节点，不追加 quota 写、不削减安全屏障。prep 已结算是归还 quota 的前提；不得在 quota 先释放后让 preparation 永久无法结算。未知或未发布出生继续保留预算，其专用回收属于后续单元。

## 独立记录与提交

增加 namespace/restore/profile 绑定的不可变 quota-return 记录，包含原 reservation 的完整 quota slot ref/epoch/owner、原 publication 及 settlement 归因、原安全 release/task 归因和本次 Stage locator。不可改写已结算 preparation record/work/receipt，因为 preparation slot 已可能复用。

首次结算仅归还仍由原 owner 和原 epoch 占用的 quota slot，写回其固定 free index，写 quota-return 记录；同一 fresh Stage 写原 committed receipt。fresh Stage 必须在第一次 Grant 前保留 exact StageReference，guard 或 commit 丢 ACK 后仅用原协议只读仲裁。恢复/重放重新认证 return record 及原 receipt；即使原 quota slot 已被新 sandbox 占用，也不能读取新 owner 作为旧退款依据或再次写 slot。

只读预检后固定完整预算为42节点：协议8 compares +领域26 compares +4成功写 +4失败点读。领域26为 ready3、publication journal/receipt6、reservation3、work marker3、prep settlement receipt3、安全 release receipt3、原 quota slot3、free index absent1、return record absent1。原候选39漏掉仍可变的work marker三项pin；保守保持已读证据在提交时一致性，因此增加Mod/Value/Lease比较。实现须基于实际protobuf锁定34 compares/4成功写/4失败点读，仍不得超过64或削弱原屏障。所有记录限制、canonical wire、revision/Lease 检查遵循现有规则。

输出 typed Outcome/Reference/Entry，Entry 是已认证元数据结果，不提供外部执行或死亡能力。缺失证据保守失败/保留预算，恢复不创建新 runtime，不重发 provider cleanup。

## 规模与所有权

仅固定点读、固定候选与常量事务，不新增按已有 sandbox 数 N 的 map、ticker、Lease 或扫描，不因旧记录多而枚举全部 slot。保留历史的容量与 GC 仍需后续测量；本单元不宣称大 N 或吞吐验收。

全部调用纳入 Backend 原关闭屏障，Close 停止准入并 join 活动工作；独立结算不延长外部 runtime 生命周期。Go1.25.6、apply_patch 编辑、Root-only 精确小提交、唯一 fresh author/reviewer、原 Cmd.Wait/Serve/IO/container join 与 exact cleanup、每次 native Root 审核等现有约束继续适用。

## 必须验证

1. 没有 release、伪造/异 workspace/异 intent/异 Node/restore locator、篡改原历史或 receipt 都不能退款。
2. 原 release 成功但 ACK 丢失可冷 Backend 读回认证；quota guard/commit 丢 ACK 保留 exact ref，恢复最多归还一次。
3. 原 workspace 已有新 owner、prep 已复用、quota 随后复用时，旧结算重放不改新 owner/epoch/索引。
4. 使用原真实进程/observer/cgroup 终止、原 provider 清理、原 owner release 得到正例，再结算 quota。仅 metadata fault 或自签测试 DTO 不冒充真实死亡。复用 tracked fixture 源码但不读取旧 OWN 产物；新 selector 不重跑旧整个矩阵。
5. 锁定实际42节点、固定读取/输入大小，所有原进程/transport/IO 加入退出，精确容器清理。

## 交付边界

这是冷建配额安全归还的独立 Backend 接点与真实验证。full Manager/main、warm/FUSE/Docker、业务认证、调度/due、证书维护、未发布出生回收、生产部署恢复、容量测量及 Redis 移除仍按总计划推进。该单元验收后连续接续，不作为整个迁移完成。
