# etcd 到期任务消费者

`internal/storage/state/etcd.Backend.StartDueWorker` 启动常驻的到期任务消费者。它枚举 due 索引、通过 Watch 补齐固定视图之后的变化，并在受信时钟确认到期后创建现有永久 cleanup task。实际事务同时校验当前沙盒元数据、原 worker session、partition、budget share 和精确 due 记录；创建任务与消费该记录原子提交。

本功能是显式调用的 backend API，当前没有接入默认 main、服务启动流程或部署配置。创建 cleanup task 不代表 runtime 已终止、工作区已清理或 owner/quota 已归还；后续仍由既有任务执行协议完成。此处没有新增物理执行器，也没有生产容量或时延承诺。

## 启动条件与份额分配

先按既有流程配置并验证 namespace identity、restore epoch、runtime publication trust、受信 AuthorityClock，以及固定 `partition-worker-v1` worker-control profile。profile 必须由运维预置；消费者不会创建或修复 profile。生产连接沿用 Backend 原有认证连接和 TLS 配置。

due 索引必须覆盖需要处理的 ACTIVE 沙盒。新 publication 写入对应永久 due 记录；没有 due 记录的历史 ACTIVE 元数据不会被此消费者扫描发现。大量历史沙盒需要先完成经批准的索引迁移与一致性核对，不能把启动 worker 当作全量元数据回填。

`DueWorkerOptions` 的约束如下：

| 配置 | 约束与含义 |
| --- | --- |
| `WorkerID` | 有界诊断身份；重复名字不能复用原权限 |
| `Partitions` | 1–256 个不同的逻辑 partition ID |
| `RangeSlots` | 1–20 个不同 ID，每个 ID 小于 20 |
| `ClaimSlots` | 1–100 个不同 ID，每个 ID 小于 100 |
| `RelistSlot` | 一个小于 4 的 ID，在 worker 整个生命周期持有 |
| `StageTTL` | 整秒，1 秒至 24 小时；零值采用 30 秒 |

只尝试配置的固定份额，冲突时不扫描其他 ID。启动失败会等待已执行的初始化和原租约清理返回。所有配置切片都会复制。每个 partition 的固定视图 cursor 始终绑定其原 Range share；partition 按配置顺序轮转分配给 `RangeSlots`。真正准入之前从已获得 credit 的原 Claim shares 中轮转选择。

一个常驻 worker 在整个生命期占有一个 Relist slot，因此同一 cell 的这个消费者最多同时运行 **4 个**；其他使用这些固定份额的消费者也会参与竞争。不能把底层 session 表的 32 个入口解释成可启动 32 个常驻 due workers。分配多份额可以让少量原 session 使用多个调度份额，无需为每份 Claim share 创建新 session。

例如四个 worker 可各分配一个 Relist ID、五个 Range ID、二十五个 Claim ID，逻辑 partition 按 ID 对 4 取模分配。它们必须共同覆盖且不重复占有计划中的 partition：

| worker | partition 条件 | Range IDs | Claim IDs | Relist ID |
| --- | --- | --- | --- | --- |
| 0 | `p % 4 == 0` | 0–4 | 0–24 | 0 |
| 1 | `p % 4 == 1` | 5–9 | 25–49 | 1 |
| 2 | `p % 4 == 2` | 10–14 | 50–74 | 2 |
| 3 | `p % 4 == 3` | 15–19 | 75–99 | 3 |

这是固定份额映射示例，不是部署方案或吞吐测量。Claim share 从空桶开始获得 credit，单份容量为 1、速率为每秒 1 次；Range 页在 RPC 前消耗原份额的整个 256 KiB chunk。失败或不确定结果不会退款。实际处理速度还受元数据竞争、时钟、网络、Watch 追赶与原会话期限影响。

调用示例（由集成方提供已经初始化的 `backend` 与生命周期 `ctx`）：

```go
worker, err := backend.StartDueWorker(ctx, etcd.DueWorkerOptions{
    WorkerID:   "due-worker-0",
    Partitions: []uint8{0x00, 0x04}, // 示例子集；实际应配置计划中的完整分片集合
    RangeSlots: []uint16{0, 1, 2, 3, 4},
    ClaimSlots: []uint16{0, 1, 2, 3, 4}, // 示例子集；需要的其他份额必须显式配置
    RelistSlot: 0,
    StageTTL:   30 * time.Second,
})
if err != nil {
    return err
}
defer worker.Stop()
```

`ctx` 管理整个常驻生命周期，不应误用一次 RPC 的短 context。Backend 的请求超时仍约束每次实际请求。配置的 session TTL 必须严格大于 `2 * RequestTimeout + 1s`；例如 3 秒请求超时配 15 秒 session TTL。消费者约在原 TTL 的三分之一处续租，保留同一 Lease ID 和 session incarnation。提前续租不能延长已经失效的 authority，也不能免除慢调用的实际 join。

## 进度与积压

`Status()` 返回有界副本，不携带 cursor 或可采用的权限。`Running` 表示原消费者生命周期，`Synchronized` 表示当前配置分片的实际列表覆盖和 Watch 追赶已通过 guard 发布。它不表示 cleanup task 已执行。

- `Revision` 是当前固定列表视图的 R；永久 checkpoint 的 replay 基线保持 R+1。
- `CoveredRevision` 是实际完整事件或有效 progress certificate 证明的覆盖。
- `ObservedRevision` 可以包含停止 Watch 之后 post-guard 观察到的更晚尾部。该尾部由下一批继续重放，不能把它等同于已覆盖 revision。
- `Frontier` 是已安全结算的列表前缀。第一个未来候选会冻结它；之后继续枚举未来尾部。Watch 准入即使发生在 frontier 之前，也不会移动这个列表 frontier。
- `Queued`、`OldestExpiry`、`OwnedBytes`、`Invalidation` 和 `LastError` 用于观察候选积压、缓存占用和重建原因。到期延迟还须结合受信时钟与现有任务执行状态评估。

消费者先流式准入固定 R 中真正到期的前缀，再缓存未来候选。因此超过 10000 条 overdue discovery 记录并不要求先把全量集合放入缓存后才创建首个任务。缓存最多保留 10000 个精确 key，同一 worker 全局最多一份源页，包含缓存、固定结构、解码与临时页的预算不超过 64 MiB。缓存路径只在已同步后调度。

Watch 使用原认证连接的有限原生 stream，不使用无界 WatchChan 缓冲。每批先 guard、再订阅、实际停止并 join stream、最后 post-guard。单帧最多 118 个事件及 256 KiB；整批在 protobuf 解码之前限制为 512 帧、10000 个事件及 8 MiB 编码输入。缺少有效 progress 不等于已追赶；Created ACK 本身不提供覆盖证明。空闲批次和本地 credit 不足是正常等待，不会建立新 session。

当前采用有界轮询：单次 stream 最多 500ms，包含 guard 的原操作受 `min(3s, SessionTTL/3, 原 session 剩余期限)` 约束，批次完成后可取消地等待 100ms；每轮先检查原 session 续租。尚未实现按最早 expiry 精确唤醒或按下一续约点进一步截断 stream。未来候选会在后续轮次重新观察受信时间；调度延迟和空闲 stream 开销需要实际测量，这些时限不构成到期处理 SLA。轮询数量受每 cell 最多四个常驻 worker 限制，不随沙盒数量逐个创建 timer。

compaction、事件间隙或 discovery 缓存溢出会使同步状态失效并进行有界重建；不能因此调高缓存、静默丢候选或把过期覆盖标成已同步。旧永久 checkpoint CAS 句柄保留到 fresh `ReadDuePage(nil)` 得到新 R/window 并成功 CAS 替换之后。已完成扫描的窗口最多每 30 秒开始一次新窗口；未完成的健康扫描不会被不断重启。受信窗口倒退时保留原记录并报告等待，不能缩小旧窗口来跳过未来候选。

## 不确定结果、重启和停止

在任何 Stage Grant 之前，消费者用当前原 owner 持久化 exact candidate、Stage reference 与 Task reference。pending 写入本身不确定时禁止 Grant。Grant 或 commit 的未知结果保留同一永久请求，停止原消费者；不能假定失败、生成替代 attempt、退款或删除 pending 来恢复进度。

下一次显式 `StartDueWorker` 获得新的原 session/partition/share 后，先解析 checkpoint 的同一个 pending reference，再恢复调度。已提交任务不会重复创建；只有已知终止的原请求才允许后续独立准入。`unknown`/`invalid`、原 fence 丢失或 session 期限失效都需要查看具体错误；消费者不会自动重建权限并把旧 cursor 交给替代 session。

checkpoint 位于对应 partition 的永久 `due-worker-checkpoint` key。它是发现与恢复证据，不是可采用的 authority。保持 namespace、restore epoch、identity 与 profile digest 一致；不要手工编辑 frontier、replay revision、pending 或 writer 字段。etcd restore 必须走既有 restore-epoch/identity 协调流程。不要把旧 epoch 的 checkpoint 复制到新 epoch 继续执行；身份不匹配应由已批准的迁移流程处理。

`Stop()`、重复 Stop、原 partition/share/session release 和 `Backend.Close()` 都等待实际工作与 once-only 清理返回。取消 context 不代表 native RPC、clock callback 或 stream 已返回。内部先关闭 workDone，再完成原 session 清理，最后关闭 `Done()`；子释放先发布自己的完成信号，再等待 worker 最终完成，避免互相等待。集成方需要在关闭客户端、退出进程或交接配置前实际 join，而不能用超时后的“已取消”日志代替清理完成。

## 原生验收边界

单一 selector 为 `^TestDueWorkerNative$`，仅当 `SANDBOX_DUE_WORKER_NATIVE=1` 时运行。测试复用已跟踪的三成员 etcd 3.6.14 启动器；每次使用新 namespace、数据目录和进程。链生命周期 75 秒，Go 测试超时 90 秒，单次 RPC/独立元数据操作不超过 3 秒，常驻 Start 的生命周期继承链 context。

验收范围包括真实 publication→常驻准入→永久任务、119 条 inert discovery 记录导致的分页、低于 frontier 的真实 Watch late insert、真实 etcd commit 后客户端丢 ACK、原 pending reference 的新 session 恢复、原 leased fence 丢失，以及所有原 actor/stream/租约和三个 etcd 进程的实际 join。测试证书不构成真实 runtime/kernel 证明；测试不执行 sandbox 工作区或物理 cleanup，也不推导生产容量。

正式 collector 维持既有 Linux arm64、只读根文件系统、network none、drop ALL capabilities、no-new-privileges、1 GiB 内存、2 CPU、256 PID、768 MiB `/tmp`，以及 runner/etcd 两个只读挂载。测试未加入默认入口；执行、镜像/工具校验、挂载和清理证据由独立 collector 保留。
