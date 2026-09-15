# API 升级触发内置 Redis Sentinel 重启记录

## 现象与影响

2026-09-15 在 `ds-ai-research/aiadp-sandbox-fuse` 将 sandbox-api 从
v0.3.25 升级到 v0.3.26 时，Redis 镜像、认证配置和数据均未改变，但
`sandbox-fuse-redis-sentinel` StatefulSet 的三个 Pod 仍发生滚动重启。
Redis 切主窗口内，新 API 曾因状态持久化确认失败退出；随后又被一个未续租的
FUSE refill 锁阻塞，Deployment rollout 无法继续。

旧的三个 API 副本在新副本启动期间保持可用。确认孤儿锁没有续租且原持有者已因
Redis 故障退出该轮 reconciliation 后，只删除了这个精确锁。新 API 随即取得新锁，
完成 FUSE 池检查并启动；最终三个 v0.3.26 API 副本均 Ready，Deployment rollout
完成。没有删除 Redis 数据、PVC、工作区或沙盒业务状态。

## 根因

Redis StatefulSet 的 `prepare` initContainer 和 `identity` sidecar 都通过
`sandbox.apiImage` 使用全局 `image.repository/image.tag`。因此只修改 API tag 也会
改变 Redis PodTemplate 哈希，Kubernetes 必然滚动更新 StatefulSet。每次 release
revision 生成的 `identity` 和 `initialize` 一次性 Job 也使用相同 API 镜像。

故障窗口内，某个 FUSE reconciliation 已成功写入 refill 锁，但 Redis 切主使
replica ACK 和后续解锁未得到确认。锁的当前 TTL 按
`prepareTimeout * (maxSize + 1)` 计算；本环境为 120 秒 × 21，即约 42 分钟。
持锁 reconciliation 已退出，锁不再续租，但新 API 的初始 reconciliation 会等待
该锁，而 startupProbe 的单次启动预算更短，于是容器被探针反复重启。

## 已实施整改

本地实现已完成以下整改，线上仍需使用新镜像和 Chart 做首次迁移及 API-only 二次升级
验收：

1. `7e1fc82` 新增独立最小 `sandbox-redis-bootstrap` 镜像，并从 API 镜像删除 helper
   命令；`6da6e9f` 让所有 built-in Sentinel helper 使用独立 repository/tag/pullPolicy。
   迁移完成后 API-only tag 变化不再改变 Redis PodTemplate。
2. `0204bba` 将 refill 锁改为固定 45 秒 lease、15 秒续租；续租失败取消本轮，原有
   token、revision、RuntimeUID 和发布 fencing 保持不变。持锁进程消失后不再需要人工
   DEL，旧 lease 最迟约 45 秒到期。
3. `6b5511e` 在 API 创建 store、池和 HTTP 服务前调用真实三成员拓扑验证，固定身份
   公钥，证明唯一 master、Sentinel quorum、复制 lineage 和 `WAIT 1` ACK；验证后
   projection 改变会 fail closed。Deployment 保持 `maxUnavailable: 0`、`maxSurge: 1`。
4. 本地真实 Sentinel fixture 已覆盖初始化、主节点终止、非零成员接管、旧主恢复为
   replica、冷恢复、三成员 IP replacement、复制和 ACK，测试资源最终清理为零。

首次采用独立 helper 镜像会有一次预期 Redis OrderedReady 滚动；不能把这次迁移误报
为“Redis 完全不动”。迁移完成后还须执行一次只改变 API tag 的现场 upgrade，确认
StatefulSet revision、三个 Redis Pod UID 和 restartCount 均保持不变，并受控验证孤儿
refill lease 在 45 秒边界内自动接管。

## 一次性 Job 与 Completed Pod

`sandbox-fuse-redis-sentinel-identity-<revision>` 负责创建或核验不可变身份 Secret，
`sandbox-fuse-redis-sentinel-initialize-<revision>` 负责核验并初始化 Sentinel 状态。
两者成功完成后不再提供运行时服务。Chart 当前设置
`ttlSecondsAfterFinished: 86400`，对应 Job 和 Pod 会在完成约 24 小时后自动回收。

旧 revision 且状态为 `Complete` 的 Job 可以提前删除，删除 Job 会一并删除其
Succeeded Pod，不影响当前 Redis/Sentinel、身份 Secret、状态 ConfigMap 或 PVC。
不要删除仍在运行/失败待排查的 Job，也不要用模糊标签批量删除当前 revision；先用
`kubectl get jobs` 确认精确名称和完成状态。保留最新一代 Completed Job 到 TTL 到期，
有助于升级后审计启动结果。
