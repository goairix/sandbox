# 普通 sync finalizer 并发修复 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 消除 v0.3.21 普通 sync DELETE 的 controller 自抢占和重复完成 503，同时保留真正中断后的跨副本清理。

**Architecture:** 延续 active repository 的 controller token、generation 和精确 runtime UID。后台扫描优先调度本地 finalizer；已 checkpoint 的 DELETE 复用同一个本地 finalizer，其他副本等待持有者完成。仅在全部清理和最终 CAS 成功后记忆完成状态；不屏蔽 stale-token。

**Tech Stack:** Go、context、sync.Mutex、Redis Lua/CAS、现有 mockRuntime 和 distributedSyncManagers 测试夹具。

---

### Task 1: 确定性回归与最小实现

**Files:**
- Create: `internal/sandbox/sync_cleanup_concurrency_test.go`
- Modify: `internal/sandbox/{manager.go,sync_lifecycle.go,lifecycle_coordinator.go,cleanup_distributed.go}`

- [x] 用 `sharedPoolRuntime` 和 `blockPreparedRemovals` 在 `sync_final_output_done` 后阻塞精确删除，分别覆盖 ephemeral/persistent：本地扫描不换 token、本地重复 finalizer 返回成功、重复本地 DELETE 和异地 DELETE 等待同一持有者。通过 channel 同步，不依赖 Pod 真实删除时长。
- [x] 执行 `go test ./internal/sandbox -run 'TestDistributedSyncCleanup|TestSyncFinalizer' -count=1 -timeout=60s`，确认旧实现失败在 controller 被替换或重复完成 stale-token，而不是构建错误。
- [x] 在 `destroySyncSandbox` 的 `finalizeMu` 内增加 `finalizeDone` 检查，仅在最终记录删除成功后设 true。
- [x] 扫描的本地正常 sync 分支移到 checkpoint 恢复分支前；有 workspace transition 的记录仍走中断恢复。
- [x] 独立复审后收紧为 checkpoint 专属 `cleanupCheckpointedSyncWorkspace`：原始 transition 为空、缓存无 transition、UID/generation 匹配时复用 `destroySyncSandbox`。真实 `unmount_synced` 不能靠字符串猜测为 synthetic，继续中断恢复。DELETE 获取 controller 被占用时等待；后台扫描跳过活跃持有者，不释放有效 token、不阻塞其它记录。
- [x] 重跑定向回归和既有 `TestDistributedSync|TestDistributedPersistentSync|TestDistributedWorkspace`，确认有效持有者与真正中断恢复均通过。

### Task 2: 失败路径、真实 Redis、复审

**Files:**
- Modify: `internal/sandbox/sync_cleanup_concurrency_test.go`
- Reuse: `internal/sandbox/cleanup_recovery_test.go`、`workspace_distributed_test.go`

- [x] 增加异地等待超时不换 token、失效 token 必须仍报错、最终 CAS 失败不能标记完成的断言；真实 Redis 使用唯一 repository scope。
- [x] 增加真实 unmount 阶段不重放输出、远端扫描不等待、清理结束需要持久化 absence ACK、Acquire 前记录消失及 ACK 失败两种回归；记录仍在时 acquisition stale 不可吞。
- [x] 执行定向测试 `-race -count=20`；使用独立本地 Redis 执行 `TEST_REDIS_ADDR=127.0.0.1:16381 go test -p 1 ./... -count=1 -timeout=120s`，不得对部署 Redis 执行清空操作。
- [x] 按 requesting-code-review 技能独立复审变更；修复有效问题后重跑相关回归，复审最终定向 `-race -count=3` 通过，无阻断项。
- [x] 执行 `go build ./...`、`go vet ./...`、增量 lint、Helm lint 和 `git diff --check`。

### Task 3: 交付

**Files:**
- Create: `docs/testing/2026-09-13-sync-finalizer-concurrency-remediation.md`

- [x] 写中文根因、RED/GREEN、全量结果和未完成的线上验收条件。此次仅 API 代码变更，无 chart/config 变更。
- 交付方式：按用户既有要求将本批已验证文件提交为 `fix(sandbox): preserve sync finalizer ownership during cleanup`，保留当前分支，不推送。
- [ ] 不构建发布镜像、不 Helm upgrade、不回滚；用户部署新 API 镜像后执行上一份报告的完整线上测试，未部署不能宣称线上通过。
