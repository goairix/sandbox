# Warm Pool Contract Rollout Implementation Plan

> **For agentic workers:** Implement this approved design locally without worktrees; use requesting-code-review for an independent read-only review.

**Goal:** Kubernetes 普通池和 FUSE 池按运行契约更新，而不是按 API 进程生命周期更新。

**Architecture:** 契约指纹覆盖运行镜像、资源、安全属性及显式版本化的 Kubernetes Pod/控制协议模板。兼容库存跨进程保留；新契约先补齐预热容量，再通过 release-scoped owner lease 和 CAS 清理无人使用的旧库存。claimed/reserved/binding/consumed 库存不参与升级清理。Docker 保留原本退出清池行为。

**Tech Stack:** Go, Redis AtomicStore/FUSEPoolRepository, Kubernetes client-go.

## 已批准的验收规则

- API 镜像、日志、副本数变化不改变池契约。
- 运行镜像、资源、安全或模板/协议变化改变契约。
- 同契约所有 API 暂时退出后，新副本仍可领取原预热 Pod。
- 旧副本仍有 lease 时不得清理旧契约库存；新池补齐后才清理 owner-free 的旧 prepared 库存。
- 删除必须使用记录 CAS 和不可变 UID；领取与升级清理竞争时最多一方成功。
- 无法确认状态时保留旧库存并重试，不能阻止已健康的新池服务。
- uninstall 使用显式 DrainRelease，而非普通 Stop。

## Task 1: 普通池契约与滚动交接

Files: `internal/sandbox/pool.go`, `ordinary_pool_shared.go`, `pool_test.go`, `cmd/sandbox/main.go`, `internal/runtime/runtime.go`, `internal/runtime/kubernetes/runtime.go`.

- [x] 添加失败测试：单副本 Stop 后兼容 warm Pod 保留并被新副本复用；契约差异换 key；旧 owner 退出后新 refill 清理旧 prepared、保留 claimed；替换准备失败不清理旧池。
- [x] Run `go test ./internal/sandbox -run 'Test.*OrdinaryPool.*(Contract|Restart|Retire|Replacement)' -count=1`，确认行为断言失败。
- [x] 扩展 PoolConfig 的 PidLimit、SeccompProfile、RuntimeContract，指纹包含固定 UID/read-only 及版本。构建 spec 使用同一默认化安全值。Kubernetes 暴露稳定的模板协议标识，不能包含 API 镜像版本。
- [x] Start/retirement 共用 scope lock；Stop 只释放 owner。refill 补齐后 CAS 清理无 owner 的旧 prepared；后台定期补池/退旧，失败仅记录并重试。
- [x] 再运行目标测试及 `go test ./internal/sandbox ./internal/runtime/kubernetes ./cmd/sandbox`。

## Task 2: FUSE 兼容库存保留与安全退旧

Files: `internal/sandbox/fuse_pool.go`, new `fuse_pool_rollout.go`, `fuse_pool_test.go`, `cmd/sandbox/main.go`, `internal/runtime/runtime.go`.

- [x] 添加失败测试：启用 shared inventory 时 Stop 保留 prepared，新副本复用相同 UID；不同契约有活跃 owner 时不退旧，owner 退出后退旧；reserved 不删；替换失败不退旧；其他 release 库存不删。
- [x] Run `go test ./internal/sandbox -run 'TestFUSEPoolShared' -count=1`，确认行为失败。
- [x] Kubernetes 配置 release-scoped owner registry/lease；登记与退旧共用 scope lock，续租失败暂停 controller/领取，就绪检查失败，恢复后在 scope lock 下重登记并自动恢复。契约加入 runtime template/scope，Stop 注销 owner 而不 drainOwned；Docker 仍 drainOwned。
- [x] reconcile 补齐且验证新 prepared 后，在 scope lock 下读取旧 owner，CAS claim 旧 prepared 并用 exact UID 删除。protected/unknown 保留，CAS 竞争视为稍后重试。
- [x] Run `go test ./internal/sandbox -count=1`。

## Task 3: 复审与验证

- [x] 自查 owner/claim/Stop 竞争及跨 release 隔离，记录兼容迁移边界。
- [x] Run `go test ./...`, `go vet ./...`, `go test -race ./internal/sandbox ./internal/runtime/kubernetes ./cmd/sandbox`。
- [x] Run `go build -o /tmp/sandbox-api-warm-pool-verify ./cmd/sandbox`，不构建/推送线上镜像，不执行 Helm 升级。
- [x] 更新运维文档：只变 API 不换池；运行契约改变先补后退；活跃业务沙盒不重启；显式 release drain 清空库存。

## 实施与复审补充

- shared FUSE 正常启动不执行 namespace-wide orphan snapshot 删除；显式 exclusive drain 保留该历史路径。
- 恢复既有 FUSE 业务实例不要求匹配新 warm key，但仍要求 consumed 记录、revision/reservation、不可变 UID、存储 owner 和健康证明；业务结束可正常清理旧代。
- Kubernetes resolver 可从受信 preparation ID/pool contract 恢复未发布 UID，并在 CAS cleanup claim 中绑定证据，避免孤儿遗留。
- 普通 refill 合并并发调度且保留 pending wakeup；每轮退旧预算 5 秒，异常旧 Pod 留记录重试。
- 普通周期 reconcile 不物理删除 claimed：领取超时或副本租约缺失不能隔离仍在进行的身份迁移。正常业务清理继续处理已接管实例，无法确认的遗弃 claim 留给受控 exclusive drain。
- 未登记 legacy FUSE 不盲目退旧；排空历史范围仍要求 namespace/Redis DB 专属于 release，独立复审已确认并在运维文档明确。

## 最终验证（2026-09-13）

- 临时本地 Redis 7，`TEST_REDIS_ADDR=127.0.0.1:6389 go test ./... -count=1` 全量通过。
- 同一 Redis 下 `go test -race ./internal/sandbox ./internal/storage/state/redis ./internal/runtime/kubernetes ./cmd/sandbox -count=1` 通过。
- `go vet ./...` 和 sandbox-api 二进制编译通过。
- 独立只读复审完成；补充的 claim 超时且 owner lease 消失测试先复现误删，再验证修复。
- 未进行线上写入、镜像构建发布或 Helm 升级。
