# 可恢复的 plain 冷建服务

本单元继续已批准的 etcd master，将准备配额、真实 Kubernetes 创建、bootstrap、证书和发布组成一个具体 Create/Resume 服务。交付必须能从同一持久 intent 接续实际创建，不以更多可注入成功工厂代替接线。main 全量切换、全部业务配置兼容及容量验收仍有独立剩余项。

## 明确的第一条创建路径

单 cell、部署预先绑定的 WorkspaceIdentity、固定 enrolled plain profile、空的独立工作目录、正的已解析 TTL。固定部署镜像及 UID/GID、CPU/内存/workspace/tmp 限额、有限候选 Node tuple；实际 Pod 由原 BuildEnrolledPlainPod 构造。输入不得替换程序、API origin、签名根、clock、provider identity 或任意 Dial callback。

这条路径不装作已实现远端 workspace 初始下载、依赖安装、warm/FUSE、Docker、无 workspace 身份、任意 mounts/PidLimit/whitelist，也不把 Timeout=-1 偷换为一年。现有 AcquireIntent 要求 TTL>0 且≤365天，该 API 的 never-expire 兼容是实际后续缺口。调用者必须明确选择本有限 plain-empty 契约；不支持的输入在任何配额或外部副作用前拒绝。不得启用新 main 入口覆盖旧功能后静默忽略这些要求。

## 组件与持有关系

- 新具体 service 可置于 internal/provisioning：封闭持有一个原生 Backend、固定 Kubernetes create/inspect client、原 production bridge client、原管理 TLS/signing credential、verifier 和受控 clock。生产请求只接收有限 typed input 和原 request/intent IDs，不接受成功 factory。Manager 的适配接点必须明确，但当前 main 仍走旧入口。
- Kubernetes provider 只负责真实有界 POST/GET 及候选诊断；service/Backend 负责持久准入、配额、attempt 与恢复决定。copied Reference、DeclareRuntimeDispatch committed 或 Pod 名称均不是执行/接管许可。provider 可复用纯 static API snapshot 和私有 owned HTTP 机制，不改变已验收 bridge/cleanup 的关闭所有权。
- 版本化不可变创建 capsule 覆盖固定实际 spec、创建时间、TTL 与配置摘要，供 Acquire snapshot/dispatch 绑定；不缓存 live phase、健康或当前准入。公共证书和 bootstrap 恢复材料按 intent 固定，私钥不进入 capsule、launcher public config 或 etcd 业务记录。

## 必须一起实现的持久准备账本

runtime quota、warm slot、inflight preparation 分立；本冷建只占 runtime quota 与全 cell 共用的32个 preparation slot，不占 warm slot。原 publication MaxActive32 和 bridge MaxActive16 都不是这个持久预算。

runtime quota 按固定 quota partition 配置有限 slot ID 集合和16桶 free index；preparation 为固定32 ID 的有界候选。领取 CAS 同时验证并删除 free index、占两个无 Lease 的 slot、写原 intent 的 preparation record。按少量候选 Range/有限 CAS 重试，不能扫全部 slot、所有 sandbox 或申请一个全局 pop。record 保存 originQuotaPartition 与完整 slot refs，workspace partition 可以不同，禁止跨 cell。

库存由显式 operator 初始化接口以有限批次创建，绑定当前 identity/restore 和配置摘要；初始化进度持久、未知结果可只读仲裁，未 ready 不分配。应用构造/首次请求不能自动重建空表或 reset 缺失 slot；不支持在线移动已占用配额。领取和结算均遵守 literal64 事务预算，所有候选/record/索引带版本与完整归属校验。

slot 不因 worker Lease/HTTP 取消/Pod GET404 消失。任何已持久 dispatch、可能迟到的 Create 或 activation unknown 都保留两个 slot，直到有明确准备完成或安全终止证据；禁止错误补偿顺手退款。原 worker 的明确未发送结果也只有在原 owned 请求已物理结束、持久仲裁成立后才可作为安全分支，不能让 copied enum 充当证明。

## 一条真实 Create/Resume 状态链

1. AcquireIntent → 原 CreationClaim → 持久 quota/preparation reservation → DeclareRuntimeDispatch。创建者先记录唯一 POST attempt、固定目标和不可变摘要，复核当前 Backend identity/restore/claim 与 reservation 后才由封闭 service 调实际 provider。claim 失租不使外部调用自动失效，gate 保持关闭且账本保留。
2. provider 对固定 API origin 发一次 owned 有界 Pod POST。无 retry/redirect/proxy/fallback，不把创建提交与调度/镜像拉取混为一次无限调用。区分明确未发送、已观测 exact UID、unknown。unknown 的 Resume 只查原确定目标和完整 profile/spec/intent/generation/node 绑定，不更换 OperationID 或重新 POST；GET404 仍 unknown，迟到创建必须可接续。
3. 取得实际 UID 后，经原 bridge 做真实 Hello；以原 Birth.BootID/public key 构造 runtime identity，不用 NodeBootID 替代。对同 UID 的证书、activation、issuer 材料在发送 activation 前固定并持久化。只保存公共 wire 和摘要；显式部署的稳定私有 signer/credential 必须匹配已有材料，重启不得自动生成替代密钥。
4. activation 丢 ACK/重启后使用原材料先做 pinned mTLS 查询；不能重新 Hello 冒认新 incarnation。仅对原 exact activation 做有界恢复，不因 TLS 拒绝推断 pristine。过期或不可判定材料保留 reservation/恢复 ID 并给 recovery-required，不伪造续期或自动开新 runtime。证书维护与这类长期恢复的运维闭环仍须在生产切换前补齐。
5. BindRuntime → ConsumeRuntimeMount 的明确 plain no-mount → 具体 Destination.ReadEnrolledPublication → 原 Backend.PublishRuntimeEnrolled。元数据 unknown 按各自 exact stage 仲裁，enrolled stage 用专用只读 attempt 接口。只有原认证证明与提交成立后才报告 completed；请求等待结束返回原 request/intent/attempt，而不是丢弃恢复任务。
6. 独立、可恢复的结算 CAS 释放 preparation slot，runtime quota 继续持有。publication 已满64节点，不能附加 quota/due 写、升上限或删原 guard。结算先经原 native loader 验证不可变已发布证据，再比较该不可变 committed witness 与原 slot/record、当前 identity/restore/claim，完成原子 free-index 恢复；这是单调的准备完成结算，不能用它释放 runtime quota。

runtime quota 只能在原精确终止、provider 安全清理和 owner release 已认证成立后另行释放。允许先保守多占、后结算，不能先归还再等待安全证明。无已发布证据的 unknown 出生不接到只适用于已发布 runtime 的释放接口；记录 recovery-required 并保留预算。不能用超时/不存在/Lease 到期制造死亡证明。

## 成功与异常必须可观察

Create/Resume 返回 typed completed、pending、capacity/retryable、recovery-required，带稳定 request/intent/attempt/精确 runtime 归因；只有 original Backend 验证与提交结果可决定 completed。重启以原记录恢复，不在 API goroutine 中永久驻留每 sandbox 控制器。持久 work marker 为后续 partition scheduler 接点；本单元不把显式 Resume 冒称调度器已实现。

service Close 停止新准入，物理中止并等待已有 owned provider/bridge I/O，不把取消当退款。健康、cache、TTL 更新、due/dirty 投影和所有业务文件/stream 仍有后续接线；输出不能声称这些已完成。

## 验证及交付边界

新 selector 先 RED→最小实现→GREEN，真实 HTTP/TLS 验证一次 POST、精确 UID/完整 spec、不重发/未知恢复、上限/取消/Close 实际 join。真实隔离 etcd 验证跨两个 Backend 的持久32预算、失租不退款、free index 原子性与容量不足；不需要启动32个 runtime。原满64事务保持，不重放历史矩阵。

新 native 必须实际 provider POST 到受控 API fixture，再由固定 fixture 启动正式 launcher，原 bridge/observer/clock/管理 TLS 和 Backend 原发布一起完成；另一个受控 ACK 丢失/Resume 场景不得产生第二 POST/runtime。继续原 Cmd.Wait/Serve/IO/attach/containerWait/exact cleanup；不使用 fake kernel、Supervisor 成功 factory 或旧 OWN 产物。隔离 allowance 和每次具体命令由 Root 检查，前置 startup R1验收前仅只读预检。

Go1.25.6、只 apply_patch、独立 epoch actual17/prepost filenames+SHA/归档每成员及 ELF/script 绑定。Root-only exactpaths/HEAD/common→本 OPEN OWN 锁小提交；一个 fresh author/唯一完整 reviewer，同 pair 修复，无 nested。无真实 Redis/Kube/secrets/依赖升级/push/deploy/Sentinel/CLOSED OWN。评估大量已有 sandbox 时，申请查询工作只依赖固定预算与当前操作，不新增按已有 N 的 map、ticker、Lease、扫描或重试循环；少量 fixture 不是容量验收。
