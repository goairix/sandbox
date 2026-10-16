# 有界快照读取与业务读取接点

落实已批准总方案的 coherent metadata view、64MiB weighted LRU 和有界只读 miss 合并。前置 quota-return 单元已验收 `36a5dd1`；本单元接续实施。沿用用户连续实施、小步提交与既有执行方式，不重复设计审批。

## 交付行为

在同一 Backend 增加 `ReadWorkspaceSnapshot(ctx, WorkspaceIdentity)`、`ReadSandboxSnapshot(ctx, sandboxID)`，返回包含原 `*SandboxReadView` 和调用者独占 `*SandboxSnapshotRecord` 的 metadata 结果；未发现 owner/placement 返回 nil。先用原 coherent view 读取 owner/fence/placement/control，再取得该 view 指定的 immutable snapshot，最后用原 `ConfirmSandboxView` 确认同一 view。任一步错误不得返回可用结果，不自动追随替换 owner 或新版本。

这些方法只返回元数据。active phase、缓存命中或最终 metadata 确认均不代表 runtime 健康、业务 TTL 有效或 operation 准入；后续业务复用仍须受控时钟、fresh target health 和独立 operation gate。保留原 `LoadSnapshot` 的历史读取行为；不把当前 owner 缺失误判为历史创建未完成。

冷建 service 的进行中恢复路径使用新 workspace snapshot 读取，取代分离的 LoadControl/LoadSnapshot，继续严格匹配原 claim/intent/generation 和 deployment capsule。完成后的历史读取仍走原路径，因为 Destroy 后的历史完成必须能查询；不得为缓存复活 owner 或刷新 TTL。main/Manager 的统一切换留待完整业务功能齐备后执行。

## 缓存和并发上界

每 Backend 只有一个私有缓存，不按 workspace、runtime 或 deployment 构造缓存。Options 增加 `SnapshotCacheBytes uint64`：0 表示默认64MiB，显式值须在256KiB至64MiB；worker 后续可选择32MiB。按 sandboxID/version/digest/partition 标识不可变快照，namespace/cluster/restore 由固定 Backend 绑定，跨 Backend 不共享。

使用 weighted LRU：权重包含保留的 payload/标识字节及保守固定 entry 开销；总权重不超过预算，tiny entries 同样计费。每次输出和输入均复制 backing，调用者修改不能污染缓存；错误、nil、未通过 schema/digest/placement 检查的结果不入缓存。无每条 TTL、ticker、Lease 或全量扫描。

全 Backend 同时最多16个不同 key 的 miss worker、256个新读取调用（含等待者）。同 key miss 共用一次只读取数，单个调用取消不取消其他等待者；填充有 Backend request timeout，不能由首个调用者无限续命。达到上限立即返回独立 `ErrSnapshotReadCapacity`，不新建队列或退避 goroutine。该错误供后续 API 映射429/503，不能冒充状态冲突或 not-found。

缓存 miss 用固定两点 profile（snapshot65536B、placement4096B）读取原记录，复用原 coherent view 的严格 flat response/unknown fields/header 校验，在 protobuf Size 遍历前拒绝异常嵌套形状；验证 canonical记录、完整 snapshot digest 和引用关系，不增加前缀 Range/Watch/写事务。只提取原 readViewPoints 已预检后的共享 dispatch/response body，旧 view 的4096B/最多4点/4pins预检契约保持不变；不修改 readEnrolledPoints 的通用协议。命中仍执行原权威 metadata discovery及最终确认，不从缓存授予权限；失效 restore/identity、并发 owner/control变化和 Backend关闭必须拒绝成功返回。

## 关闭与资源所有权

缓存和新读取调用纳入 Backend 原关闭屏障：关闭先封准入并取消活跃读取及 miss，再 join 全部调用/worker，清空缓存，最后完成原 client/其他 owner 退出。构造 Backend 不创建常驻 goroutine；只随当前有限并发创建工作。不得复用 publication 的 MaxActive 配额假装独立读取预算，也不添加随已有 N 增长的活动 registry。

## 验证与范围

限定 host/race 测试验证 LRU实际权重、tiny/large eviction、独占返回字节、16/256上限、同 key 合并、单 waiter取消和 Close join。新的真实 TLS etcd selector 验证 miss后命中不再取 snapshot、每次 metadata确认、digest/placement篡改拒绝、填充期间control替换/restore变化、两 Backend隔离及关闭中调用退出；使用原实际上游响应，不能以缓存或假 record 当 authority。新 scoped service native 证明恢复路径实际使用该接点，原历史完成语义不变；不重跑旧整个矩阵。

来源/collector/原 Wait、Serve、IO和精确清理沿用总迁移约束。只声称固定场景的 RPC/bytes减少，不称整体延迟、吞吐或10k/100k/1m容量验收。业务 snapshot codec、完整 API/Manager参数/认证、健康、永久TTL、调度、FUSE/collector、部署和Redis移除仍后续完成。
