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

## 后续优化候选

以下只记录问题与验收边界，尚未形成批准的实现规格：

1. 将 Redis bootstrap/identity 辅助镜像从 sandbox-api 镜像配置中解耦，单独固定
   repository、tag 和 pullPolicy。只升级 API 时 Redis StatefulSet revision 必须不变；
   只有辅助程序本身改变时才允许按维护流程滚动 Redis。
2. 重新设计可续租 refill 锁的首个续租周期和孤儿恢复上限。Redis 故障后不应等待
   约 42 分钟，也不能靠人工删除 Redis key；缩短恢复窗口时仍须保留 token、CAS、
   RuntimeUID 和所有发布操作的 fencing，不能产生两个有效 refill writer。
3. 明确内置 Sentinel 与 API 的升级编排：Redis 未恢复三节点 Ready、Sentinel quorum
   和可写 master 前，不启动新的 API 池初始化；同时保留旧 API 的服务可用性。
4. 增加真实 Sentinel 滚动/切主测试，覆盖“锁写入成功但 ACK/解锁未确认”、API
   startupProbe 预算和自动恢复，验收目标是不需要手工修改 Redis 状态。

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
