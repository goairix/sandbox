# 普通池策略收尾修复实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [x]`) syntax for tracking.

**Goal:** 关闭 v0.3.20 验证发现的普通池 Pod 删除后、策略删除前的中断重试窗口。

**Architecture:** 沿用用户已确认的 API 整改设计 B/E，以及既有 RuntimeRef、OrdinarySandboxRemover、OrdinarySandboxPolicyCleaner 能力。保留短退休预算和正常 Pod 终止确认；先持久发布 cleanup，再完成精确 UID 的 runtime/策略收尾，最后 CAS 退役记录。只修改普通池/runtime 清理，不扩展 Redis Cluster、不调整性能探针、不操作业务集群。

**Tech Stack:** Go、Kubernetes typed/dynamic clients、现有 AtomicStore、fake API tracker、隔离 Redis。

## Task 1：精确 runtime 缺失后的策略恢复

Files: `internal/runtime/kubernetes/ordinary_cleanup_test.go`、`ordinary_network.go`、`runtime.go`。

- [x] 添加指定 RuntimeRef、Pod 已不存在、普通及 Cilium 策略仍存在的失败回归；检查目标策略删除、其它 UID 策略保留。覆盖策略删除失败、Pod 重现、可选 Cilium policy API 404。
- [x] Run: `go test ./internal/runtime/kubernetes -run 'TestExactOrdinaryRemoval|TestOrdinaryDeletionTimeout' -count=1`；确认残留策略导致断言失败。
- [x] 为既有 `cleanupBoundOrdinaryPolicies` 增加 `expectedUID string`，过滤非目标 UID；保持完整策略归属、namespace、Pod absence 重查、policy UID 删除前置条件及删除后读回确认。

```go
if expectedUID != "" && identity.runtimeUID != types.UID(expectedUID) {
    continue
}
```

- [x] 删除 runtime 的 NotFound 分支先执行 bound policy cleanup，再决定 nil/ErrNotFound，不因指定 UID 提前跳过策略。
- [x] 为正常删除确认预算超时增加 ErrTerminationUnconfirmed 分类，保留原 context 错误；403 等真实错误不降级。重复运行上述命令直到通过。

## Task 2：普通池缺失 Pod 的 cleanup 记录收尾

Files: `internal/sandbox/ordinary_pool_cleanup_test.go`、`ordinary_pool_shared.go`、`pool_test.go`。

- [x] 添加旧 pool record、没有 Pod 但有模拟实际策略副作用的失败回归；第一次终止不确定、第二次策略失败均保留 cleanup 记录，第三次成功才删除。另测同名 replacement、无 cleaner、未绑定准备项。
- [x] Run: `go test ./internal/sandbox -run 'TestOrdinaryPoolCleanup' -count=1`；确认记录过早删除/策略未清理导致失败。
- [x] 已绑定 RuntimeID/UID 时，即使 inventory 未返回 Pod，也必须调用精确 remover；NotFound 时要求既有 policy cleaner 收尾，失败不 CAS 删除记录。

```go
ref := runtime.RuntimeRef{ID: record.RuntimeID, UID: record.RuntimeUID}
err := remover.RemoveOrdinarySandbox(ctx, ref)
if errors.Is(err, runtime.ErrNotFound) {
    cleaner, ok := p.pool.runtime.(runtime.OrdinarySandboxPolicyCleaner)
    if !ok { return errors.New("ordinary pool policy cleanup capability is unavailable") }
    err = cleaner.CleanupOrdinarySandboxPolicies(ctx, ref, record.RuntimeID)
}
if err != nil { return err }
```

- [x] 测试 sharedPoolRuntime 补齐 policy cleaner 契约，实际检查原 runtime 缺失；不将同名替换解释为成功。
- [x] 短预算的已分类 termination pending 记录 WARN，其它退休错误仍 ERROR；不增加长时间全局锁持有或后台 goroutine。重复上述测试直到通过。

## Task 3：验证、复审与提交

- [x] Run: `go test ./internal/runtime/kubernetes ./internal/sandbox -count=1 -timeout=120s`，验证已有替换 UID、claim、库存及退休竞争回归。
- [x] 使用本机独立临时 Redis，Run: `TEST_REDIS_ADDR=127.0.0.1:16381 go test -p 1 ./... -count=1 -timeout=120s`。
- [x] Run: `TEST_REDIS_ADDR=127.0.0.1:16381 go test -race -p 1 ./internal/runtime/kubernetes ./internal/sandbox ./internal/storage/state -count=1 -timeout=180s`。
- [x] Run: `go build ./...`、`go vet ./...`、`golangci-lint run --new-from-rev=HEAD`、`bash scripts/test-helm-chart.sh`、`git diff --check`。
- [x] 复审 namespace/UID/替换/权限/证据保留以及稳态无全量孤儿扫描；补充中文整改报告，区分本地通过和待用户构建部署后的现场验收。
- [x] 停止并移除本次隔离 Redis，提交明确文件，不 push、不部署、不删除历史业务资源。
