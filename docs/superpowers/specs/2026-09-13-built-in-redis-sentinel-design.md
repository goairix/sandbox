# Helm 内置 Redis Sentinel 设计

## 已确认需求与范围

用户没有现成 HA Redis，要求 Chart 增加内置 Sentinel，并计划卸载旧 release 后重新安装。镜像构建、线上 uninstall/install 由用户执行，本轮不操作当前业务 Redis、不删除 PVC，也不提供无证据在线迁移。

选择 Chart 原生 StatefulSet + Redis 自带 Sentinel，不要求用户先部署外部集群。与单实例相比提供自动主从切换；与 Cluster 相比不引入尚无需求的数据分片和六实例拓扑。外部 standalone/sentinel/cluster 路径继续保留，内置 Cluster 不在范围。

## 拓扑与部署

- 新增 `redis.mode: standalone | sentinel`，默认 standalone，保留现有内置单实例行为。
- Sentinel 模式固定三个 Redis/Sentinel Pod，每 Pod 一个 Redis、一个 Sentinel，以及一个原生 sidecar 身份核验器，正常形成 1 主 2 从及 3 Sentinel，quorum=2。核验器声明在 `initContainers` 中，`restartPolicy: Always`，不是运行一次就退出的 init；用户于 2026-09-13 确认此调整。
- StatefulSet 使用 Parallel 启动，不能使用要求前一个 Redis Ready 才启动下一个的默认顺序，避免首次多数派形成死锁。
- 三 Pod 按 hostname 必须分散到不同工作节点；不足三个合格节点时 Pending，不把同节点多副本描述为高可用。配置 PDB 保留至少两个可用实例，解释其只限制主动驱逐、不保证真实节点故障无中断。
- 内部 headless Service 提供稳定的每 Pod DNS，publishNotReadyAddresses 用于启动发现；分别暴露内部 Redis 6379 与 Sentinel 26379，不通过普通 round-robin Service 猜测当前主节点，不对公网暴露。

依据：[Redis Sentinel](https://redis.io/docs/latest/operate/oss_and_stack/management/sentinel/)。

## 持久化、认证与服务发现

- 每 Pod 独立 PVC，保存 Redis AOF 和 Sentinel 可写状态文件；Sentinel config/myid/epoch/已知拓扑不能只放 ConfigMap 或 emptyDir。开启 Sentinel 必须开启持久化。
- 新配置包含 masterName、Sentinel resources、启动发现超时、downAfterMilliseconds、failoverTimeout、parallelSyncs、副本确认数量/超时；只允许符合固定三实例拓扑的有效值。内置 Sentinel 的 durability 固定 replica_ack，不接受 native/best_effort；ackReplicas 固定 1 以保留单节点故障容忍，不能允许 2 然后宣称仍可单节点故障写入。
- Sentinel 模式要求使用密码，优先引用用户提供的已有 Secret；不自动生成升级时变化的密码。Redis 复制认证、Sentinel 管理 Redis 的认证，以及客户端连接 Sentinel 的认证同时配置。
- 现有 Redis client Options 没有 SentinelPassword/SentinelUsername；增加独立字段并传给 go-redis UniversalOptions，补原生 config/env 和 external values 的兼容字段。内置模式可以从同一个 Secret 映射两端密码，不假定 Redis Password 自动用于 Sentinel。
- 配置 Sentinel requirepass/sentinel-pass（客户端及 Sentinel 相互认证）和 auth-pass（访问 Redis），以及 Redis requirepass/masterauth；四条链路均验证，不只给 API 加一个密码。引用 Secret 时同一引用传给 API、初始化、Redis/Sentinel 和 drain；外部 Redis/Sentinel 允许使用不同密码，Secret 各键明确区分。
- 探针/初始化用 REDISCLI_AUTH 或私有配置文件传递凭据，不把密码放 redis-cli 命令参数或日志；私有配置文件只允许服务用户读取。配置生成必须按 Redis 配置语法转义引号、反斜杠和空白，拒绝 NUL/控制字符，测试带特殊字符的真实认证，不拼接不可信 shell。
- 上述转义不能修复 Redis 自身后续重写的缺陷：Redis 7.4 及 8.2 的 [Sentinel rewrite](https://github.com/redis/redis/blob/7.4/src/sentinel.c) 对 auth-pass/sentinel-pass 使用未引用的原值。内置模式先保守限定管理/数据密码为至少 32 字节的 ASCII 安全 token（字母、数字、`_`、`-`），不接受空格或引号等会在重写后改变解析的值，不悄悄变换用户密码。外部客户端的特殊字符密码支持不变；已有真实 fixture 的首次认证成功不能证明 Sentinel 重写后的冷恢复，必须额外运行持久配置重启验收。不能将手工引用的测试字符串冒充 Redis 真实 rewrite 输出。
- 启用 resolve-hostnames/announce-hostnames，并显式配置各 Redis 的 replica-announce-ip 和 Sentinel 的 announce-ip 为稳定 Pod DNS；所有成员统一使用 DNS，不仅在 Service 上设置一个名称。测试 client hostname 兼容及 Pod IP 变化，不能继续持久化旧 Pod IP。

## 首次启动及重启恢复

1. Sentinel 服务与 Redis 启动发现独立启动，使用 Parallel Pod 管理和 publishNotReadyAddresses。Sentinel 的配置传播/发现依赖 Redis Pub/Sub，首次引导不能要求所有 Sentinel 先完成互相发现、再启动任何 Redis，这会产生循环依赖。
2. 引入仅用于初始化/恢复身份的 namespace 级状态 ConfigMap，记录 clusterID、固定成员和 Pending/Initialized 阶段；自身 PVC 保存相同 clusterID 及本地初始化标记。Chart 正常更新保留状态，卸载默认保留该记录以匹配保留的 PVC；记录与非空卷不匹配、记录缺失但卷非空都拒绝重新引导。ConfigMap 不是新的稳态选主数据库。
3. Pending 的全新实例各自确认卷为空/身份一致，唯一允许的初始主是该新 clusterID 的 0 号成员，其余成员启动为它的从；Redis 与 Sentinel 随后形成真实拓扑。首次写入本地标记和配置需可中断恢复；Pending 不等于允许每次重启都重置角色，部分初始化或已发生切换时保留持久配置、由 Sentinel 继续收敛，不能重新指定 0 号为主。一次性、有界初始化 Job 验证三个身份、主角色、复制完成、Sentinel 成员/epoch 收敛和副本确认，再以 resourceVersion 前置条件将阶段置为 Initialized；API 等待该阶段才接入，不以局部 PING 放行。
4. 初始化 Job 只能 get/update 本 release 指定名称的状态 ConfigMap，不访问 Node、任意 Pod Exec 或其它 release 状态。各成员先在本地验证 PVC 标记及固定身份，Job 经认证连接验证固定成员的服务身份和拓扑；共享密码或相同 DNS 不能单独证明 PVC 身份。可信本地验证结果如何供 Job 核对，必须在实现计划明确传输协议及防重放边界，并先通过初始化中断/同名 replacement 实验，不凭脚本返回值假定远端卷正确。同名 replacement 或重复 Job 不取得重置/删除权限。该 Job 不是 pre-install 等待尚未创建 StatefulSet 的 hook。
5. Initialized 后保留可写 Sentinel config/myid/epoch 和 Redis 持久配置；正常运行中的晋升只由 Sentinel 完成。报告相同地址/epoch 和 CKQUORUM 是发现/健康证据，不是额外共识协议，包装器不得据此擅自执行 REPLICAOF NO ONE 来提升副本。已晋升主存在时，旧主重启只能追随当前主或等待 Sentinel 完成切换。
6. 空状态 replacement 必须向已有初始化成员学习当前配置，不能先生成旧 0 号 master 配置来凑成一个“新多数派”。丢失多数初始化状态时停止自动引导，要求恢复备份/人工确认，不能制造两个空状态 Sentinel 的默认意见覆盖剩余新 epoch。
7. 全组冷启动区分状态一致的正常恢复与状态缺失/分歧。只允许恢复身份、持久主角色及 Sentinel 配置证据一致的既有主，不能把副本重新提升；看见更高 epoch 不得选择较旧多数报告。无法证明时明确阻断并提供人工恢复说明，不承诺任意全组停机/PVC 丢失都能自动无损恢复。
8. 仅一个 Redis 容器重启、整个 Pod 重建、非 0 号主晋升后的全组重启，以及两份状态丢失/epoch 分歧均要覆盖；启动包装器最终 exec redis-server，正确处理初始化等待期间的终止信号，不新增 sleep PID 1，也不添加自制持续选主控制器。

## 身份核验 sidecar 协议

原生 sidecar 先于业务容器启动并常驻，只读取本 Pod 的 PVC 和经本地认证取得 Redis 进程信息；不修改 Redis 角色、不参与选主、不代理业务命令，也不依据远端 quorum 修改配置。普通 init 无法在 Redis 容器单独重启后回答新的身份挑战，因此不能替代。要求 Kubernetes 1.33 或已经启用原生 sidecar 的受支持版本，见 [Kubernetes 原生 sidecar](https://kubernetes.io/docs/concepts/workloads/pods/sidecar-containers/)。

- 三个固定成员各有独立 Ed25519 私钥，客户端只持有三个公钥。私钥与 Redis/Sentinel 密码独立；每个 sidecar 只挂载自己的私钥文件，Redis/Sentinel、API、初始化 Job 不挂载私钥。已有 Secret 不通过升级重新生成；私钥/公钥和 namespace clusterID 的对应关系必须验证。密钥配置或固定成员变化拒绝复用旧卷，不自动换身份。
- 请求只允许协议版本、用途 `inventory | live` 和 256-bit 随机 nonce，不能提交 marker、角色、epoch、run_id 或待签名任意字节。固定本地身份、文件读取和 INFO 信息均由可信本地提供者产生。响应采用固定结构、域分离签名，包含版本、用途、nonce、clusterID、固定成员、随机 sidecar session、本地卷状态及配置摘要。`live` 额外绑定本地认证 INFO server 的当前 run_id；核验端重新连接相同固定 DNS 的 Redis，校验当前 run_id，不以 DNS/共享密码代替 PVC 证明。
- `inventory` 在 Redis 未运行时也可回答，支持全组冷恢复。卷状态分为真正空卷、只有 Reserved 标记、Configured；Configured 必须读取实际持久 Redis 配置与 Sentinel 配置。Sentinel 主映射 `config-epoch` 与全局 `current-epoch` 分离，后者不能作为晋升主节点的证据。配置不能完整读取、读取期间变动、主角色与 monitor 不一致时拒绝签名；不拿内存中的选主成功推断 CONFIG REWRITE 落盘成功。
- 第一次启动包装器只在真正空卷上排他创建 Reserved marker，不先写角色配置；三个 nonce 绑定的 Reserved/空数据证明经一次性初始化协调器验证后，CAS 登记预期 marker 和唯一全新 seed grant。登记不可被重复 Job 或同名 replacement 改写。seed grant 仅允许同一 clusterID、同一组 marker 的 0 号初始主与其它初始从；一旦任一完整配置产生，不重新许可空卷替换原登记。初始化期间中断须依据已登记 grant 和保留配置恢复，不把 Pending 当重置授权。
- Initialized 后的 replacement 空卷因 marker 与登记不符自动阻断，需要明确恢复/替换身份的人工流程；不承诺自动处理已经丢失 PVC 的节点。保留原卷的 Pod 重建及 Redis 单容器重启通过新 nonce、当前 marker 和 run_id 重新核验。完全相同的卷/密钥备份克隆无法与原卷区分，信任 Kubernetes/DNS/Secret 与节点控制面，不声称硬件来源证明。
- HTTP 核验端口只用于启动/恢复检查，不处理业务 Redis 流量。签名响应不传密码、配置原文或私钥；请求体/响应体、并发、超时、重试均有上限，客户端禁止重定向和环境代理、只连接固定成员 DNS。NetworkPolicy 限制到本 release 包装器和初始化 Job；即使读取者重放旧响应，新 nonce 也必须拒绝。HTTP 本身不提供流量机密性，不在响应中放机密数据。
- sidecar startup/liveness 只检查本地核验服务，不等待 Redis、远端成员或初始化 Job，避免启动循环。稳态无定时选举/拓扑写入，签名/文件读取只在核验请求发生时执行；资源开销和恢复延迟需实测，不能用“不代理业务”推断零开销。

此节修订原两容器方案的身份传输缺口，不改变 Sentinel 唯一选主权、replica_ack、PVC 保留和线上不自动迁移的边界。

## 数据安全与可用性边界

- 内置 Sentinel 给 API 的有效配置默认为 `requireHA=true`、`durability=replica_ack`、ackReplicas=1；确认超时有界，保留现有同连接 Lua/屏障/WAIT 和不确定结果 fail-closed 处理。
- Redis 配置 AOF，以及 min-replicas-to-write/min-replicas-max-lag，缩小隔离旧主继续接受写入的窗口；三个节点拓扑要能在一个故障时仍满足一个副本确认。
- WAIT 是复制确认，不是共识协议或绝对零数据丢失保证。副本不足/切换期间允许明确失败，不吞错、不自动降级 best_effort，不承诺零中断或没有已确认写入回退风险。
- NetworkPolicy 只放行本 release Redis/Sentinel 间的复制/选举，以及本 namespace API/drain hook 对相关端口的访问；不能阻断卸载前 drain，也不添加跨 namespace 或全网开放规则。
- readiness 验证主/从角色、从的复制链路和主所需健康副本，并通过 Sentinel CKQUORUM 检查投票能力；初始 API 接入还要求 Initialized 及同连接有实际写入 offset 的有界 barrier/WAIT，不以 offset=0 的 WAIT 假确认。稳态探针不反复写 barrier。
- liveness 只检查本地服务是否存活，不能因远端副本不足或 Sentinel quorum 丢失重启健康实例，避免网络故障时重启风暴。startupProbe 总预算覆盖角色发现超时，不在正常引导等待中反复杀进程；各探针、初始化 Job 和 Helm 等待预算明确匹配。

## Chart 控制面一致性

- 抽取有效 Redis 连接配置供 API deployment、API 等待 Redis、pre-delete drain 和离线 pre-upgrade drain 共用；mode/addrs/masterName/认证/HA/ACK 必须一致且 env 名称唯一。
- pre-upgrade 已安装 deployment lookup 的环境保留逻辑继续使用旧实际配置，不强行切到尚未创建的新拓扑；新用户本次采用重装，不据此删除原有升级安全 hook。
- productionSafetyChecks 接受满足约束的内置 Sentinel，继续拒绝内置 standalone、best_effort 和缺失必要认证/持久化条件；外部 HA 检查保持有效。
- standalone 默认渲染不产生 Sentinel、额外 PVC 或拓扑变化；external 模式不产生内置服务。新增配置注释、示例和 native config 同步，避免只在 Chart 增加字段。

## 卸载重装

卸载前现有 release drain 必须在 Redis/Sentinel 仍可访问时完成；不绕过 hook、不自动删除业务能力证据。StatefulSet PVC 默认保留，uninstall 不等于数据库被清空；文档明确新安装的卷复用和全新数据区别。若确实要丢弃旧卷，另行核对精确卷、备份及用户授权，不把 PVC 批量删除放进模板或安装脚本。

Sentinel StatefulSet/卷使用区别于旧 standalone 的名称，不自动复用旧单实例卷；保留的 clusterID 状态只匹配相同的 Sentinel 卷组，不把 uninstall/install 当成初始化许可。彻底清空与恢复旧组是两种不同运维操作，均需精确确认卷及初始化状态。当前旧部署及其历史 Redis 记录不由本轮修改。

## 验收

- Helm 结构化测试覆盖默认 standalone、内置 Sentinel、external 三种模式、生产检查、三个成员、反亲和/PDB、PVC、AOF、凭据引用、探针和 API/drain 的有效配置及 env 唯一性；非法配置必须失败。
- 确定性启动测试覆盖首次引导无循环依赖、阶段关闭、重复初始化、非空旧卷/clusterID 不匹配、持久状态恢复、非 0 号主、少数派/不一致/更高单份 epoch、两份状态丢失、成员越界、DNS/IP 变化及发现超时，不只检查脚本字符串。
- Go 测试覆盖 Sentinel 认证字段传递、旧配置兼容以及 replica_ack 不确定错误不吞掉。
- 使用独立本地网络/卷运行真实 Redis 1 主 2 从 + 3 Sentinel，验证四条认证链路及特殊字符密码、正常写入与 ACK、主故障切换、旧主恢复为从、非 0 号主后的正常冷恢复，以及状态缺失/分歧必须阻断而不是回到 0 号；测试只操作隔离实例，结束清理自身资源。
- Kubernetes 隔离部署检查启动、分散调度及正常 drain；真实业务 API/FUSE 多副本和节点故障矩阵在用户部署后继续验收。本地通过不等于生产集群 HA 通过。

## 设计自审

不把 API 支持连接误写为 Chart 已搭建集群；全新初始化授权与恢复分开，不采用固定主 Pod 回退或把读到多数地址当选主授权；Sentinel 状态可写且持久；启动不依赖有序 Ready/先发现后 Redis 的循环；认证覆盖全部链路；PVC 与初始化状态不自动清空；原有 fencing/ACK/卸载安全路径保留。自审修正记录见 [双项规格自审](../../testing/2026-09-13-apparmor-sentinel-design-review.md)。本设计描述目标能力，不是完成声明。
