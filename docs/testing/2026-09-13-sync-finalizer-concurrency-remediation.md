# 普通 sync finalizer 并发清理整改

## 结论与范围

v0.3.21 线上矩阵中普通 sync DELETE 的 stale-token 竞争已补确定性回归并修复。本地全量、真实 Redis 定向、race、构建、vet、增量 lint 和 Helm lint 均通过，独立最终复审无阻断问题。

这是仓库修复结果，**不是部署后全量通过结论**。本轮没有构建发布镜像、部署、升级、回滚、重启/缩容集群，也没有手工删除线上 Redis 或 Pod/网络策略。线上仍需用户部署包含此提交的新 `sandbox-api` 镜像后复跑；旧镜像不能验证新代码。

## 根因与实现

原 finalizer 已写入 `sync_final_output_done`，仍在等待普通 Pod 正常终止。后台扫描先匹配 checkpoint 恢复分支，再检查本地 lifecycle，恢复过程主动停止并释放原 controller。原 DELETE 后续 Fence/checkpoint 因 token 不再有效而返回 503；恢复者最终清理完成，造成“请求失败但随后无本轮遗留”的现象。确定性回归证实该代码窗口；不声称已完整还原每个历史请求的 token 时序。

修复保持 Redis 权威控制面、精确 UID/generation/CAS 和数据直连，无 API 间 HTTP 转发：

- 本地扫描优先调度已有正常 sync finalizer，不撤销其 controller。远端扫描发现有效持有者直接跳过，不因一个慢销毁拖住整个扫描。
- DELETE 命中 finalizer checkpoint 时，专属函数复用身份匹配的本地 finalizer；异地请求等待该持有者完成。真正的 mount/sync/unmount transition 仍走中断恢复，真实 `unmount_synced` 不被误当成正常 finalizer 检查点。
- `finalizeMu` 无竞争时仍走一次 TryLock；重复请求等待锁时响应 context 超时。只有最终清理 CAS 成功才设置 `finalizeDone`，重复 worker 不再 Fence 已经删除的记录。
- 异地等待读到记录消失后调用现有 `ConfirmRecordAbsence`，保留配置要求的持久化 ACK。删除 Lua 已执行但 ACK 未确认时不能假报成功。
- 请求拿到 checkpoint 快照后，原持有者可能已经完成；Acquire 返回 stale 时，只有重新读取记录确实不存在且终态确认成功才能幂等成功。记录仍在、generation/UID 替换、lost controller、transport 错误均不吞成成功。

本批不修改 Pod graceful termination，不宣称普通销毁从约 30 秒变成瞬时；修复的是有效清理过程中自抢占产生的错误，以及等待的性能/可用性边界。

## 红绿验证与覆盖

首先修正测试夹具类型并确保可构建，再观察旧生产实现的 7 个子场景失败：ephemeral/persistent 本地扫描、两种模式的本地/异地重复 DELETE，以及已完成 finalizer 的重复调用。错误对应 cleanup pending/deadline、原 controller stale 或异地提前 pending，而非构建失败。

复审追加的回归也先观察到 RED，再修复：远端扫描等待阻塞、真实 unmount 已同步后错误复用 local finalizer、absence ACK 失败被当成功，以及 Redis 在 snapshot→Acquire 之间记录消失的 durable/unconfirmed 两种结果。最终全部 GREEN。

`internal/sandbox/sync_cleanup_concurrency_test.go` 覆盖：

- ephemeral/persistent checkpoint 后阻塞精确删除，扫描保持原 token；重复同副本/异地 DELETE 超时不抢占。
- 内存与真实 Redis 的异地 DELETE 等待成功；运行中 controller 保持有效。
- 每次成功确认 runtime 仅精确删除一次，active、owner、lease 及对应 session/ephemeral lifecycle 无遗留。
- 真正失效的 controller 继续报 stale，保留 checkpoint，释放旧能力后异地可完成恢复。
- 真实动态 `unmount_synced` 后 runtime 不存在，禁止重放最终输出，仍能恢复。
- 终态 absence 持久化失败必须报错；Acquire 前记录消失只有持久确认才能返回成功；记录仍在时 stale 继续报错。

现有最终 CAS 失败回归还增加了“不得标记完成、重复 DELETE 继续报失败并保留证明”的断言。

## 已执行的最新验证

隔离本地 `redis:7-alpine`，仅绑定 `127.0.0.1:16381`；新增真实 Redis 测试用 DB 14 和唯一 scope。未连接业务 Redis。实际复制 ACK 失败用 repository 故障注入验证；这不是 Redis 主从切换的现场验收。

| 命令 | 结果 |
| --- | --- |
| `TEST_REDIS_ADDR=127.0.0.1:16381 go test -p 1 ./... -count=1 -timeout=120s` | 全部包通过；需部署端点/chaos driver 的 opt-in 场景不计为现场通过 |
| `TEST_REDIS_ADDR=127.0.0.1:16381 go test -race ./internal/sandbox -run 'TestDistributedSyncCleanup\|TestSyncFinalizer\|TestDistributedSync\|TestDistributedPersistentSync\|TestDistributedWorkspace' -count=20 -timeout=180s` | PASS，19.951 秒，无 race |
| `TEST_REDIS_ADDR=127.0.0.1:16381 go test -race -p 1 ./internal/sandbox ./internal/runtime/kubernetes ./internal/storage/state/redis ./internal/api/handler -count=1 -timeout=180s` | 四个包通过，无 race |
| 独立复审重复上述定向 `-race -count=3` | PASS，4.424 秒，无提交阻断项 |
| `go build ./...`、`go vet ./...` | 通过 |
| `golangci-lint run --new-from-rev=HEAD --timeout=5m` | 0 新问题；不声称历史 lint 全部清零 |
| `helm lint deploy/helm/sandbox`、`git diff --check` | 通过；Chart 仅提示推荐 icon |

## 部署后继续验收

本批只有 API Go 代码、测试与文档变化，不新增配置，不需要更新 runtime/FUSE mounter 镜像或同步 Chart。用户构建并部署新 `sandbox-api` 后，按 `2026-09-13-ds-ai-research-v0.3.21-validation.md` 重新核对三副本镜像摘要，再执行 API 整改套件、完整工作区矩阵、预备 UID 复用和最终 Pod/NP/CNP/Redis 审计。

普通 sync 必须包括销毁后重开验证与两方向同前缀互斥，不能只看 DELETE 最终无遗留。单独重跑的准确过滤为：

```sh
WORKSPACE_FUSE_API_URL=http://127.0.0.1:18083 \
go test ./test/integration/workspacefuse \
  -run '^TestWorkspaceFUSE$/^ephemeral_sync_finalization$' -v -count=1 -timeout=3m \
  -args -runtime kubernetes -profile minio-sigv4-path-style-v1
```

API 密钥仍从 Secret 注入环境变量，不写入报告。Calico/双华云、生产 LSM、Redis 真实高可用故障切换、长期性能 p95/p99 及历史其它 scope owner 的安全审计仍未由本批本地测试验收；不得算作通过或批量删除历史状态。
