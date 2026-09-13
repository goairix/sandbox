# Sentinel 独立认证配置实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 将 Redis 数据节点及 Sentinel 的独立凭据从 YAML/环境变量完整传至客户端，默认认证行为不变。

**Architecture:** 配置增加两个显式字段；main 使用可单测的纯 options 映射函数。Redis 客户端 builder 和 Helm 认证由独立任务处理；此计划不启用尚未完成安全启动协议的内置 Sentinel。

**Tech Stack:** Go 1.25、Viper、go-redis v9。

---

### Task 1: 配置解析

**Files:** Modify `internal/config/config.go`, `configs/config.yaml`; Create `internal/config/sentinel_auth_test.go`.

- [x] 写测试：环境变量 `SANDBOX_STORAGE_STATE_REDIS_SENTINEL_USERNAME=sentinel-user`、`SANDBOX_STORAGE_STATE_REDIS_SENTINEL_PASSWORD=sentinel-secret` 与数据用户名/密码独立，YAML 同样传递，默认空。
- [x] 执行 `go test ./internal/config -run TestSentinelAuth -count=1`，预期新增字段缺失导致 RED。
- [x] RedisConfig 增加 `SentinelUsername string mapstructure:"sentinel_username"`、`SentinelPassword string mapstructure:"sentinel_password"`；两字段分别 `v.SetDefault("storage.state.redis.sentinel_username", "")`、`v.SetDefault("storage.state.redis.sentinel_password", "")`，不设置凭据回退；config.yaml 增加相应空值和中文解释。
- [x] 重跑同一命令，预期 PASS。

### Task 2: 完整映射

**Files:** Modify `cmd/sandbox/main.go`; Create `cmd/sandbox/redis_config.go`, `cmd/sandbox/redis_config_test.go`.

- [x] 测试 `redisOptionsFromConfig(config.RedisConfig{Username:"data", Password:"data-secret", SentinelUsername:"observer", SentinelPassword:"observer-secret", AckTimeoutMS:123})` 返回独立四凭据和 `AckTimeout==123*time.Millisecond`，检查所有已有 mode/addrs/master/db/durability/pool/timeout/retry 字段不丢失。
- [x] 执行 `go test ./cmd/sandbox -run TestRedisOptionsFromConfig -count=1`，预期函数缺失 RED。
- [x] 将 main 原完整 inline options 原样移入 `func redisOptionsFromConfig(c config.RedisConfig) redisstate.Options`，额外显式映射 `SentinelUsername:c.SentinelUsername, SentinelPassword:c.SentinelPassword`。main 改为 `redisstate.New(ctx, redisOptionsFromConfig(cfg.Storage.State.Redis))`。Addrs 复制为 `append([]string(nil), c.Addrs...)`。
- [x] 重跑同一命令，预期 PASS；执行 `go test ./internal/config ./cmd/sandbox ./internal/storage/state/redis -count=1`、`go test -race ./internal/config ./cmd/sandbox ./internal/storage/state/redis -count=1` 和 `git diff --check`，预期全通过。
- 该项实现及复审已完成；由父任务随本轮验证后的代码批次统一提交，不单独部署或构建镜像。

## 范围自审

这是一项可独立验证的配置链路，不声称内置 Sentinel Chart、安全引导、故障恢复或真实网络认证已完成。任何错误不得输出密钥；原有 ACK/CAS/fencing 与 HA 验证逻辑保持不变。
