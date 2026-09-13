# Redis 显式连接模式修正计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 防止 go-redis 按地址数量/masterName 推断覆盖已声明的 standalone/cluster 模式。

**Architecture:** 保留 UniversalOptions 及全部认证/ACK 映射，显式 cluster 设置 IsClusterMode；standalone/cluster 的残留 masterName 作为配置冲突拒绝，不静默接入 Sentinel。检查只涉及构造器与配置验证，不访问线上。

**Tech Stack:** go-redis v9.18.0、Go 单测、Helm 验证。

---

### Task 1: 构造器模式回归

**Files:** Modify `internal/storage/state/redis/store.go`, `options_test.go`.

- [x] 新增以下单测并执行 `go test ./internal/storage/state/redis -run 'TestUniversalOptions(ExplicitCluster|RejectsConflicting)' -count=1`，预期当前返回 Client 而非 ClusterClient、冲突不报错 RED：

```go
opts, err := universalOptions(Options{Mode: ModeCluster, Addr:"seed:6379"})
require.NoError(t, err)
client := goredis.NewUniversalClient(opts)
t.Cleanup(func(){ _ = client.Close() })
require.IsType(t, &goredis.ClusterClient{}, client)
```

- [x] 实现 `IsClusterMode: mode == ModeCluster`；在非 Sentinel 模式且 MasterName 非空时返回 `redis: master name is only valid in sentinel mode`。
- [x] 重跑聚焦及包/race 测试，预期 PASS；原 cluster tuning expected options 增加 `IsClusterMode:true`。

### Task 2: 配置/Chart 同源拒绝冲突

**Files:** Modify `internal/config/config.go`, `templates/_helpers.tpl`; Test `internal/config/sentinel_auth_test.go`, `scripts/test-helm-sentinel-auth.sh`.

- [x] 对 standalone/cluster + 非空 masterName 增加解析及 Chart 失败测试，预期当前接受 RED。
- [x] 在两层已有模式验证中加入 `mode != sentinel && masterName != empty` 明确错误；不更改无冲突配置。
- [x] 执行配置单测、Chart/auth/backend-switch 脚本和全量 Go/race/vet，预期 PASS。记录改动是现有模式推断缺陷修正，不承诺真实 Redis Cluster 故障实验已完成。

## 原因证据

已检查本地固定版本 go-redis `universal.go:375`：MasterName 优先选择 FailoverClient；只有 Addrs>1 或 IsClusterMode 才创建 ClusterClient。此前声明 cluster 且仅一个发现地址会被构造为普通 Client，不能正常处理 MOVED。

## 实施验证

构造器、native config、Helm 冲突各自先观察到行为 RED；修正后包测试及新旧 Chart 脚本通过。独立只读复审核对 pinned go-redis 选择顺序及未修改的同连接 replica_ack 路径，未确认 P1/P2。最终全量 Go/race/vet 与增量 lint 通过，完整记录见 `docs/testing/2026-09-13-apparmor-sentinel-implementation-review.md`。若直接使用 Store Options，Sentinel 必须显式声明 mode，不再由残留 masterName 静默覆盖 standalone/cluster。
