# 独立外部 Sentinel 认证夹具

运行：`bash scripts/test-redis-sentinel-auth.sh`。要求本机已经缓存 `redis:7-alpine`、`golang:1.25-alpine` 以及 Go module cache；不自动拉镜像、不构建镜像、不读取 `.env`。

可选清理负例：`bash scripts/test-redis-sentinel-auth.sh --verify-failure-cleanup`。该模式刻意让自有 test-runner 退出 42，脚本应保留退出码 42、确认自有资源清理为零，并且不输出认证 PASS；它不是认证验收运行。

夹具使用公开 dummy 常量，分别创建 Redis `data-user` 与 Sentinel `sentinel-user`，密码互异并包含空白、双引号、反斜杠和 Unicode。生成器实际调用 `redisbootstrap.QuoteConfigValue`，私有配置为 0600；不使用密码 CLI 参数或输出密码。

独立 Compose project/网络内有三个 Redis、三个 Sentinel 与六个自有持久卷，无宿主端口。生成 Go 容器无网络；测试 Go 容器使用同一 fixture 网络，从而可访问 Sentinel 返回的稳定 `redis-0` DNS。仓库/module cache 只读，构建 cache 仅在临时容器 `/tmp`。

验证 Redis 复制、Sentinel 访问 Redis、客户端访问 Sentinel、Sentinel peer 认证；每个端点拒绝无认证/另一端密码，生产 `Store.New` 拒绝错 Sentinel 或数据密码。正确 Store replica_ack Set/Get、两个副本读回和实际写入同连接 WAIT 1 均需通过。

脚本每次运行生成不可覆写的唯一 project，先确认没有同 project 资源，退出前验证精确 project/service/name，再 `down --volumes` 清理仅自有资源并确认三个资源列表为空。仅删除六个明确的临时配置文件，不 prune、不操作业务对象。故障时保留非零退出码；目标验证/清理失败不会声称通过。

Go integration test 默认跳过；只接受 `TEST_REDIS_SENTINEL_AUTH_ADDRS=sentinel-0:26379,sentinel-1:26379,sentinel-2:26379`，没有读取真实凭据的入口。此测试不是 built-in Chart、初始化身份授权、failover/IP replacement、Kubernetes 或生产 HA 验收。
