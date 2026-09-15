# Sentinel 升级可用性整改验证

## 整改目标

本次整改解决三个相关问题：

1. API 镜像 tag 更新不应改变 Redis StatefulSet PodTemplate 或重启 Redis；
2. FUSE refill 持有者异常退出后，不应留下约 42 分钟且必须人工删除的锁；
3. 新 API 在创建 Redis store、沙盒池和 HTTP 服务前，必须实时证明固定三成员 Sentinel
   拓扑、唯一 master、复制 lineage 和至少一个 replica ACK。

对应设计和执行计划：

- `docs/superpowers/specs/2026-09-15-sentinel-upgrade-availability-design.md`
- `docs/superpowers/plans/2026-09-15-sentinel-upgrade-availability.md`

## 已实现内容

- 新增 `docker/images/redis-bootstrap/Dockerfile`，bootstrap 镜像只包含
  `/app/redis-bootstrap` 和 CA 证书；API 镜像不再包含该命令。
- Helm 新增严格的 `redis.sentinel.bootstrapImage.repository/tag/pullPolicy`，所有
  built-in Sentinel helper 统一使用该镜像，不允许隐式回退到 API 镜像。
- FUSE refill 使用固定 45 秒 lease 和 15 秒续租；续租失败会取消本轮，原 token、CAS、
  revision、RuntimeUID 与发布 fencing 保持不变。
- API bootstrap gate 在同一 10 分钟预算内调用真实拓扑验证，成功后再次读取 identity
  projection；验证过程中身份变化会 fail closed。
- built-in Sentinel 的默认 startupProbe 总预算调整为 945 秒，覆盖默认 720 秒初始化、
  最多 45 秒孤儿 lease 恢复和 180 秒余量。

## 本地验证证据

2026-09-15 在仓库根目录执行并通过：

```bash
bash scripts/test-redis-bootstrap-image.sh
go build ./cmd/sandbox ./cmd/redis-bootstrap
go test ./cmd/sandbox ./internal/redisbootstrap ./internal/sandbox ./internal/storage/state/redis -count=1
go test -race ./cmd/sandbox ./internal/redisbootstrap ./internal/sandbox ./internal/storage/state/redis -count=1
go test -tags helmtests ./internal/helmtest -count=1
bash scripts/test-helm-chart.sh
bash scripts/test-helm-sentinel-auth.sh
bash scripts/test-built-in-redis-sentinel.sh
helm lint deploy/helm/sandbox
go vet ./cmd/sandbox/... ./cmd/redis-bootstrap/... ./internal/redisbootstrap/... ./internal/sandbox/... ./internal/storage/state/redis/...
golangci-lint run --new-from-rev c3f6fd2 ./cmd/sandbox/... ./cmd/redis-bootstrap/... ./internal/redisbootstrap/... ./internal/sandbox/... ./internal/storage/state/redis/...
```

真实三节点 Docker Sentinel fixture 也通过初始化、认证、master 终止、非零成员接管、旧主
恢复为 replica、Redis-only restart、冷恢复、更高 epoch、缺卷拒绝、两节点失败拒绝和三成员
IP replacement；测试结束后 fixture 容器、卷和网络均清理为零。

本机实际 Docker 镜像构建额外尝试两次，均在拉取 `docker/dockerfile:1` 的 Docker Hub
匿名 token 时被网络连接重置，尚未进入项目构建阶段。本限制不表示 Dockerfile 通过了实际
镜像层验收；上线构建流水线仍须完成下面的镜像内容检查：

```bash
docker run --rm --entrypoint /bin/sh "$BOOTSTRAP_IMAGE" -ec \
  'test -x /app/redis-bootstrap; test ! -e /app/sandbox; ! command -v docker'
docker run --rm --entrypoint /bin/sh "$API_IMAGE" -ec \
  'test -x /app/sandbox; test ! -e /app/redis-bootstrap; command -v docker >/dev/null'
```

## 线上首次迁移验收

首次采用独立 helper 镜像会触发一次预期的 Redis OrderedReady 滚动。它不是
“API-only 不滚动”验收轮次；应先确认始终保有 Sentinel quorum，等待三成员全部 Ready。

迁移完成后记录 Redis StatefulSet 的 `currentRevision`，以及三个 Redis Pod 的 UID 和
restartCount。随后只改变 API `image.tag` 再执行一次 upgrade，验收标准是：

- Redis StatefulSet PodTemplate 与 `currentRevision` 不变；
- 三个 Redis Pod UID、restartCount 不变；
- API Deployment 按 `maxUnavailable: 0`、`maxSurge: 1` 完成滚动；
- 新 API 在拓扑未完整时保持未 Ready，旧 API 持续服务；
- 拓扑完整后新 API 通过启动门禁并正常提供沙盒 API；
- 注入孤儿 refill lease 后，无人工删除 Redis key，最迟约 45 秒由新持有者接管；
- 无双 writer、重复 reservation、错误 RuntimeUID 发布或遗留测试资源。

`WAIT 1` 只表示至少一个 replica 确认写入，不是共识提交或零数据丢失保证。线上仍须保留
可靠持久卷、备份、恢复演练和 Sentinel 故障演练。
