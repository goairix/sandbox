# 普通沙盒并发销毁整改

## 问题及修复

2026-09-14 的 [AppArmor 启用后 API 测试](2026-09-14-apparmor-api-validation.md) 中，三个普通沙盒分别向三个 API 副本同时 DELETE，共 9 次请求、6 次返回 `503 SANDBOX_CLEANUP_PENDING`。切换本地网络后仍稳定复现，根因为普通分支未取得清理控制器就立即返回错误。

本次仅修改本地代码：普通等待者复用既有 `waitForActiveCleanup`，等待原控制器完成并确认活动记录的持久化缺失；仍只有 fenced owner 执行精确 UID 的运行时删除。后台扫描遇到 live owner 则跳过，不让新增等待阻塞其它实例的恢复。获取控制器之前记录恰好被原 owner 删除的竞态，也必须确认缺失后才成功；记录仍存在时不会吞掉 stale token。

独立复审还发现一个随之出现的普通指标问题：多个等待者成功会重复扣减 `sandbox.active`、累加 `sandbox.destroy.total`。通过内部完成结果标记，让普通路径只有实际完成销毁的请求记录指标；等待者和确认历史完成的请求不重复记录。不改变工作区分支的既有指标行为，其并发重复计数是后续观测整改项，不扩展到本次生命周期修复。

没有修改 Helm 模板、values 接口、Redis 结构、TTL、池配置、镜像或 AppArmor profile，也没有修改线上 release、节点、业务沙盒或线上 Redis。

## 测试驱动证据

先添加回归并运行旧实现：内存与专用本地真实 Redis 均复现 `cleanup already owned`；获取控制器前记录消失的 Redis 用例复现 stale token；超时等待与持久化确认用例也因提前返回而失败。再修改普通清理分支，目标用例变绿。

指标测试同样先观察失败：一次 Create、三次并发成功 DELETE 后 active=-2；快照提前消失场景 active=-1。指标修正后都归零，destroy.total=1。

新增回归覆盖：

- 内存/真实 Redis、ephemeral/persistent、同副本/跨副本的三个并发 DELETE 共享成功结果；运行时只删除一次，持久会话和活动记录删除。
- 原副本及两个 peer 的 deadline/cancel 不影响清理 owner，不伪造清理成功。
- 后台扫描跳过正在工作的 owner；运行时终止未确认时保留恢复记录，peer 后续恢复。
- 等待者必须通过最终持久化缺失确认；Redis 快照提前消失既覆盖成功确认，也覆盖确认失败。
- 仍有活动记录时，stale capability 依旧失败。
- SDK ManualReader 验证普通并发及快照竞态的 active/destroy 指标只计一次。

真实 Redis 为独立 Docker Desktop 临时容器 `sandbox-ordinary-destroy-redis-20260914`，只绑定 `127.0.0.1` 随机端口，不挂载既有数据卷，关闭 RDB/AOF，仓库采用随机 state scope；不使用线上 Redis。镜像自动创建的匿名测试卷随测试容器一并清理。内存仓库的简化 acquire 不模拟 Redis 的缺失记录拒绝，因此提前消失竞态使用真实 Redis 脚本验证。

## 验证命令及边界

本轮 Redis 地址通过 `TEST_REDIS_ADDR` 指向上述本地实例；不将这些命令指向生产状态库。

```sh
TEST_REDIS_ADDR=127.0.0.1:50270 go test ./... -count=1 -timeout=10m
TEST_REDIS_ADDR=127.0.0.1:50270 go test -race ./internal/sandbox \
  -run 'TestDistributedOrdinaryCleanup|TestDistributedSyncCleanup|TestKubernetesDistributedDestroy' \
  -count=10 -timeout=5m
go vet ./internal/sandbox ./internal/api/... ./test/integration/api
golangci-lint run --new-from-rev=d932b6e ./internal/sandbox/... ./test/integration/api/...
git diff --check
```

上述最终命令全部退出 0，全仓普通 Go 测试通过（其中 Redis bootstrap 包 55.824 秒），普通/sync/分布式销毁目标 race 连续 10 轮通过（29.431 秒），vet 与新增 lint 问题检查通过（0 issues），Go 文件已 gofmt。独立复审发现的指标 P2 已整改，复审者再跑普通清理测试通过（5.521 秒），最终结论可合入。

完整 lint 在既有文件报告 28 条历史告警（19 errcheck、2 ineffassign、7 staticcheck）；本次新增问题检查单独验收，不宣称全仓 lint 干净。

新增 `TestDeployedOrdinaryConcurrentDestroy` 是显式 opt-in HTTP 回归，默认跳过。用户部署新 API 镜像后，设置至少三个不同的直接副本 URL 与 API key 再执行：

```sh
go test ./test/integration/api -run '^TestDeployedOrdinaryConcurrentDestroy$' -v -count=1 -timeout=8m
```

该用例为每种普通模式创建一个隔离沙盒，同时向三个副本 DELETE，要求每份响应为 200 且有有效的 `sandbox destroyed` JSON，不把空 200 当成功；之后所有副本 GET 均 404。清理独立于测试 context，所有 worker 汇合后再断言。

本地测试不等同于新镜像已在线验收。无需新增线上 values 项；仅需重建、更新 `sandbox-api` 镜像版本，AppArmor 加载器、普通 runtime、FUSE mounter 镜像及 Helm templates 无需因本修复重建或修改。不需要 uninstall、清空 Redis 或重建 PVC。
