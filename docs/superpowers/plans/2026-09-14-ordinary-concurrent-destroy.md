# 普通沙盒并发销毁修复计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 普通无工作区沙盒的重复 DELETE 等待同一个受租约保护的清理结果，而不是立即返回 cleanup pending。

**Architecture:** 使用既有 `waitForActiveCleanup` 和最终持久化确认，不新增 HTTP 转发、后台 goroutine 或配置。请求路径等待；后台生命周期扫描遇到仍持有销毁权的实例时直接跳过。持有过销毁快照但到获取销毁权时记录已消失的请求，只有确认记录持久化缺失后才成功；仍存在的 stale generation/UID 不放行。

**Tech Stack:** Go、ActiveSandboxRepository、现有 mock runtime、内存状态仓库和可选本地真实 Redis。

用户在 [线上验证记录](../../testing/2026-09-14-apparmor-api-validation.md) 的根因、修复方向与边界说明后要求“修复”，本计划据此直接在当前分支执行，不创建 worktree、不修改线上部署。

## 取舍与验收

复用既有受 context 控制的等待路径；不采用直接吞掉 503（无法确认清理），也不让请求绕过控制器重复删除运行时。保持现有 50 ms 等待轮询，不额外提高频率；新增等待不进入后台扫描路径。所有者失败后的恢复继续由既有生命周期协调器负责。

## Task 1：失败回归

**Files:** Create `internal/sandbox/ordinary_cleanup_concurrency_test.go`；参考现有 `sync_cleanup_concurrency_test.go` 的阻塞运行时、控制器观察和持久化确认仓库。

- [x] 使用 `distributedSyncManagers` 创建三个副本，共享 `sharedPoolRuntime`，普通创建请求不设置 WorkspacePath。`blockPreparedRemovals` 在精确 runtime 删除处阻塞第一个 DELETE；通过 `observedCleanupRepository.owned` 确认重复请求已检查销毁权后再解除阻塞，避免凭 sleep 猜测顺序。
- [x] 测试内存及真实 Redis 仓库、ephemeral/persistent 两种普通模式；三个并发调用全部成功，runtime 只删除一次，活动记录/持久会话删除。
- [x] 覆盖重复请求 deadline/cancel、后台扫描不等待 live owner、runtime 清理失败后记录保留及 peer 恢复、等待完成仍要求持久化确认、记录消失发生在获取销毁权之前、仍存在的 stale token 不放行。
- [x] 先执行并观察旧实现返回 `cleanup already owned` 的预期失败：

```sh
go test ./internal/sandbox -run '^TestDistributedOrdinaryCleanup' -count=1 -timeout=2m
```

## Task 2：最小生产改动

**Files:** Modify `internal/sandbox/active_store.go`、`internal/sandbox/lifecycle_coordinator.go`、`internal/sandbox/manager.go`。

- [x] 保留原入口，以带显式等待策略的内部方法承载实现：

```go
func (m *Manager) destroyDistributedSandbox(ctx context.Context, id string) error {
    _, err := m.destroyDistributedSandboxWithWait(ctx, id, true)
    return err
}
```

- [x] 普通分支未获取销毁权时使用以下逻辑；其它工作区分支的既有行为不改变：

```go
if !acquired || controller == nil {
    if !waitForOwner {
        return false, nil
    }
    return false, m.waitForActiveCleanup(ctx, id)
}
```

- [x] 对获取销毁权时的 `ErrActiveSandboxStaleToken` 加载最新记录：读取失败保留错误；记录仍在保留 stale 错误；记录消失调用 `confirmActiveCleanupAbsence`。不放宽 UID 删除、控制器 fencing 或持久化门禁。
- [x] 生命周期协调器调用等待策略 false 的内部入口；用户 DELETE 和 release drain 继续使用等待入口。
- [x] 重跑失败用例，确认变绿；`gofmt` 格式化新增/修改 Go 文件。
- [x] 复审发现普通等待者成功会重复更新指标；先用 SDK ManualReader 观察 RED（active=-2），再让内部方法返回是否应记录销毁指标。普通等待者/记录已确认消失返回 false，fenced owner 完成返回 true；manager 仅 true 时计数。回归同时覆盖同副本、跨副本及快照提前消失。

## Task 3：HTTP 回归、复审与提交

**Files:** Create `test/integration/api/concurrent_destroy_test.go`、`docs/testing/2026-09-14-ordinary-concurrent-destroy-remediation.md`。

- [x] 新增显式 opt-in 的线上 HTTP 用例：验证至少三个不同副本 URL；每种普通模式创建一个隔离沙盒；三个请求同时 DELETE，逐一读取有界响应、严格校验 200 JSON；随后三个副本 GET 均 404。goroutine 不调用 require.FailNow，所有结果汇合后由测试线程断言，独立 cleanup 无论失败与否均执行。
- [x] 执行以下本地验证；真实 Redis 用独立本地容器和随机 state scope，不读取或修改线上 Redis。

```sh
go test ./internal/sandbox ./internal/api/... ./internal/storage/state/... ./internal/runtime/kubernetes ./test/integration/api -count=1 -timeout=10m
go test -race ./internal/sandbox -run 'TestDistributedOrdinaryCleanup|TestDistributedSyncCleanup|TestKubernetesDistributedDestroy' -count=1 -timeout=5m
go vet ./internal/sandbox ./internal/api/... ./test/integration/api
golangci-lint run --new-from-rev=d932b6e ./internal/sandbox/... ./test/integration/api/...
git diff --check
```

- [x] 按 requesting-code-review 技能安排独立复审，处理实际问题；记录 RED/GREEN、真实 Redis覆盖与未部署边界，更新本计划状态。

收尾约定：只提交本次文件，不混入用户并行修改；不构建或推送镜像。用户只需重建、更新 sandbox-api 镜像版本，无需新增 Helm/values 配置或重建加载器/运行时镜像；线上通过结论须等用户更新后复测。

完整 lint 检查报告 28 条既有告警，本次不混入不相关整改；基于 d932b6e 的新增问题检查单独验收。最终验证还增加全仓 `go test ./...` 及目标 race 连续 10 轮，不将 opt-in 线上用例的默认跳过当成线上通过。
