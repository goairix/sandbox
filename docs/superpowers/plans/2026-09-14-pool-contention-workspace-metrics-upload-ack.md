# 普通池竞争、工作区销毁指标与上传确认实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (if subagents are available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 修复已确认规格中的三项问题，保留运行时精确身份、清理 fencing 和流式上传边界。

**Architecture:** 普通池遇到合法快照争用后有界重新扫描；工作区清理将实际完成者结果传给现有指标层；上传测试助手读取并严格校验有限长度 ACK，同时关闭 pipe、等待 producer 退出。不串行化取池，不新增 Redis 持久化字段或遥测锁。

**Tech Stack:** Go、state.AtomicStore/Redis、OpenTelemetry SDK ManualReader、httptest、现有 runtime 测试替身。

**范围：** 已批准的 [规格](../specs/2026-09-14-pool-contention-workspace-metrics-upload-ack-design.md)，基线 `f2b299c`，当前分支直接实施，不创建 worktree、不修改线上或 Helm。任务顺序实施，每项先观察 RED，再 GREEN、规格与质量复审；用户统一构建部署。

## Task 1：普通池有界快照刷新

**Files:** 修改 `internal/sandbox/ordinary_pool_shared.go`；新增 `internal/sandbox/ordinary_pool_contention_test.go`；复用 `ordinary_pool_cleanup_test.go`、`ordinary_pool_inventory_test.go` 和共享池测试替身。

- [x] 在真实原子存储操作外围加入一次性测试交错：扫描读取 prepared 后，领取更新 claimed/确认删除，或业务 Pod 身份迁移。覆盖启动扫描和稳态 refill，断言业务 runtime 未删除、未重新发布/收录，并能补齐池容量。
- [x] RED：`go test ./internal/sandbox -run 'TestOrdinaryPool.*Contention' -count=1`；预期旧实现报 `ordinary pool record changed before cleanup claim`。
- [x] 为扫描引入可识别的快照冲突结果。清理 CAS 失败先校验最新记录；合法状态变化请求重新加载记录和库存，不沿用旧索引执行 orphan 扫描。形状为 `for attempt := 0; attempt < limit; attempt++ { ... if errors.Is(err, errSnapshotChanged) { continue } ... }`，每轮受 context 和原有共享锁约束。
- [x] 保留损坏记录、非法状态、存储错误、UID replacement、未确认终止和缺失 Pod 策略收尾负例；不把所有 CAS 失败当成功，不对稳态进行 broad inventory。
- [x] GREEN：`go test ./internal/sandbox -run 'TestOrdinaryPool|TestSharedOrdinary' -count=1`；独立本地 Redis 重跑相关用例，再运行目标 race 重复轮次。
- [x] 规格与质量复审，修正问题后提交该项代码。

## Task 2：工作区清理的实际完成结果

**Files:** 修改 `internal/sandbox/active_store.go`、`cleanup_distributed.go`、`sync_lifecycle.go`、`manager.go`（必要的调用边界）；新增 `internal/sandbox/workspace_destroy_metrics_test.go`、`workspace_destroy_metrics_recovery_test.go`、`workspace_destroy_metrics_absence_test.go`、`workspace_destroy_metrics_store_test.go`，分别组织主场景、恢复、缺失/取消和真实存储隔离；复用普通指标捕获和 sync/FUSE 生命周期测试设施。

最终复审的原子完成结果补充：涉及 `internal/sandbox/lifecycle_coordinator.go`、`internal/storage/state/active_sandbox.go`、`internal/storage/state/redis/active_sandbox.go` 及对应仓储/故障注入测试。保留原 error-only 接口，新增内部完成结果能力；同一 Lua 返回本次删除或原本已缺失，不增加请求、状态 key 或持久化结构。

- [x] 用 SDK ManualReader 捕获 `sandbox.active`、`sandbox.destroy.total`。创建一次后同副本/跨副本并发销毁，覆盖 ephemeral/persistent、sync/FUSE、final-output checkpoint 和 interrupted recovery，断言 `active == 0`、`destroy.total == 1`，并检查 runtime/session/owner/lease/active 收尾。
- [x] RED：`TEST_REDIS_ADDR=127.0.0.1:56861 go test ./internal/sandbox -run 'TestDistributed(Sync|FUSE)DestroyMetricsCountOnlyCompletingWorker' -count=1`；旧实现因等待者无条件返回完成标记而重复计数。补充 FUSE 公共创建漏记 active 加一的 RED，再平衡同一个 active 字段，失败创建不计入。
- [x] 保留 error-only helper 兼容已有后台调用，在内部增加 `(bool, error)` 结果或同等完成结果。生命周期互斥锁持有者仅在本次真正完成收尾时返回 true，等待者、历史缺失确认返回 false；传递到 `destroyDistributedSandboxWithWait`，不增加新指标写入依赖。
- [x] 检查取消、checkpoint、失效 controller、replacement 和未确认持久化边界；错误不得伪造 true 成功。不扩大为全遥测改造，不承诺崩溃事务级 exactly-once。
- [x] 补充 RED：只暂停第一个 owner 在 Fence 后的删除调用，释放其 controller，让无缓存 peer 完成 checkpoint 恢复，再恢复旧调用。两者仍活着时 active 必须为 0、destroy 为 1。GREEN 必须使用仓储原子返回的本次删除标记，不能仅由 nil 幂等 ACK 推断 true；durability 未确认仍失败，旧 error-only 仓储可继续清理但不能伪造实际完成量。
- [x] GREEN：`TEST_REDIS_ADDR=127.0.0.1:56861 go test -race ./internal/sandbox -run 'Test.*DestroyMetrics|TestFailedFUSECreate|TestDistributedSyncCleanup|TestSyncFinalizer|TestDistributedFUSE' -count=10`；独立 Redis 相关路径和现有安全负例。
- [x] 规格与质量复审，修正问题后提交该项代码。

## Task 3：上传 ACK 与 producer 生命周期

**Files:** 修改 `test/integration/workspacefuse/workspace_fuse_test.go` 中 `uploadSized`；新增 `test/integration/workspacefuse/upload_sized_test.go`。

- [x] httptest 表驱动覆盖 HTTP 200 合法/零大小 ACK，缺失字段、空/截断/非法/超限/尾随 JSON、错误 path/size、204/其它状态、读错误、请求错误、取消与提前响应。合法 multipart 实际文件字节数必须匹配。
- [x] RED：`go test ./test/integration/workspacefuse -run 'TestUploadSized' -count=1`；预期旧助手把无效 2xx ACK 当成功。
- [x] ACK 使用有限完整读取（例如 64 KiB 上限加 1）和单值 JSON 校验，字段使用指针以区分缺失和合法零值：`struct { Path *string; Size *int64 }`。仅接受 HTTP 200、字段存在并与请求匹配。
- [x] 缓冲结果 channel 接收 producer 结果；所有退出路径关闭 reader 使写端解除阻塞，并等待 producer 退出。关闭响应体并保留读取/生产错误；context 取消不遗留 goroutine，仍然 `io.CopyN` 流式生成，错误不打印凭据或无限响应体。
- [x] 保留现有文件 stat、FUSE flush、销毁和压力规模检查。
- [x] GREEN：`go test -race ./test/integration/workspacefuse -run 'TestUploadSized' -count=10`；规格与质量复审，修正问题后提交该项代码。

## 总体验证与交付

- [x] 独立本地临时 Redis（非线上，不复用现有状态）执行目标回归；清理本轮准确创建的容器与匿名卷。
- [x] `go test ./... -count=1`；目标并发/上传用例 race 重复；`go vet ./internal/sandbox ./internal/storage/state/redis ./test/integration/workspacefuse`。
- [x] `golangci-lint run --timeout=3m --new-from-rev=f2b299c`、`git diff --check`。全仓历史 lint 不在本轮整改范围，不把增量通过说成全仓清零。
- [x] 最终独立复审，记录中文整改报告和实际命令/结果（线上、故障注入 opt-in 跳过不计为通过），更新本计划完成状态，提交。
- [x] 告知用户涉及 API 镜像重建，是否还有其它镜像按实际修改判断；本轮不自动发布、升级或声称线上已验证。

**执行备注：** 三项没有代码依赖。一个普通池实施 agent 工作期间，主流程先完成独立上传助手；普通池评审结束后再派遣工作区指标实施 agent，没有并行派遣多个实施 agent。后者补恢复场景时主流程在独立文件补历史完成/取消指标断言，评审只读并行。所有产品改动均在当前工作区；私有且不再有调用者的两个旧包装函数移除，保留实际后台调用使用的 error-only 包装。

**完成记录：** 上传 `d0dfda4`、普通池 `313eb5b`、工作区指标及原子完成结果 `0d5505c`。三项规格/质量与最终整合复审通过，开放问题 0；稳定最终补丁上的完整命令、实际结果、临时资源清理及部署边界见 [中文整改报告](../../testing/2026-09-14-pool-contention-workspace-metrics-upload-ack-remediation.md)。只需重建 API 镜像，values 不变；线上验收等待用户部署后进行，销毁耗时与 FUSE 创建长尾仍后置。
