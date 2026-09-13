# 内置 Sentinel 基础可行性实验结果

## 范围与结论

已执行本地真实 Redis/Sentinel 隔离实验，基础引导、主故障晋升、旧主重返、保留状态的全组正常冷恢复以及副本不足拒绝写入均通过。本结果只解除这些基础机制的可行性门槛，不代表生产 Chart、初始化授权/卷身份协议或故障矩阵已实现。

- Redis：`redis:7-alpine`，实际 `Redis server v=7.4.11`，linux/aarch64。
- 独立 project：`sandbox-sentinel-review-20260913-839f51f`；开始前同标签容器/卷列表为空。
- 拓扑：3 Redis + 3 Sentinel，独立网络、无宿主机端口映射，6 个独立实验卷分别保存 AOF/Redis 配置和 Sentinel 可写配置。
- 使用公开实验密码 `review-only-not-production`，未引用线上 Secret；未构建或推送镜像。
- 首次配置只在文件不存在时复制；重启不以原始模板覆盖 Sentinel/Redis 的角色和 epoch。

## 实测证据

| 场景 | 实际结果 |
| --- | --- |
| Redis 先于 Sentinel 启动 | redis-0 主、redis-1/2 从；初始 wait_bgsave 后两个副本变为 online，不依赖 Sentinel 先 Ready。 |
| Sentinel 收敛 | CKQUORUM 返回 `OK 3 usable Sentinels`；MASTER 报告 num-slaves=2、num-other-sentinels=2、quorum=2。 |
| 认证拒绝 | 无凭据 Redis/Sentinel PING 均返回 `NOAUTH Authentication required.`；检查响应正文，未把 CLI 进程码 0 误认作认证成功。 |
| 写入副本确认 | 同一 redis-cli 连接 SET before-failover 返回 OK，随后 WAIT 1 3000 返回 2。 |
| 主故障晋升 | 停止 redis-0 后 Sentinel 自动晋升 redis-2；三个报告最终一致，config-epoch=1，新主 INFO 确认为 master 且有一个健康副本。停止到第一次观察晋升约 19.3 秒，是观察上界而非精确切换时长。 |
| 晋升后写入 | redis-2 同连接 SET after-failover=OK、WAIT=1。未人工执行 REPLICAOF NO ONE。 |
| 旧主重返 | redis-0 刚启动曾报告 master/零副本，随后 Sentinel 重配为 slave、master_host=redis-2、master_link_status=up；收敛后尝试写入返回 READONLY。未宣称此过程零中断或强一致。 |
| 持久角色 | redis-0/1 的可写配置被 CONFIG REWRITE 保存为 replicaof redis-2；redis-2 配置不再含旧 replicaof。三个 Sentinel 保存不同 myid，monitor 指向 redis-2，config/leader/current epoch 均为 1。 |
| 全组正常冷恢复 | 全部六容器停止，保留卷后启动；约 11.5 秒检查时 redis-2 仍为主、两个从链路 up、三个 Sentinel 均返回 redis-2；GET=after-failover，新 SET=OK、同连接 WAIT=1，CKQUORUM 正常。时间同样为观察上界。 |
| 两副本故障 | 停止 redis-0/1 后剩余主 SET 返回 `NOREPLICAS Not enough good replicas to write.`，本地 PING 仍 PONG。此场景未配置 Kubernetes 探针，不声称验证了探针行为。 |

当前相同密码下，复制、Sentinel 管理 Redis、客户端认证及 Sentinel 多数派形成均正常。独立密码、特殊字符转义和 Go client Sentinel 字段仍未验证。

## 新观察与尚未关闭的门槛

- 保留 Redis 可写配置很重要：本轮真实 CONFIG REWRITE 保存晋升/从属角色；仅持久化 AOF 而每次重置角色不能等同本实验。
- Redis 成员地址已用稳定 DNS；实验 Sentinel 没有设置每实例 announce-ip，known-sentinel 保存容器 IP。生产方案必须按规格配置稳定 Pod DNS，并追加容器/Pod IP 换代实验。本次只是同一容器 stop/start，不覆盖重建换 IP。
- 没有实现 clusterID/阶段 ConfigMap、初始化 Job、本地卷标记的可信传输、防重放或部分引导续跑。
- 尚未执行多数状态丢失、epoch 分歧、突然断电/AOF 回退、网络分区、特殊密码、真实 Kubernetes 分散调度/节点故障、API/FUSE 业务矩阵。
- WAIT/异步复制不是共识或绝对零数据丢失保证。不能从此次正常恢复推断任意全组停机/PVC 丢失可自动无损恢复。

## 清理与交付

清理前按 project 标签核对六容器、六卷和一个网络，均为本次新建实验资源。使用同一 project/file 执行 down --volumes，移除上述实验容器/卷/网络；随后三个资源列表为空。没有删除业务资源、缓存镜像或其它 project，实验数据仅存在本次已删除卷中。

夹具及报告留在仓库，可复现实验。后续生产实施计划须先补初始化身份协议及失效状态实验；本轮没有修改生产 Chart/runtime/config、线上部署或 Redis。
