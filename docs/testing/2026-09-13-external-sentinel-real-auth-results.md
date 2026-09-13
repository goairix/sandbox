# 独立外部 Sentinel 真实认证验证结果

## 结论与范围

真实隔离 Redis/Sentinel 夹具验证通过：独立数据节点与 Sentinel ACL 用户/特殊字符 dummy 密码可用于四条认证链路，生产 `redis.New` 与 Options builder 可发现稳定 DNS 主节点并执行 replica_ack 写入及实际写入同连接 WAIT 1。

这不是 built-in Sentinel Chart、初始化身份协议、故障切换、冷恢复、IP replacement、Kubernetes 或生产 HA 验收。没有修改生产/Helm代码、原已提交 feasibility 夹具；没有读取真实 keys 或 `.env`、构建/拉取镜像、操作集群、提交代码。

## 夹具与隔离

- Redis 缓存镜像 `redis:7-alpine`，实际输出 `Redis server v=7.4.11`；缓存 Go 镜像 `golang:1.25-alpine` 元数据 `GOLANG_VERSION=1.25.14`。
- 三个 Redis、三个 Sentinel；只有独立 Compose 网络，无宿主机端口；六个自有命名卷分别保存 Redis AOF/配置及 Sentinel 可写状态。
- 公开 test-only `data-user` 和 `sentinel-user`，不同 dummy 密码均包含空格、双引号、反斜杠和 Unicode。默认 ACL 用户关闭，无认证必须失败；不输出密码或原始配置。
- 生成器实际调用 `internal/redisbootstrap.QuoteConfigValue`，包括整个 ACL `>` 参数；私有配置写入模式 0600，复制到各自卷后由官方 Redis entrypoint 启动。没有手写部分密码 shell quoting。
- 生成 Go 容器 network=none；测试 Go 容器使用同一独立网络。仓库与 host GOMODCACHE 只读，GOPROXY=off、GOTOOLCHAIN=local、GOFLAGS=-mod=readonly；构建 cache 仅在临时容器 `/tmp`。Compose 强制 `--env-file /dev/null`。
- Go 测试仅接受专用地址 `sentinel-0:26379,sentinel-1:26379,sentinel-2:26379`；未配置专用 env 时跳过，其他非空地址在任何拨号前拒绝。

## 四条认证链路的真实证据

| 链路 | 配置与实测结果 |
| --- | --- |
| Redis 复制 | replica masteruser/data password 独立配置；主 INFO 有两个 online 副本，两从 master_link_status=up；生产 Store 写入值被两个副本实际读回。 |
| Sentinel 访问 Redis | Sentinel auth-user/auth-pass 使用 data credentials；三个 Sentinel MASTER 均无 disconnected/s_down/o_down、报告两个健康副本，REPLICAS 链路为 ok。发现并刷新复制拓扑需要成功执行认证后的 Redis INFO/PING。 |
| 客户端访问 Sentinel | Sentinel ACL 只开放独立 sentinel-user/password；正确生产 Options 中 SentinelUsername/SentinelPassword 使 `redis.New` 发现 `redis-0:6379`，无认证/另一端密码均拒绝。用户名字段也实际参与认证，不仅测试默认用户密码。 |
| Sentinel peer | sentinel-user/sentinel-pass 使用独立 Sentinel credentials；三个 Sentinel 各报告两个稳定 sentinel-N DNS peer、无 disconnected/down、last-ok-ping-reply<5000ms，CKQUORUM 均有 `OK 3 usable Sentinels`。不是只凭 Redis hello 消息发现 peers。 |

## 否定与写入检查

- 六个 Redis/Sentinel 端点都拒绝无凭据 PING，以及使用另一端密码的 PING；响应为 NOAUTH/WRONGPASS。
- 生产 `redis.New` 使用错误 Sentinel 密码失败，返回包含 WRONGPASS 的认证错误，而不是把连接超时当作密码验证证据。
- 使用正确 Sentinel 认证但错误数据节点密码，生产 `redis.New` 同样返回 WRONGPASS；两端密码没有回退替代。
- 正确 `redis.New(Options{Mode: sentinel, Durability: replica_ack, AckReplicas: 1, AckTimeout: 3s, ...})` 成功；生产 `Store.Set/Get` 成功，两个 direct replica 最终读回相同值。
- 从生产创建的 client 取得 dedicated Conn，在同一连接执行 fresh SET 再 WAIT 1 3000；三次正常运行分别返回 2、1、1，均满足 >=1。这不是 fresh connection offset=0 或仅只读检查后的 WAIT。
- WAIT 是异步复制确认，以上结果不构成共识或绝对零数据丢失保证。

## 精确运行结果

命令：`bash scripts/test-redis-sentinel-auth.sh`。

| 唯一 project | Go integration 结果 | 同连接 WAIT 1 | 完整脚本与清理 |
| --- | --- | --- | --- |
| sandbox-sentinel-auth-20260913183140-35566-18803 | PASS 5.591s | 2 | exit 0；自有容器/六卷/网络清理为零 |
| sandbox-sentinel-auth-20260913183340-35753-10819 | PASS 5.858s | 1 | exit 0；自有容器/六卷/网络清理为零 |
| sandbox-sentinel-auth-20260913183433-35951-10162 | PASS 4.643s | 1 | exit 0；自有容器/六卷/网络清理为零 |

清理负例命令：`bash scripts/test-redis-sentinel-auth.sh --verify-failure-cleanup`。

- 独立 project `sandbox-sentinel-auth-20260913183513-36426-31137` 的自有 test-runner 刻意退出 42。
- 完整脚本保留 exit 42，trap 输出 `fixture cleanup: zero project containers, volumes and networks`，没有 `real external Sentinel auth tests: PASS`。
- 这是错误结果也执行精确清理的测试，不是一次认证验收 PASS。

脚本只接受不可覆写的本次生成 project。清理前检查 project/service/container name、六个精确卷名及唯一网络名；未知目标或任何 Docker inventory 查询失败均拒绝成功声明。`down --volumes` 不带 broad prune/down-all，不删除镜像；仅删除六个明确的 mktemp 配置文件后 rmdir 自有目录。正常和强制失败结果均精确清理实验资源；未删除业务数据。

## TDD 与本机回归边界

新 Go integration test 先于 fixture 实现编写。已有独立凭据映射已经 GREEN，按授权未回退生产或其他代理代码制造 RED；真实否定检查先于 positive Store 写入执行，观察到 NOAUTH/WRONGPASS 作为 fixture 认证正确性证据。

未设置专用 env 的新测试确认 SKIP，不把此结果称为真实认证通过。随后本机限定旧 builder/新 env-gated test 的检查通过：

```bash
TEST_REDIS_SENTINEL_AUTH_ADDRS= TEST_REDIS_ADDR= go test ./internal/storage/state/redis \
  -run '^TestSentinelAuthIntegration$|^TestUniversalOptions(SentinelCredentials|DefaultStandalone|ConnectionTuning|StandaloneDatabase|TopologyValidation)$' -count=1 -v
# PASS 0.720s；real integration SKIP

TEST_REDIS_SENTINEL_AUTH_ADDRS= TEST_REDIS_ADDR= go test -race ./internal/storage/state/redis \
  -run '^TestSentinelAuthIntegration$|^TestUniversalOptions(SentinelCredentials|DefaultStandalone|ConnectionTuning|StandaloneDatabase|TopologyValidation)$' -count=1
# PASS 1.624s；real integration SKIP
```

以上 race 只覆盖本机确定性测试，未声称在真实 Redis fixture 内执行 race。中途使用更宽 `^TestUniversalOptions` 还捕获主任务正在新增的 single-seed Cluster/conflicting MasterName RED；这些并行生产/测试片段不属于本子任务，未修改，也不宣称当时完整 Redis package 通过。

主任务完成该独立 mode 修正后，本子任务收尾复核 `TEST_REDIS_SENTINEL_AUTH_ADDRS= TEST_REDIS_ADDR= go test ./internal/storage/state/redis -count=1` PASS 2.527s，相同 env 下 `go test -race ./internal/storage/state/redis -count=1` PASS 2.467s。真实 integration 仍按设计 SKIP；历史并行 RED 不再是该最终本机 package 结果。

`bash -n scripts/test-redis-sentinel-auth.sh`、gofmt 与 `git diff --check` 通过。交付可执行脚本、testdata 夹具、env-gated Go integration test 和子计划/本报告，供主任务继续审查；无提交。
