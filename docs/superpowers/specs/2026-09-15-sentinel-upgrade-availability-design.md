# Sentinel 升级可用性与 FUSE refill lease 设计

## 背景与目标

2026-09-15 在 `ds-ai-research` 将 sandbox-api 从 v0.3.25 升级到 v0.3.26 时，
内置 Redis Sentinel 的辅助容器复用了全局 API 镜像。API tag 变化因此改变 Redis
StatefulSet PodTemplate，触发三个 Redis/Sentinel Pod 滚动重启。切主窗口内，一个
FUSE refill 锁已写入 Redis，但复制 ACK 和后续解锁未得到确认；持锁进程退出后，
该锁按 `prepareTimeout * (maxSize + 1)` 保留约 42 分钟。新 API 在初始 FUSE
reconciliation 中持续等待，最终只能通过核对并删除精确孤儿锁恢复。

本设计一次关闭三个相互关联的问题：

1. API-only 升级不再改变 Redis StatefulSet PodTemplate；
2. refill 控制器异常退出后能够在短时间内自动重新选主，同时保留完整 fencing；
3. 新 API 只有在真实 Redis/Sentinel 三成员拓扑和写入 ACK 恢复后才初始化运行时池，
   RollingUpdate 期间继续由旧 API 提供服务。

不修改 Redis 数据格式、PVC/身份 Secret 所有权、Sentinel 选主协议、工作区租约、
运行时 UID 契约或业务 API。也不增加 API 副本间 HTTP 转发。

## 方案选择

### 采用：独立配置、复用现有 sandbox-api 镜像内容

新增 `redis.sentinel.bootstrapImage`，包含必填的 `repository`、`tag` 和
`pullPolicy`。镜像仍可指向现有 sandbox-api 仓库，因为其中已经包含
`/app/redis-bootstrap`；但 tag 独立固定，只在 redis-bootstrap 程序本身需要升级时
修改。首次升级应填写当前 Redis PodTemplate 已使用的 sandbox-api 镜像版本，避免
因为引入新字段而产生一次无意义 Redis 滚动。

所有直接从镜像执行 `/app/redis-bootstrap` 的资源使用 bootstrapImage：

- Sentinel StatefulSet 的 `prepare` initContainer；
- Sentinel StatefulSet 的 restartable `identity` init sidecar；
- identity Secret 创建/核验 Job；
- Sentinel initialize Job；
- rollback guard Job。

Redis 和 Sentinel 主进程仍使用 `redis.image`，它们从 `prepare` 写入的受保护
`emptyDir` 执行 `/redis-tools/redis-bootstrap`。API Deployment 继续使用全局
`image`。修改 API tag 必须只改变 API Deployment；修改 bootstrapImage 或
`redis.image` 才允许改变 Redis StatefulSet。

未采用单独构建精简 bootstrap 镜像，因为当前没有降低镜像体积或独立供应链的硬性
需求，引入第三种构建产物会增加发布和多架构维护成本。未来可在不改变 values 契约的
前提下替换 repository。

未采用 Helm hook 串行控制所有工作负载。Hook 无法代替进程对 Redis 身份、拓扑和
写入 ACK 的证明，失败/重试还会增加 release 状态和保留资源的复杂度。

## bootstrapImage 配置契约

默认 values 提供完整结构，但 repository/tag 为空；standalone 和 external Redis
忽略该配置，built-in Sentinel 模式必须显式填写非空合法值。空值不回退到全局 API
镜像，避免后续 API tag 变化重新引入隐式耦合。

```yaml
redis:
  sentinel:
    bootstrapImage:
      repository: registry.i.huaxisy.com/library/ai-infra/sandbox-api
      tag: v0.3.26
      pullPolicy: IfNotPresent
```

repository 和 tag 分开渲染为 `repository:tag`。本批不增加 digest 字段；部署方可以在
repository 中使用现有仓库地址，并遵守不可覆盖已发布 tag 的规则。pullPolicy 仅允许
Kubernetes 支持的 `Always`、`IfNotPresent`、`Never`。

Helm schema、模板验证、默认 values 中文注释和部署文档必须同步。缺失或类型错误在
创建 Kubernetes 资源前失败，错误信息不得暗示回退到 API 镜像。

## FUSE refill lease

### 时限

refill 分布式锁改为固定 45 秒 renewable lease，续租周期为 15 秒。该时限是内部安全
契约，不开放 values 调节。`PrepareTimeout` 继续只约束单个 prepared runtime 的创建，
不再用于计算控制器 lease TTL；`MaxSize` 也不再放大失效控制器的恢复时间。

正常 reconciliation 无论持续多久，都由独立续租 goroutine 保持 lease。进程退出、
网络中断、Redis 切主、token 被替换或任何续租错误都会取消本轮上下文。新控制器只有
在 Redis 按服务器时间确认旧 lease 到期后才能获得新 token，恢复上限从约 42 分钟
缩短为约 45 秒，不依赖人工删除 key。

### fencing 与不确定结果

缩短 TTL 的安全前提沿用现有 Redis 原子协议：

- `CreatePreparingWithAdmission`、`BindPreparingRuntime`、prepared/reserved 发布等
  refill 写入必须同时验证当前 lock token；
- 每条 pool record 继续验证 `PreparationID`、state、revision、RuntimeID/RuntimeUID
  和对应 transition token；
- 续租在 lease 的前三分之一触发，失败后立即取消 reconciliation，剩余约三分之二
  TTL 为在途调用停止和补偿留下安全窗口；
- 丢失 lease 的旧控制器不能发布 prepared/reserved，也不能删除属于新 revision 或
  新 RuntimeUID 的运行时；
- Kubernetes 创建已成功但 Redis bind/publish 未确认时，继续通过 intent record、
  精确 RuntimeRef 和 cleanup tombstone 恢复，不能裸删 Redis record 或猜测 Pod；
- unlock 的响应不确定不改变 lease 到期语义。token 不匹配只表示另一个生命周期已经
  接管，不能删除对方的锁。

续租 Redis 调用继续受现有客户端 I/O timeout 和父上下文约束。本批不在 lock value
中加入 Kubernetes Pod 查询或进程心跳；API 进程是否存在不能替代 Redis token fencing。

## API 启动拓扑门禁

### 当前缺口

`waitForRedisBootstrap` 当前只等待受信 ConfigMap/Secret projection 中的 phase 变为
`Initialized`，校验配置的三个 Sentinel 地址，但没有调用已经存在的
`redisbootstrap.VerifyTopology`。`WaitBootstrapInitialized` 的接口注释也明确要求
调用方在业务写入前另行验证认证的 HA/ACK 契约。因此历史初始化状态不能证明本次
Redis 滚动后当前 master、replica、Sentinel quorum 和复制 ACK 已恢复。

### 新流程

非 cleanup 的 built-in Sentinel API 启动依次执行：

1. 在现有 10 分钟有界上下文内等待 bootstrap projection 为 `Initialized`；
2. 读取并固定 cluster、registration 和三成员公钥，确认配置中的三个 Sentinel 地址
   与固定成员 DNS 一一对应；
3. 调用 `redisbootstrap.VerifyTopology`，使用 `all=true`，要求三个身份固定成员全部
   可达；验证唯一 master、Sentinel 证据/epoch、Redis runID、复制 lineage、replica
   链路和 quorum；
4. 向已证明的 master 写入短期随机 barrier，并要求 `WAIT 1` 在配置的 ack timeout 内
   成功；
5. 成功后重新读取 bootstrap projection，必须与本轮固定的 cluster、registration 和
   公钥一致，关闭验证期间 projection 替换窗口；
6. 只有上述检查全部成功，才创建状态 store、初始化普通/FUSE 池并启动 HTTP 服务。

拓扑暂时不满足时，在同一个启动进程内有界重试，不输出密码、challenge、proof 或
完整 Redis 响应。上下文取消、10 分钟预算耗尽、身份变化或配置契约不一致均 fail
closed。drain/audit/recovery 命令继续绕过业务启动门禁，避免初始化不完整时无法执行
既有安全清理；它们仍受各自 Redis 状态契约约束。

### RollingUpdate 可用性

API Deployment 保留：

```yaml
strategy:
  type: RollingUpdate
  rollingUpdate:
    maxUnavailable: 0
    maxSurge: 1
```

新 API 在拓扑门禁或初始池 reconciliation 中尚未通过时不会 Ready，Deployment 因此
不会删除下一份旧 API Pod。无需 API 间转发。API-only 升级不会滚动 Redis；真正修改
Redis/bootstrap 镜像时，StatefulSet 继续使用 OrderedReady 逐成员滚动，Redis 与
API 同时被 Helm 更新也由新 API 的进程门禁阻止过早初始化。

Redis-only 修改不强制滚动已经健康的 API Deployment：稳态 Sentinel 客户端负责处理
正常切主，避免为了验证而主动制造 API 重启。下一次 API 启动会执行完整门禁；
initialize Job 仍在每个 Helm revision 对当前三成员拓扑进行独立核验。

## 错误处理与观测

- API 日志只报告固定阶段和稳定错误分类，不输出 Redis 密码、公钥证明正文、网络
  响应或 lock token。
- 拓扑门禁可记录开始、重试和完成阶段；重复失败必须限频，不能每秒刷 ERROR。
- refill lease 续租失败保留现有 fail-closed ERROR，并在指标中归类为 lease loss；
  正常竞争和等待不记为 ERROR。
- startupProbe 继续屏蔽门禁期间的 liveness。现有 built-in Sentinel 自动预算不得因
  本批缩短；测试必须证明默认预算覆盖 10 分钟门禁、45 秒孤儿 lease 恢复和既有启动
  余量。

## 测试与验收

### Helm

- 同一 bootstrapImage 下只改变全局 API tag：API Deployment image 改变，Redis
  StatefulSet `spec.template` 完全不变；identity/initialize/rollback guard 仍使用固定
  bootstrapImage。
- 改变 bootstrapImage tag：Redis StatefulSet 的 helper 容器发生预期变化；Redis
  数据容器 image 不被错误替换。
- built-in Sentinel 缺失、空白或非法 bootstrapImage 时渲染失败；standalone/external
  行为保持不变。
- Deployment 保持 `maxUnavailable=0`、`maxSurge=1`，startupProbe 默认预算满足上述
  下限。

### refill lease

- 旧实现下会得到 `PrepareTimeout * (MaxSize + 1)` 的失败测试先证明 42 分钟问题；
  实现后默认 TTL 精确为 45 秒、续租周期为 15 秒。
- 长时间 refill 在多轮成功续租后仍保持单 writer。
- 续租错误、token 丢失和上下文取消都会停止旧 writer；推进 Redis server time 到
  lease 到期后，新 controller 自动取得锁并完成初始 reconciliation。
- lease 丢失后的延迟 bind/publish 被 Redis token/revision fencing 拒绝；已创建 runtime
  进入精确补偿/恢复，不被发布为可领取实例。
- Redis repository 的真实脚本测试覆盖 TTL、续租、过期接管、错误响应和耐久性不确定。

### API 门禁与 Sentinel

- 静态 Initialized 但拓扑 verifier 失败时，HTTP/池初始化不得开始；有界重试后成功
  才返回。
- verifier 必须收到 `all=true`、固定 registration/public keys、masterName、两套独立
  密码和正确 ack timeout。
- 验证成功后 projection 改变必须 fail closed；cleanup/audit 路径保持可执行。
- 真实三成员 Sentinel fixture 覆盖 master 终止、非零成员接管、写入复制、旧主以
  replica 恢复、初始化核验和冷恢复。
- Helm 模板、配置、Go 单元/集成、race、build、vet 和增量 lint 全部通过后才能发布。

### 部署后

首次线上 values 将 bootstrapImage 固定为升级前 Redis StatefulSet 已使用的
`sandbox-api:v0.3.26`，新 API 使用新 tag。升级前后核对 Redis StatefulSet
`updateRevision/currentRevision`、Pod UID 和 restartCount 均不变；API 逐副本 Ready。

再在隔离维护窗口做一次受控 Sentinel 单成员故障/切主：旧 API 持续提供健康与已有
业务访问，新 API 在拓扑未恢复三成员前不初始化池，恢复后自动 Ready；人为制造持锁
进程退出后不删除 Redis key，确认 45 秒边界内自动取得新 lease。最终审计 session、
workspace、普通/FUSE pool、Pod 和策略，没有活动测试残留。

## 明确不做

- 不让 API 副本通过 HTTP 转发生命周期请求；
- 不使用 `kubectl delete pod --force`、手工 DEL 或 Helm rollback 作为自动恢复流程；
- 不把 `WAIT 1` 描述为共识、同步落盘或零数据丢失；
- 不更换 Sentinel 身份 Secret、clusterID、PVC 或成员 DNS；
- 不顺带优化普通沙盒 30 秒终止和 FUSE 挂载阶段延迟；
- 不借本批清理全仓历史 lint 欠账。
