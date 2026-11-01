# Etcd Operation Coordination Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [x]`) syntax for tracking.

**Goal:** 保留 etcd 的权威协调语义，减少普通 Data 请求的重复写入、Lease 创建和排空轮询。

**Architecture:** API 实例按沙盒身份共享 leased Data operation，进程内维护请求引用；每请求用一次只读事务同时检查 record 与 operation。原生 KeepAlive 维护共享 Lease，单实例共享 Watch 分发变化通知，排空最终仍由权威读取及既有 fencing 确认。Mutation/Exclusive 保持独立 operation。

**Tech Stack:** 当前 Go 1.25.0、etcd client/v3 v3.6.14、现有 state 可选能力接口、Manager gate、Viper、Helm、OpenTelemetry；不新增产品依赖。

**Spec:** `docs/superpowers/specs/2026-11-01-etcd-operation-coordination-design.md`（用户已确认）。

## Global Constraints

- “Lease TTL 30s、每 10s 维护一次；引用数归零后空闲保留 15s。”
- “registry 默认最多 1024 个组，含 creating/draining 的占位；超限请求走现有独立 operation 路径。”
- “只能淘汰引用为零的组，不能为控制内存删除在途保护。”
- “每秒至多一次带抖动的权威复核，取代 10/25ms 轮询。”事件唤醒的读单独统计。
- “新增一个默认关闭、可部署配置的 `sharedDataOperations` 开关，贯穿配置与 Helm。”
- 服务端 leased key 为权威，持久 ExpiresAt 只作提示；Generation、RuntimeUID、token 和现有事务 fencing 必须保留。
- 文件流保持远端 Close/join 后释放；不更改运行时协议、FUSE、镜像 tag、workspace owner 或后台生命周期扫描协议。
- 断网、Lease 过期、context 取消都不构成远端停止证明。无法证实隔离的用例阻止开启新路径，须明确报告。
- 不支持可选能力的后端继续原路径，不增加 Redis 数据迁移或缓存授权。
- 只操作独立测试 namespace/prefix；本计划不包含生产 rollout 或业务数据清理。

## Review Focus

1. 同一 Pod 的容器重启仍沿用 POD_UID：每个 Manager 的组使用新启动 nonce 和随机 token，不能继承前进程保护（Task 3/6）。
2. 首个 HTTP 请求在建组期间取消：不能终止其他等待者共用的注册或维护（Task 3/4）。
3. 非法/延迟 KeepAlive 响应、读取与续期之间 Lease 消失：不能延长不确定期限或补建旧组（Task 1/3）。
4. Watch 被同前缀 controller 更新淹没、历史压缩或缓冲溢出：有界内存和读回重建，不能错误判定排空（Task 2/5）。
5. 无人显式 Close 的文件流、长时间 helper join、停止期间还有后台健康检查：必须拒绝新准入并正确保留或失效在途保护，不能提前报排空（Task 4/6）。

## File Structure and Sequence

| Task | 新建/修改文件 | 职责 |
| --- | --- | --- |
| 1 | 新建 `internal/storage/state/active_data_operation.go`、`internal/storage/state/etcd/active_data_operation.go` 及同名 `_test.go` | 共享 Data 的可选接口、只读快照与 KeepAlive |
| 2 | 新建 `internal/storage/state/active_changes.go`、`internal/storage/state/etcd/active_changes.go`、`internal/sandbox/active_change_hub.go` 及各自测试 | 单前缀 Watch、revision 分发与有界恢复 |
| 3 | 新建 `internal/sandbox/shared_data_operations.go`、`shared_data_operation_group.go`、`shared_data_operations_test.go` | 本地组状态、引用与维护 |
| 4 | 修改 `internal/sandbox/manager.go`、`active_store.go`；新建 `shared_data_manager_test.go`，扩展 `files_v2_test.go` | 接入 Data 路径和 shutdown，保留流 join |
| 5 | 新建 `internal/sandbox/active_operation_wait.go` 及测试；修改 `active_store.go`、`workspace_distributed.go`、配置/Helm/指标文件 | 排空替代轮询、开关与可观察性 |
| 6 | 新建 `internal/sandbox/shared_data_etcd_test.go`、`shared_data_bench_test.go`、`scripts/test-etcd-operation-coordination.sh` 和验收报告 | 真 etcd、多副本/故障、旧路径对照与验收证据 |

Task 1→3→4、Task 2→5，以及全部→6 有依赖，执行时逐任务完成并提交。以下签名为计划锁定的跨任务契约；内部辅助函数由实现者确定。

### Task 1: Shared Data repository capability

**Interfaces**（定义于 `state/active_data_operation.go`，实现于 etcd 同名文件）：

```go
type ActiveDataLease struct {
    Operation ActiveSandboxOperation
    LeaseID int64
    RequestedTTL time.Duration
}
type ActiveDataView struct {
    Record *ActiveSandboxRecord
    Lease ActiveDataLease
}
type ActiveDataOperationRepository interface {
    OpenDataOperation(context.Context, string, string, time.Duration) (*ActiveDataView, error)
    ReadDataOperation(context.Context, ActiveDataLease, bool) (*ActiveDataView, error)
    KeepAliveDataOperation(context.Context, ActiveDataLease) (*ActiveDataView, time.Duration, error)
}
```

`ReadDataOperation` 的 bool 为 requireActive；false 只用于已注册组排空维护，允许 active/exclusive/destroying/cleanup_pending，仍拒绝 publishing、身份改变或 operation 缺失。返回时必须确认确切 LeaseID；不把服务端 LeaseID 本身当授权。`KeepAliveDataOperation` 返回本轮确认的服务端 TTL，期限由调用方从发起前的单调时刻保守计算。关闭使用现有 `EndOperation(ctx, lease.Operation)`。

- [x] **RED tests:** 在 etcd `_test.go` 用现有 `testStore`/`validActiveRecord` 写 `TestSharedDataReadUsesOneSnapshot`（一次只读 Txn，0 KV 写）；`TestSharedDataLeaseKeepAlivePreservesKey`（LeaseID、ModRevision、持久值不变，TTL 得到续期）；`TestSharedDataKeepAliveRejectsMissingLease`（Lease 被撤销后不复活）；`TestSharedDataKeepAliveUnknownReply`（不补 Grant/Revoke）；`TestSharedDataExpiredAdvisoryTimestamp`（服务端 key 仍活时可读）；`TestSharedDataReadRejectsCorruptIdentity`（token/kind/generation/LeaseID 不符拒绝）。断言示例：

```go
require.Equal(t, before.ModRevision, after.ModRevision)
require.Equal(t, before.Lease, after.Lease)
require.Zero(t, countedWrites)
require.ErrorIs(t, err, state.ErrActiveSandboxStaleToken)
```

- [x] **Verify RED:** `TEST_ETCD_ENDPOINT=http://127.0.0.1:2379 go test ./internal/storage/state/etcd -run 'TestSharedData' -count=1 -v`；确认测试因尚未实现的新能力失败。端点必须来自独立测试 etcd，缺失时不得将 SKIP 当通过。
- [x] **Implement:** Open 复用 BeginOperation，再确认服务端绑定；Read 在一次默认线性一致只读 Txn 中读取 record+operation。KeepAlive 先 Read(requireActive=false)，再对原 Lease KeepAliveOnce；没有新业务 Put 或 Lease 替换。错误/TTL<=0/响应 LeaseID 不匹配均失败关闭。旧 Begin/Renew/End 不改语义。
- [x] **Verify GREEN:** 上述测试及 `go test ./internal/storage/state/...`；用真实过期/续期证明持久 ExpiresAt 不妨碍旧 LiveOperations/EndOperation，列出实际消费者检查结果。
- [x] **Commit:** 仅提交本任务接口、实现与测试，`feat(state): add shared data lease capability`。

### Task 2: Bounded change stream and local hub

**Interfaces**（state 数据类型不暴露 clientv3）：

```go
type ActiveChange struct { SandboxID string; RecordChanged, OperationChanged bool }
type ActiveChangeBatch struct { Revision int64; Reset bool; Changes []ActiveChange }
type ActiveOperationObservation struct { Count int64; Revision int64 }
type ActiveChangeRepository interface {
    WatchActiveChanges(context.Context, int64) (<-chan ActiveChangeBatch, error)
    ObserveOperations(context.Context, string) (ActiveOperationObservation, error)
}
```

同 revision 事件作为完整批次交付；Reset 表示不能依赖此前历史，需读回。`WatchActiveChanges` 按 record 前缀订阅，从指定 revision 开始（0 为当前）；识别 record 变化和 operation 删除，不让 controller 子 key 变化唤醒业务等待者。Observe 的 count 和 revision 来自同一次线性一致 count-only Get。

`newActiveChangeHub(ctx context.Context, repo state.ActiveChangeRepository) *activeChangeHub`；`Subscribe(id string, afterRevision int64) (<-chan state.ActiveChangeBatch, func())`；`Stop()`。一个 hub 只持一个 Watch，维护最多 256 个完整 revision 的本地回放，订阅队列容量 1 合并通知；落后/溢出发 Reset，绝不静默跨越缺口。订阅取消只移除本订阅。

- [x] **RED tests:** `TestActiveChangesPreserveRevisionBatches`、`TestActiveChangesResumeAfterDisconnect`、`TestActiveChangesCompactionRequestsReset`；hub 的 `TestActiveChangeHubSubscribeRace`、`TestActiveChangeHubBoundsHistory`、`TestActiveChangeHubCoalescesControllerNoise`。用受控 channel 保证读 R 后注册前有事件，断言收到事件或 Reset；回放超过 256 个 revision 后必须 Reset；100 个本地订阅仍只有 1 个底层 Watch。
- [x] **Verify RED:** `go test ./internal/sandbox -run 'TestActiveChangeHub' -count=1` 及带 TEST_ETCD_ENDPOINT 的 `go test ./internal/storage/state/etcd -run 'TestActiveChanges' -count=1 -v`；确认新能力缺失失败。
- [x] **Implement:** 使用现有 client 建 Watch，按最后完整 revision 重连；压缩/取消错误要求 Reset、重新建立流。错误重连退避从 100ms 到 5s 带抖动，context 取消立即退出；不得把断线重试当无事件证明。hub 只通知本地注册 ID，无全库快照缓存；注册/停止线程安全。
- [x] **Verify GREEN:** 执行上述测试和 `go test -race ./internal/sandbox -run 'TestActiveChangeHub' -count=1`；真实 etcd 验证删除 Lease 所产生事件及独立 namespace 隔离。
- [x] **Commit:** `feat(state): add bounded active coordination notifications`。

### Task 3: Local groups and lease maintenance

**Interfaces:** 消费 Tasks 1/2。定义 `sharedDataOperations`、`sharedDataOperationGroup` 与 `sharedDataOptions`（TTL=30s、renew=10s、idle=15s、maxGroups=1024），内部注入时钟/定时器供测试。构造 `newSharedDataOperations(ctx context.Context, repo state.ActiveSandboxRepository, capability state.ActiveDataOperationRepository, hub *activeChangeHub, instanceID string, options sharedDataOptions) *sharedDataOperations`；方法：

```go
Acquire(ctx context.Context, sandboxID string) (*state.ActiveSandboxRecord, context.Context, func(), error)
CloseAdmission()
Stop(ctx context.Context) error
```

满额返回本包哨兵 `errSharedDataCapacity`，仅该错误允许 Manager 回退旧 operation；后端/租约错误不得伪装成容量不足。创建前按 sandboxID 合并占位，确认后绑定完整 Generation/RuntimeID/UID；进程启动 nonce+随机 token 不复用 cfg.InstanceID 的旧身份。

- [x] **RED tests:** `TestSharedDataGroupSingleRegistration`（64 个并发 Acquire 只建 1 组）；`TestSharedDataGroupFirstCallerCancellation`（第一个取消不终止其他请求）；`TestSharedDataGroupIdleAcquireRace`（15s 边界新引用与淘汰不能同时成功）；`TestSharedDataGroupCapacityFallback`（1024 个占位后返回精确哨兵）；`TestSharedDataGroupPauseExpiresAdmission`（时钟推进至确认期限后不继续授权）；`TestSharedDataGroupKeepAliveResponseLate`（响应延迟不从收包时刻加 TTL）。
- [x] **Verify RED:** `go test ./internal/sandbox -run 'TestSharedDataGroup' -count=1`；确认因缺少 registry 实现失败。
- [x] **Implement:** 每组 gate 先预留引用再读权威快照，每请求只读一次；group 不缓存可变 snapshot。维护独立于请求取消，默认续期约 10s±10% 抖动；本地保护截止时刻为发起时间+确认 TTL−1s，不能推进已进入 lost 的组。任一续期错误关闭准入并取消所有请求，旧组不补 Lease。阶段通知关闭 gate，refs==0 后只 End 一次。idle 退出仅 refs==0；注册失败、drain 和退出均释放占位。
- [x] **Implement context boundaries:** 每请求 context 继承调用方，同时注册 group lost 的取消传播；release 幂等，解除传播并减引用。阶段正常 drain 不取消已有请求；lease lost 取消整组。Stop 拒绝准入，取消并等待调用方/远端 release 的既有边界；超时不能提前删除仍在途记录。
- [x] **Verify GREEN:** 上述测试，增加 `TestSharedDataGroupOldGenerationCannotJoin`、`TestSharedDataGroupRestartUsesNewToken`、`TestSharedDataGroupDrainingRenewsUntilReleased`、`TestSharedDataGroupLostLeaseDoesNotAffectSuccessor`；执行 `go test -race ./internal/sandbox -run 'TestSharedDataGroup' -count=1`。
- [x] **Commit:** `feat(sandbox): share data operation protection within each API instance`。

### Task 4: Manager admission and streaming shutdown

**Interfaces:** ManagerConfig 增加 `SharedDataOperations bool`，Manager 持 registry 和 hub。将现有独立实现改名为 `beginIndividualDistributedOperation`，保持原签名；`beginDistributedOperation(ctx,id,kind)` 为单入口，只有开关开启、Kubernetes、Data kind、两种可选能力均存在时走共享组。其余场景及容量不足均原路径。

**Files:** 修改 `internal/sandbox/manager.go`（配置/初始化/BeginShutdown/Stop）、`active_store.go`（入口与运行时验证）；新建 `shared_data_manager_test.go`，扩展 `files_v2_test.go`。运行时身份验证可提取 `validateActiveRuntime(ctx context.Context, sb *Sandbox) error` 供两条路径复用；不重构其他业务代码。

- [x] **RED tests:** `TestSharedDataManagerReadsFreshSnapshot`（第二请求看到更新的 TTL/网络快照）；`TestSharedDataManagerRejectsChangedRuntimeUID`；`TestSharedDataManagerOnlyCapacityFallsBack`；`TestSharedDataManagerDisabledUsesIndividualOperations`；`TestSharedDataManagerBeginShutdownKeepsInflightLease`；扩展 `TestSharedDataFileStreamReleaseWaitsForJoin`，用阻塞 reader.Close 断言 join 放行前 End 次数为 0，其他请求取消不影响该流。
- [x] **Verify RED:** `go test ./internal/sandbox -run 'TestSharedData(Manager|FileStream)' -count=1`。
- [x] **Implement:** 初始化 group/hub 必须有可终止的 Manager 生命周期且不使用首请求 context。注册后补 Read 验证阶段，所有 Data 调用（包括健康检查）通过单入口。BeginShutdown 关闭 shared admission、保留在途维护；调整 Stop 顺序使它不会先 cancelControl 再要求已被取消的共享维护排空。保持现有 helper stream Close/join wrapper；未完成远端 join 不减少引用。
- [x] **Verify GREEN:** 上述测试；增加 `TestSharedDataManagerStopTimeoutKeepsProtection`（Stop ctx 超时不是排空证明）、`TestSharedDataManagerNoCapabilityKeepsLegacyPath`、`TestSharedDataManagerHealthDuringShutdownRejected`、`TestSharedDataFileStreamCallerAbandons`（调用方未 Close 时 shutdown 不得提前 End，租约失效传播到 helper；不能确认远端结束则保留失败结论）。`go test -race ./internal/sandbox -count=1`；真实 etcd 场景在 Task 6 验证。
- [x] **Commit:** `feat(sandbox): integrate shared data admission and shutdown`。

### Task 5: Drain waits, configuration and bounded metrics

**Interfaces:** `waitDistributedOperations(ctx context.Context, sandboxID string, exclusive *state.ActiveSandboxOperation) error` 放在新 `active_operation_wait.go`。nil exclusive 等待 count==0；非 nil 等待 count==1 后由既有 `fenceWorkspaceOperation` 再次确认自己的 token/Lease/UID。共享通知可用时 Read R→Subscribe R+1→事件/Reset/每秒复核→再 Read；一个 ID 只允许一个在途观察。缺少可选能力时维持原路径。

**Config files:** 修改 `internal/config/config.go`、`configs/config.yaml`、`cmd/sandbox/main.go`；新增 `internal/config/shared_data_operations_test.go`、`cmd/sandbox/shared_data_operations_test.go`。配置键 `storage.state.shared_data_operations`，Viper 默认 false；环境变量 `SANDBOX_STORAGE_STATE_SHARED_DATA_OPERATIONS`；Helm `config.storage.state.sharedDataOperations: false`，修改 `deploy/helm/sandbox/values.yaml`、`templates/deployment.yaml`，新增带 helmtests tag 的 `internal/helmtest/shared_data_operations_test.go`。只影响 API 的 operation 协调，不进入 backend identity/fingerprint。

**Metrics files:** 修改 `internal/storage/state/etcd/store.go`、`active_operation.go`、Tasks 1/2 的新仓库文件、`cmd/sandbox/state_backend.go`、`internal/telemetry/metrics/metrics.go` 及测试。增加 Options `CoordinationStageObserver func(context.Context,string,string,string,time.Duration)`，接线 `metrics.RecordCoordinationStage` 同签名；直方图 `sandbox.coordination.stage.duration` 的 count 统计已完成 RPC。固定 operation 标签 open/read/renew/end/observe/watch，stage 标签 read/grant/commit/keepalive/revoke/event，result 标签 success/error/conflict/reset。registry 计数 `sandbox.coordination.groups`、`sandbox.coordination.fallbacks` 采用固定状态/原因；不得用 ID/token 作标签。一个 Watch 流的建立与流内事件分开计量，事件数量不是 RPC 数。

- [x] **RED tests:** `TestSharedDataWaitNoNotificationRechecksAtMostOncePerSecond`（带抖动的间隔为 1s–1.1s，不低于 1s）；`TestSharedDataWaitEventBetweenReadAndSubscribe`；`TestSharedDataWaitReadErrorDoesNotDrain`；`TestSharedDataWaitCoalescesSameSandbox`；配置默认 false/env true/YAML true；Helm 开关渲染且切换不改变 etcd StatefulSet/PVC/fingerprint；指标拒绝动态 ID 标签。
- [x] **Verify RED:** `go test ./internal/config ./cmd/sandbox ./internal/telemetry/metrics ./internal/sandbox -run 'TestSharedData|TestCoordination' -count=1`；`go test -tags helmtests ./internal/helmtest -run TestSharedData -count=1`。
- [x] **Implement:** 两处热轮询调用替换为上述 helper。清理之前的 durable close-admission、事务、exclusive fence 均保留；关闭全局开关时不得启动 shared hub/registry。配置不支持新能力时记录固定原因并使用旧路径，不影响 Redis/Docker。对实际 RPC 发一次 observer，不把重复唤醒计为 RPC；watch 字节/事件量另统计。
- [x] **Verify GREEN:** 上述测试及 `go test ./internal/config ./cmd/sandbox ./internal/telemetry/metrics`、`bash scripts/test-helm-chart.sh`、`go test -tags helmtests ./internal/helmtest`。验证生产 values 未默认开启。
- [x] **Commit:** `feat(sandbox): replace hot drain polling and expose shared operation rollout controls`。

### Task 6: Real etcd safety and load evidence

**Files:** 新建 `scripts/test-etcd-operation-coordination.sh`、`internal/sandbox/shared_data_etcd_test.go`、`shared_data_bench_test.go`；报告 `docs/superpowers/reports/2026-11-01-etcd-operation-coordination.md`，更新 `docs/deployment/helm-production.md` 的开关和回退步骤。

**Interfaces:** 脚本 `scripts/test-etcd-operation-coordination.sh [safety|bench]` 启动本任务专用三成员 etcd（固定 `quay.io/coreos/etcd:v3.6.14`，回环 client 端口），设置 TEST_ETCD_ENDPOINT、独立前缀，执行相应 Go 测试，trap 只清理自己创建的资源。必须验证实际用例运行而非 SKIP；不依赖仓库中的业务 kubeconfig。现有 Go helper只支持单 endpoint 时，测试连接一个本地成员，其余成员的容错仍在脚本控制中验证。

- [x] **RED safety tests:** `TestSharedDataEtcdMixedReplicaDestroy`、`TestSharedDataEtcdExclusiveWaitsForAllGroups`、`TestSharedDataEtcdOldExpiryStillCountsLiveLease`、`TestSharedDataEtcdCrashAndLeaseLoss`、`TestSharedDataEtcdWatchCompaction`。新旧两条路径并存，断言 durable 关闭后新请求失败、最后流 join 前受保护副作用次数为 0、旧 token 不删除后继。
- [x] **Verify RED/negative controls:** 独立三成员集群运行新测试；临时测试开关注入“提前 End”及“Watch 当权威”错误时测试必须失败，然后移除错误注入。这是测试有效性验证，不保留产品绕过开关。
- [x] **Implement fault fixtures:** 使用包内受控 runtime/helper 与 etcd client 包装器，屏障注入断连、丢响应、revocation、进程暂停和恢复；不通过 sleep 推断已排空。另测三成员中一个停止时读写仍可继续、失去 quorum 时新准入失败；重连后不复活旧请求。无法证明远端副作用隔离的情况必须列为失败/阻塞，不能用 mock 取消成功代替真实证明。
- [x] **Verify safety:** `bash scripts/test-etcd-operation-coordination.sh safety`；其中运行真 etcd `go test -race ./internal/storage/state/etcd ./internal/sandbox -run 'TestSharedData|TestActiveChanges|TestActiveChangeHub' -count=1 -v`。再运行 `go test ./...` 和 `go vet ./...`，明确普通运行中哪些既有集成测试因未配置后端而跳过。
- [x] **Load comparison:** `BenchmarkSharedDataCoordination` 子项 legacy/shared × one_hot/many_cold/long_stream/mixed_replicas，分别记录稳态和首次注册/15s 空闲退出/10s 维护。基线至少含 10000 次短请求、64 并发、1 与 2 个 Manager、2048 个低频 sandbox（覆盖 1024 容量回退）；单轮不少于 60s，热身与 steady 分开。`bash scripts/test-etcd-operation-coordination.sh bench` 输出 RPC/业务请求、KV 写入、Grant/Revoke/KeepAlive、Watch 量、Lease/组峰值、DB 增长与 p50/p95/p99；业务与后台任务分表，不以一次均值替代尾延迟。
- [x] **Acceptance report:** 每个安全用例、实测命令、提交、镜像版本、拓扑和结果写入报告；热组短请求达到 1 只读 RPC、0 每请求写入，组成本另计。低复用场景若不受益如实记录。默认开关仍 false；报告中给出单实例开关、正常排空后关闭/回滚、不能删除 etcd keys 的运维步骤。没有 Kubernetes/真实远端故障验证时，明确“本地协调验证通过、生产灰度条件未满足”，不宣称全面安全或直接上线。
- [x] **Commit:** `test(sandbox): verify shared operation safety and coordination costs`。完成分支 diff 与安全边界复核后，报告通过项、未验证项和是否满足灰度条件；没有用户部署指令不执行生产 rollout。

## Plan Self-Review

- Spec 覆盖：共享记录/身份/容量/失效对应 Tasks 1/3，单请求权威读/stream/shutdown 对应 Task 4，Watch/revision/排空对应 Tasks 2/5，开关/兼容/回滚与指标对应 Tasks 5/6，故障证明与收益对应 Task 6。
- 保留旧 operation 契约；新仓库方法只供 Data 组使用，跨任务签名一致。Manager 不接触 clientv3 或底层 key。
- Review Focus 的五类输入均有点名测试；不把有界本地测试或 mock 结果扩大成远端隔离结论。
- 执行前须用户评审本计划并选择执行方式；用户已确认计划并选择当前会话逐项实现；复选框按实际验证进度更新，执行证据见验收报告。
