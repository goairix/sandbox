# API 验证整改 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use subagent-driven-development or executing-plans to implement task-by-task. 不创建 worktree，仅按用户明确要求提交。

**Goal:** 修复现场报告的九类问题，补可重复回归、部署安全校验与性能证据。

**Architecture:** Redis active snapshot 是 Kubernetes 权威状态；工作区控制直连 runtime，分布式排他 gate 排空冲突数据流。生命周期恢复通过持久阶段和 controller fencing，不使用 HTTP 转发。

**Tech Stack:** Go、Gin、Kubernetes client-go、Redis Lua、Helm。

**当前状态（2026-09-13）：** Task 1–5 的代码与本机回归已完成；Task 6 完成观测和基准，现场性能验收未执行；Task 7 完成只读审计、生产配置、opt-in 套件及同连接副本确认；Task 8 独立复审、最终完整本机验证和交付文档均完成。现场部署由用户负责，本机通过不代表线上验收通过。

## Global Constraints

- 当前 checkout；不创建 worktree。
- 不操作线上部署/Redis；用户统一构建镜像和部署。
- 保留 UID/generation/CAS/fail-closed；不盲重试非幂等操作。
- Docker 保留单进程，公共缺陷同步修复。
- 原有 Helm env 去重改动保留；仅按用户明确要求提交。

## Task 1：工作区排他 gate

Files: `internal/storage/state/active_sandbox.go`, `internal/storage/state/redis/active_sandbox.go`, `internal/storage/state/redis/active_sandbox_test.go`, `internal/sandbox/active_store.go`, `internal/sandbox/active_store_test.go`。

Interfaces: 在现有 BeginOperation/RenewOperation/EndOperation 中增加 exclusive 类型；`beginDistributedWorkspaceOperation(ctx,id)` 返回权威 Sandbox、operationCtx、release 和 error。持有排他 token 后 LiveOperations 排空到自身 1，检查 token 未过期再执行。

- [x] 写回归：先进入 data，进入 exclusive 后新的 data/mutation/exclusive 被拒绝；已有 data 可正常结束/续期；排空后执行；失效 token 不能更新 snapshot。
- [x] Run 真实 TEST_REDIS_ADDR 的 active/workspace 定向测试，已观察新增行为 RED。
- [x] 实施 Lua admission 与 fencing：exclusive 与 mutation 互斥，exclusive 拒绝新 data；续期/释放校验 generation/token，更新校验 token。
- [x] 定向测试与真实 Redis/sandbox race。

## Task 2：权威工作区复制与发布

Files: 新建 `internal/sandbox/workspace_distributed.go`、回归测试；修改 `workspace.go`, `sync_lifecycle.go`, `lifecycle_coordinator.go`, handler/workspace.go。

Interfaces: `syncFromContainerSnapshot(ctx,sb,scoped,exclude)` 只用显式参数；旧 Docker wrapper 保留。Kubernetes Mount/Unmount/Sync 分支调用请求级权威实现；controller 独占 renewal，公开 info 只读 active。

- [x] 三 Manager 远端控制/FUSE info、durability 失败、回复丢失、cleanup、UID 替换和崩溃阶段回归。
- [x] 定向测试观察现场同类 RED，再 GREEN。
- [x] transition journal、请求 excludes 和最终同步 checkpoint；权威发布、单 controller 续租与精确解绑。读回不能代替副本确认，响应丢失重新 CAS 取得 ACK。
- [x] handler 检查 err/nil，返回稳定错误而不 panic。
- [x] 定向单测、三 Manager 真实 Redis和 race；恢复失败保留证据。

## Task 3：资源、stdin 和下载（独立子任务）

Files: `internal/sandbox/manager.go` 创建分支、新建资源 helper/test；`internal/api/handler/sandbox.go` 与解释器回归；`internal/runtime/kubernetes/file.go`, runtime.go 和测试；handler/file.go/test。

- [x] 资源池兼容、解释器 stdin/特殊代码、缺失下载 404/空文件 200/transport 错误与 runtime NotFound 回归。
- [x] 对应包定向 RED。
- [x] interpreter 独立 stdin、资源直接创建补默认、确认缺失错误映射；Docker FUSE 下载也覆盖。
- [x] handler/runtime/sandbox 单测与 race、独立复审。

## Task 4：one-shot durable cleanup

Files: `internal/api/handler/execute.go`, `internal/sandbox/active_store.go`, `lifecycle_coordinator.go`，handler/server 与 manager 回归。

- [x] 真实 HTTP SSE 完整结束、断开持久清理、服务停止和普通清理/最终删除失败接续回归。
- [x] 回归观察 RED。
- [x] ScheduleDestroy 持久 BeginDestroy 后交有界 tracked coordinator；Docker 同步 fallback；SSE 收尾清 deadline。FUSE worker 去重、取消和阶段恢复已补齐。
- [x] 定向/race 保证 HTTP 不等待 Pod 删除，durable evidence 不提前丢弃。

## Task 5：Cilium 内部白名单

Files: `internal/runtime/kubernetes/network.go`, ordinary_network.go/runtime.go，network tests；runtime/network.go，API error mapping；部署RBAC与网络说明。

- [x] 集群字面 CIDR 拒绝、Service selector 范围、失败事务和 supernet deny 交集回归。
- [x] network 定向 RED。
- [x] Service 目标、联合 attempt/UID/readback、权威范围有界发现/显式零扫描与 RBAC/配置贯通。
- [x] network/runtime/Helm 回归及独立 spec/quality PASS；现场连通性待验收。

## Task 6：FUSE 阶段性能证据

Files: manager Create FUSE、Kubernetes WaitSandboxReady、metrics、回归/bench。

- [x] 低基数阶段观测与 noop benchmark。
- [x] 保留 UID、mount/probe 和 PodReady，未根据少量样本擅改 readiness 等待。
- [ ] 部署后显式 30 次/并发采样 p50/p95/p99、完整错误率、UID 复用与 pool 开销。

## Task 7：审计与部署整改

Files: workspace coordinator审计接口/test，生产values示例和Helm验证脚本、部署说明。

- [x] 审计完整构造链无写入，非迁移 session 查询；归属/最终输出/终止不明均拒绝恢复。历史 Kubernetes owner 缺少 scope，因此工具不强行恢复；有 journal 的正常接续独立处理。
- [x] 生产 profile/Helm 拒绝内置或非 HA Redis、best_effort、LSM bypass/空/unconfined/disable，不改开发默认或线上。
- [x] 有界多副本 opt-in API 套件已编译验证；未提供 deployed URL 时明确 skip。
- [x] 副本确认的 Lua write/真实同 slot 屏障/WAIT 绑定同一 key master 连接，独立复审与回归；终态及幂等缺失确认闭合。
- [ ] 现场 HA、LSM、Cilium 和性能验收。

## Task 8：最终复审与验证交付

- [x] 对照设计逐项复审实际代码，不采信子任务成功声明；先spec review，再quality review。
- [x] `go test -p 1 ./...`、独占真实Redis、多 Manager/race、`go build ./...`、`go vet ./...`、newdiff lint、Helm lint/render/env唯一性。
- [x] 更新验证状态与交付清单，列出未完成现场验收和需构建镜像/Chart；不声称线上已修复。
