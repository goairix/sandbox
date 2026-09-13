# Helm 内置 Redis Sentinel 设计

## 已确认需求与范围

用户没有现成 HA Redis，要求 Chart 增加内置 Sentinel，并计划卸载旧 release 后重新安装。镜像构建、线上 uninstall/install 由用户执行，本轮不操作当前业务 Redis、不删除 PVC，也不提供无证据在线迁移。

选择 Chart 原生 StatefulSet + Redis 自带 Sentinel，不要求用户先部署外部集群。与单实例相比提供自动主从切换；与 Cluster 相比不引入尚无需求的数据分片和六实例拓扑。外部 standalone/sentinel/cluster 路径继续保留，内置 Cluster 不在范围。

## 拓扑与部署

- 新增 `redis.mode: standalone | sentinel`，默认 standalone，保留现有内置单实例行为。
- Sentinel 模式固定三个 Redis/Sentinel Pod，每 Pod 一个 Redis 和一个 Sentinel，正常形成 1 主 2 从及 3 Sentinel，quorum=2。
- StatefulSet 使用 Parallel 启动，不能使用要求前一个 Redis Ready 才启动下一个的默认顺序，避免首次多数派形成死锁。
- 三 Pod 按 hostname 必须分散到不同工作节点；不足三个合格节点时 Pending，不把同节点多副本描述为高可用。配置 PDB 保留至少两个可用实例，解释其只限制主动驱逐、不保证真实节点故障无中断。
- 内部 headless Service 提供稳定的每 Pod DNS，publishNotReadyAddresses 用于启动发现；分别暴露内部 Redis 6379 与 Sentinel 26379，不通过普通 round-robin Service 猜测当前主节点，不对公网暴露。

依据：[Redis Sentinel](https://redis.io/docs/latest/operate/oss_and_stack/management/sentinel/)。

## 持久化、认证与服务发现

- 每 Pod 独立 PVC，保存 Redis AOF 和 Sentinel 可写状态文件；Sentinel config/myid/epoch/已知拓扑不能只放 ConfigMap 或 emptyDir。开启 Sentinel 必须开启持久化。
- 新配置包含 masterName、Sentinel resources、启动发现超时、downAfterMilliseconds、failoverTimeout、parallelSyncs、副本确认数量/超时；只允许符合固定三实例拓扑的有效值。
- Sentinel 模式要求使用密码，优先引用用户提供的已有 Secret；不自动生成升级时变化的密码。Redis 复制认证、Sentinel 管理 Redis 的认证，以及客户端连接 Sentinel 的认证同时配置。
- 现有 Redis client Options 没有 SentinelPassword/SentinelUsername；增加独立字段并传给 go-redis UniversalOptions，补原生 config/env 和 external values 的兼容字段。内置模式可以从同一个 Secret 映射两端密码，不假定 Redis Password 自动用于 Sentinel。
- 探针/初始化用 REDISCLI_AUTH 或私有配置文件传递凭据，不把密码放 redis-cli 命令参数或日志；私有配置文件只允许服务用户读取。
- 启用 hostname 解析与公告，通过稳定 Pod DNS 发现副本和当前主节点，Pod IP 变化不能使旧 Sentinel 状态永久不可达。

## 首次启动及重启恢复

1. Sentinel 服务与 Redis 启动发现独立启动，避免 Redis 的 initContainer 阻止 Sentinel 先形成多数派。
2. 本 Pod 已有 Sentinel 状态时保留其 master/epoch/myid，不把监控目标改回 0 号 Pod；空状态只生成首次监控配置，不直接授予该 Pod 主角色。
3. Redis 启动必须取得至少两个 Sentinel 对相同 master/配置 epoch 的一致报告，校验目标属于本 release 三个稳定成员；意见不一致、权限/网络错误或少数派时保持 NotReady 并有界重试，不能以 0 号 Pod 兜底。
4. 首次空状态多数派收敛到 0 号成员后才允许它启动为主，其余成员作为从。后续重启按照持久化 Sentinel 多数派报告，旧主必须追随已晋升的新主。
5. 仅一个 Redis 容器重启、整个 Pod 重建、非 0 号主晋升后的全组重启均要覆盖；不能以“第一次正常启动”替代恢复正确性。
6. 启动包装器最终 exec redis-server，转发终止信号，不新增 sleep PID 1；Sentinel 自己负责正常运行期间监测、选举和重配置，不添加第二个自制持续选主控制器。

## 数据安全与可用性边界

- 内置 Sentinel 给 API 的有效配置默认为 `requireHA=true`、`durability=replica_ack`、ackReplicas=1；确认超时有界，保留现有同连接 Lua/屏障/WAIT 和不确定结果 fail-closed 处理。
- Redis 配置 AOF，以及 min-replicas-to-write/min-replicas-max-lag，缩小隔离旧主继续接受写入的窗口；三个节点拓扑要能在一个故障时仍满足一个副本确认。
- WAIT 是复制确认，不是共识协议或绝对零数据丢失保证。副本不足/切换期间允许明确失败，不吞错、不自动降级 best_effort，不承诺零中断或没有已确认写入回退风险。
- NetworkPolicy 只放行本 release Redis/Sentinel 间的复制/选举，以及本 namespace API/drain hook 对相关端口的访问；不能阻断卸载前 drain，也不添加跨 namespace 或全网开放规则。
- 只有实际副本及 Sentinel 多数派满足条件时才对外认为启动就绪，不能仅依赖 PING。存储、调度和 CNI 故障继续可见，不用长时间固定 sleep 掩盖。

## Chart 控制面一致性

- 抽取有效 Redis 连接配置供 API deployment、API 等待 Redis、pre-delete drain 和离线 pre-upgrade drain 共用；mode/addrs/masterName/认证/HA/ACK 必须一致且 env 名称唯一。
- pre-upgrade 已安装 deployment lookup 的环境保留逻辑继续使用旧实际配置，不强行切到尚未创建的新拓扑；新用户本次采用重装，不据此删除原有升级安全 hook。
- productionSafetyChecks 接受满足约束的内置 Sentinel，继续拒绝内置 standalone、best_effort 和缺失必要认证/持久化条件；外部 HA 检查保持有效。
- standalone 默认渲染不产生 Sentinel、额外 PVC 或拓扑变化；external 模式不产生内置服务。新增配置注释、示例和 native config 同步，避免只在 Chart 增加字段。

## 卸载重装

卸载前现有 release drain 必须在 Redis/Sentinel 仍可访问时完成；不绕过 hook、不自动删除业务能力证据。StatefulSet PVC 默认保留，uninstall 不等于数据库被清空；文档明确新安装的卷复用和全新数据区别。若确实要丢弃旧卷，另行核对精确卷、备份及用户授权，不把 PVC 批量删除放进模板或安装脚本。

此次新旧 Redis 名称/卷布局要明确区分，不自动把旧 standalone 卷当作完整 Sentinel 三节点数据集。当前旧部署及其历史 Redis 记录不由本轮修改。

## 验收

- Helm 结构化测试覆盖默认 standalone、内置 Sentinel、external 三种模式、生产检查、三个成员、反亲和/PDB、PVC、AOF、凭据引用、探针和 API/drain 的有效配置及 env 唯一性；非法配置必须失败。
- 确定性启动测试覆盖首次启动、持久状态恢复、非 0 号主、少数派/不一致/错误响应、成员越界、DNS/IP 变化及发现超时，不只检查脚本字符串。
- Go 测试覆盖 Sentinel 认证字段传递、旧配置兼容以及 replica_ack 不确定错误不吞掉。
- 使用独立本地网络/卷运行真实 Redis 1 主 2 从 + 3 Sentinel，验证认证、正常写入与 ACK、主故障切换、旧主恢复为从，以及非 0 号主后的重启恢复；测试只操作隔离实例，结束清理自身资源。
- Kubernetes 隔离部署检查启动、分散调度及正常 drain；真实业务 API/FUSE 多副本和节点故障矩阵在用户部署后继续验收。本地通过不等于生产集群 HA 通过。

## 设计自审

不把 API 支持连接误写为 Chart 已搭建集群；不采用固定主 Pod 回退；Sentinel 状态可写且持久；启动不依赖有序 Ready；认证覆盖客户端 Sentinel 通道；PVC 不自动清空；原有 fencing/ACK/卸载安全路径保留。本设计描述目标能力，不是完成声明。
