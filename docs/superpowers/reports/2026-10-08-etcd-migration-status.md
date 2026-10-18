# etcd 迁移整体状态

本次目标仍是主方案阶段一至五完整替代；多 cell 是后续扩展。默认启动仍连接 Redis，显式 etcd 模式仅开放已验收路由，不能宣称生产切换完成或线上性能已经提升。

## 当前汇总：2026-10-18

| 口径 | 进度 | 判断依据 |
| --- | --- | --- |
| 功能代码 | 约70%，粗估范围65%–75% | 已有状态协议、plain创建/查询/执行/文件下载、清理框架；业务与FUSE仍有缺口 |
| 整体交付 | 约45% | 沿用20/30/20/15/15权重和90/70/30/0/0阶段估算，包含容量、故障、部署恢复及最终替代 |
| 生产切换 | 0% | 默认入口仍Redis，没有执行切换或部署 |

功能代码未完成：其余API、Pool/Docker/Upload及默认入口替代、detached销毁消费者及
独立自动清理worker的其余故障/恢复组合；分区due/dirty/Watch的
剩余调度；etcd FUSE创建/挂载/flush/双进程终止证明与collector；长期历史及证书维护；
Redis路径移除。自动清理循环和v2入口、独立worker命令已有实现与局部验证，但真实
组合已通过四个新native场景；完整Task审查发现的两项代码问题已修复并限定复核关闭，
容量与恢复组合仍在补验，不能计为自动清理完整交付。

整体交付另差：大量已有sandbox的容量/延迟/内存与长稳验证、真实故障组合、部署配置、
备份恢复及权限验收、最终完整审查和切换验收。组件模型通过不能代替这些验收；当前
没有实测生产性能提升或大N容量结论。上述百分比不按行数、提交数或测试数累计。

新增 `361003b` 完成真实signed detached/plain来源分类与显式pinned image配置；
`9697527` 完成严格v2 termination wire并保留v1字节兼容。两项host/race/Linux vet、
完整证据核对及各唯一独立Task审查通过，见[detached基础验证](2026-10-18-etcd-detached-foundation-verification.md)。
`784f760`/`7e1656b`补齐原Pending、唯一settlement codec与64节点事务投影；
`898cac7`补显式v2固定键、完整profile拒绝及observer请求128KiB/回复256KiB边界。
这些切片host/race/Linux vet及限定独立审查通过；原短draft回复预算漏算经反例修复，
合并后定向host通过。`58f38fd`完成持久writer、原draft Unknown仲裁及真实历史读取组件，
六个真实三成员顶层用例、四个Pending子场景及单独32KiB点读组件通过；Linux构建/vet、
完整指纹审计及原唯一审查员限定复核通过，见[writer组件验证](2026-10-18-etcd-detached-writer-verification.md)。
`809838d`/`bc9dc8a`完成实际writer→native loader→observer读取组件；真实三成员
正向、四类来源ABA及取消兼容性RED/GREEN通过，唯一Task4A审查限定Spec/Quality通过，
见[reader组件验证](2026-10-18-etcd-detached-reader-verification.md)。
`45ed1ca`完成真实Joined门禁的新Run51节点恢复、Unknown原候选只读协调、
完成Stage安全退休及保留对象图计量；实际七项native、两项graph和定向race/vet通过，
唯一reviewer组件Spec/Quality通过，见[冷恢复组件验证](2026-10-18-etcd-detached-cold-verification.md)。
实测85209字节仅属该fixture；全局容量和消费者仍开放。
`7102368`补固定合法signed长输入：实际Q16949字节经原publish/签名/writer/load全链通过，
主分支合并同ELF也通过长输入（Q16952）和原provider Unknown；这不是绝对max或长输入内存验收。
`54e3d08`接通生产provider59节点reservation、slot0/1、一次Terminate Unknown及
同Claim只读诊断不重发；真实TLS/native用例与原v1 race/vet通过，唯一组件审查通过。
审查发现的续租cutoff问题已真实RED/GREEN关闭，见
[provider首片验证](2026-10-18-etcd-detached-provider-verification.md)。
`9c5ad2e`修复实际Unknown调用后owner保留ctx/cancel阻塞冷恢复的问题，保持原marker身份；
Close改为锁内快照cancel/done。真实newRun/newClaim retained Query、短租约回归、
18项race及Linux构建/vet通过，唯一组件审查通过，见[新Run查询验证](2026-10-18-etcd-detached-query-verification.md)。
`1e4d836`补真实PID1/cgroup死亡及NodeObserver内部签发proof，原七槽全部complete；
辅助与主分支实际Linux组合、构建/vet、完整指纹审计及唯一限定审查通过，见
[物理组件验证](2026-10-18-etcd-detached-physical-verification.md)。测试调度仍有明确fixture
接缝，不代表managed admission。HTTP仍501；其后继续owner/terminal/quota、自动
消费者和部署/HTTP组合。完整Task4A、容量及最终替代仍开放。
`5f540a2`补实际三成员 client/peer TLS fixture，跨成员线性读写、observer权限、
三个原进程Wait与Linux构建/vet通过，见[TLS3验证](2026-10-18-etcd-detached-tls3-verification.md)。
`c0735b2`修复真实合法长输入触发128KiB门禁的问题：原门禁内退休冗余已完成缓存，
固定长输入完整draft+recovery图129326字节，三项native及Linuxbuild/vet、限定独立
审查通过。其他合法最大值、全局容量与消费者未关闭，见上述冷恢复验证报告。
`95525f0`补SandboxID/SnapshotVersion各128字节的确定组合，真实producer通过后
曾再现完整图超限；在原强门禁内退休完整已完成close缓存后，新组合122085字节、
原长输入117652字节，四项native/build/vet及限定独立审查通过。主分支新组合和
provider配置hooks整合也通过；全phase及全局64MiB准入仍缺实现，不能作为大N容量结论。
`0c6a009`补独立严格v2 owner seal、历史Claim归属及63节点纯投影；原五项RED/GREEN、
race/Linuxvet和主分支整合通过，唯一限定审查通过，见
[owner基础验证](2026-10-18-etcd-detached-owner-foundation-verification.md)。尚无live删除、
surviving history认证或consumer接线，不能计为安全释放完成。
`0bd7852`新增独立v2 quota-return codec，原v1固定字节回归不变；六项RED/GREEN、
race/Linuxvet、主分支整合及唯一限定审查通过，见
[quota编码验证](2026-10-18-etcd-detached-quota-codec-verification.md)。同时最大模型15360字节，
未接quota writer/消费者，不能计为配额归还完成。
`1049226`接通原job/Claim下的63节点owner释放和删除index后的独立13点认证；实际
提交后丢ACK、死Claim两次只读仲裁、standalone/paid冷读通过，race十四项及Linux
build/vet、指纹审计和唯一限定审查通过，见
[owner原生组件验证](2026-10-18-etcd-detached-owner-native-verification.md)。普通/损坏ACK、
cutoff/Close矩阵仍缺，自动消费者和全局准入未接完。后续`268cd22`已补真实发送前
absence→原Joined/actor51→新Claim/普通ACK63→cold13验证；独立普通/损坏ACK仍开放。
`9328ab4`补齐终态v2的13点历史/22 pin有限传输profile，Host/race/Linuxvet、主分支
整合与证据审计通过，见[传输验证](2026-10-18-etcd-detached-terminal-transport-verification.md)。
旧v1维度不变；新profile尚未由终态消费者选用，不能计为公开终态查询接通。

新增 `2a6c46d`/`71aa9ad` 完成实际自动 cold quota actor、单共享工作区、原 job
来源与配额确认门禁、quota-only 终态消费及 P 保留/删除接线；`00806a9`/`d16b720`
接通显式 v3 部署与 `serve-cold-quota <manifest>` 独立命令。新 actor host20组、
相关 race79组及 Linux vet、部署全包 race34组与实际 Linux 命令构建通过，各唯一
Task 审查发现的问题经修复和限定复核关闭。两个新增真实 Linux 场景已通过：
mounted 与独立 role 都从启动前真实冷创建积压完成物理清理、Run join、配额归还，
原 P 在实际 receipt 提交后才删除；实际 Close/Wait 与独立进程信号退出、租约释放
通过。详见[自动配额及 v3 验证](2026-10-18-etcd-automatic-cold-quota-verification.md)。
两场各一条 Task，尚未覆盖大 N、32 物理并发或全部替代/Unknown 故障组合；
全量功能和交付粗估口径不因此按提交数上调，默认生产入口仍为 Redis。

最近小步提交：`c711d8a`补满槽/重复/空位恢复控制回归；`5c92719`接入严格v2自动部署；
`fae0b5b`新增独立cleanup worker命令。前者host/race通过，后两者host/race/Linux vet
通过，命令ELF已构建。`9631088`新增四个真实Linux组合：启动前积压自动物理清理、
独立命令signal/失权退出、P的ABA及已提交删除丢ACK恢复，全部通过，详见
[自动组合验证](2026-10-17-etcd-automatic-native-verification.md)。每场一条真实Task，
不能作为32物理并发或大N容量证明。首次容器配置及新helper数值精度失败日志保留。

唯一完整Task审查仍未整体批准。`acda95c`修复可选E损坏阻塞后续历史索引；`fd92f9a`
以固定摘要保留自动job失败。两片均通过定向host/race及Linux检查，同一审查员限定
复核确认I1、I2均已解决且无新增问题。`a156a01`补齐真实物理失败后同worker恢复、
Close/Wait保留失败及正式进程exit1证据，见[结果保留真实验证](2026-10-17-etcd-automatic-result-native-verification.md)。
`bbeca13`通过真实三成员分页/压缩拒绝/原producer丢ACK后新Backend发现/读取公平性，
同一审查员认可该元数据范围。`31240d3`补齐31个元数据占槽加1条真实物理清理、满槽
拒33、原worker安全释放后33恰复用同槽，真实Linux场景与独立审计通过，同一审查员
认可该持久槽证据项，见[持久容量验证](2026-10-17-etcd-persistent-capacity-native-verification.md)。
它不是32物理并发或大N验收。I3的真实去重及后继物理进展、负载下执行公平性、压缩后
完整恢复和其Close组合仍开放；没有无Run归还永久槽捷径。

`8585e2c`修复冷创建receipt identity被业务TTL截断、不能覆盖清理会话的问题；业务
publication/activation仍受原TTL约束，定向host/race/Linuxvet通过，长期证书更新未完成。
`b881535`将原quota返还API接到Grant前prepare/一次consume原语，固定原ref且保持
原34/4/4事务与replay顺序；host/race/Linuxvet及独立Task审查通过，见
[原语验证](2026-10-17-etcd-cold-quota-prepared-verification.md)。接续 `cdd489e` 将公共API
接到Grant前持久journal，Unknown仅仲裁原ref、原aborted receipt固定后才替换，host/race/
Linuxvet和唯一Task审查通过，见[journal验证](2026-10-17-etcd-cold-quota-journal-verification.md)。
`c929253` 的两个新增真实Linux场景验证成功返还、journal丢ACK后冷Backend由public API
自行仲裁、实际quota槽epoch+1复用及旧请求replay不读或改新槽；两场完整通过、独立审计
及唯一Task审查通过。首次父fixture分类失败如实保留，未重播旧矩阵，见
[真实public配额验证](2026-10-17-etcd-cold-quota-journal-native-verification.md)。
当时仍未接入自动 worker；该接线、terminal pending 删除 gate 与显式 v3 配置现由
`71aa9ad`/`00806a9`/`d16b720` 完成，新增真实组合结果见本节最新记录。

新增 `b228e7f` 验证公开quota协议body的最大尺寸、原RPC顺序和明确反例；
`9f4714c` 实现离线严格v3配额清单解析，`c954648` 实现原worker双预算原子准入。
三片均完成各自host/race/Linux检查、证据审计和唯一Task审查，见
[事务预算基础](2026-10-17-etcd-cold-quota-worker-wire-verification.md)、
[v3配置基础](2026-10-17-etcd-cold-quota-v3-manifest-verification.md)及
[双预算准入](2026-10-17-etcd-cold-quota-worker-admission-verification.md)。
这些基础切片验收时 v3 仍未由 CLI 接受；最新 v3 入口、实际自动 consumer 和共享
保留内存核算现已接通并完成上述范围验证，生产切换与大 N 容量仍未验收。

`3c3afed` 接通五个私有固定 RPC 的真实 gRPC 通信层：限制每次请求、回复、累计
protobuf预算和重复发送，取消后仍为原已识别租约保留一次有界回收。host、race、
Linux vet及唯一完整Task审查通过，见
[通信层验证](2026-10-17-etcd-cold-quota-worker-rpc-verification.md)。`8e745f9` 接通
实际双预算worker journal，保留原引用、全部history/worker比较和54/61节点；
未知ACK消耗原权限，重复败者不能消耗赢家，实际客户端返回后才完成worker调用。
host八组、race四十二组、Linux vet及唯一完整Task审查通过，见
[worker journal验证](2026-10-17-etcd-cold-quota-worker-journal-verification.md)。
首次新测试辅助代码的客户端关闭顺序失败已如实保留，修复没有放松关闭或业务断言。
这两片验证的是有界host协议，尚无自动quota结算、已验证quotaCommitted删除gate、
保留内存容量或真实etcd故障验收；原公开协议和默认启动路径保持原行为。

`c6b0822`/`0707ded` 接通原 Range-only W20 metadata 点读，支持最多14个业务点及
64KiB history，request/reply 合计不超过原256KiB额度，Claim完全独立；host/race/
Linux vet及唯一Task审查通过，见[metadata验证](2026-10-17-etcd-cold-quota-worker-metadata-verification.md)。
`dd4dd64` 提取 compact return/receipt 纯校验，原公开顺序及恢复引用检查保持原样；
`b620b4c` 接通 worker 提交后的分步确认，每轮只付费读一个点，第二步额度不足保留
第一步结果，实际receipt必须匹配原完整引用。两片均完成host/race/Linux vet及独立
Task审查，见[纯校验验证](2026-10-17-etcd-cold-quota-proof-verification.md)与
[分步确认验证](2026-10-18-etcd-cold-quota-worker-postread-verification.md)。
原permit仍保留prepared/history，未完成32任务内存验收。这些私有协议尚未接入完整
自动quota driver，也不表示物理清理或P删除权限；整体粗估百分比保持不变。

`66036ab` 新增独立双预算 worker Resolve，完整W24/29节点，仅仲裁原journal引用的
receipt；丢ACK不重发，不申请新Stage，结果仍为普通Outcome。定向host/race、Linux
vet及独立Task审查通过，见[恢复仲裁验证](2026-10-18-etcd-cold-quota-worker-resolve-verification.md)。
恢复后的aborted receipt或committed return/receipt仍须另行付费认证。

`c3165c7` 提取共享 settlement 纯校验并保持公开读取顺序；`0e53797`/`29cef73` 接通七组
分别计费的完整历史认证，保留真实 publication/终止签名、原出生和六个 CAS source KV。
额度不足从当前阶段继续，Claim 不消耗，返回后及 cached done 仍复核原 owner/context/members。
`b2749da` 的 worker recovery 已实现先检查 committed return、再读原 journal、付费 Resolve 后另付费
认证 aborted receipt 或 committed return/receipt；Unknown 保留原引用，不生成新的 attempt。
两项均完成定向 host/race、Linux vet、证据核对和独立 Task 审查，见
[历史分步认证](2026-10-18-etcd-cold-quota-worker-history-verification.md)与
[分步恢复](2026-10-18-etcd-cold-quota-worker-recovery-verification.md)。
`0d30123`/`c9886d9` 共享原 ready/slot/freeze 校验并完成两阶段付费 prepare；
P2 额度不足保留 P1 ready，合法槽位及原绑定通过后才分配一个 UUID，done 返回原未使用
prepared 指针。host/race、公开协议回归、Linux vet 及独立 Task 审查通过，见
[分步 prepare 验证](2026-10-18-etcd-cold-quota-worker-prepare-verification.md)。首次验证发现的
quota/owner 分区等值限制已修正；原协议允许独立 quota 分区，测试数据和断言未放松。
这些私有 cursor 的共享工作区、32 job 保留内存约束、原 job 正向来源及自动结算/P
删除 gate 已由 `71aa9ad` 接通；显式 v3 配置及两个真实组合也已验证。真实 etcd
完整故障恢复和大 N 容量验收仍开放，功能/整体百分比保持上述粗估。
三项合并后的同包定向 host 回归于隔离工作树通过（2.384s，9119 输入，原始输出
40460 字节，SHA-256 `28b2aa9e75aed3f7ad42b8874829c1ef9c930345aad0f0e12067f2ce47a45d36`），
完整输入/工具/源码指纹核对通过；这不是自动 actor 的端到端组合或容量验收。

`2032c8c`/`626315a`分别接入原Stage销毁service及HTTP DELETE。支持的local v2目标
返回202仅表示任务提交；当前HTTP创建的detached目标返回501且不写销毁状态，不能
宣称完整销毁API已完成。定向race/Linuxvet通过；`de7a7c0`补齐真实local创建/Exec→
HTTP202→原自动worker归档/终止/安全释放/P删除/全部join，以及历史detached拒绝场景。
唯一入口Task审查Spec/Quality通过，无新增C/I/M；二进制时代与未覆盖故障组合见
[真实入口组合](2026-10-17-etcd-destroy-native-verification.md)。接续 `8f58b50` 提取共享纯校验器，
`0f4c6c8` 接通已认证释放后的终态GET：真实HTTP GET200→DELETE202→物理释放后GET404，
实际AcquireIntent复用workspace后旧ID仍404，后继quartet的bytes/MVCC保持不变。host、
race、Linux检查、实际worker运行及独立Task审查均通过，见
[终态查询验证](2026-10-17-etcd-terminal-lookup-verification.md)。原四点reader仍严格拒绝不完整状态，
新结果仅证明旧ID已释放，不授予清理/配额权限；自动quota与detached消费仍未接线。
FUSE静态合同及endpoint编码修复已合入`d2948b3`/`63756cd`，完整host、定向race及
Linux检查通过，同一审查员R1确认问题解决。它仍未启用任何生产FUSE/profile选择器，
不是helper登记、挂载/flush、collector或双进程终止验收。`a83a276` 另接通实际mounter
生产factory的本地PID1/proc/pidfd/ns/exe身份保持、准入撤销及原进程join后句柄回收。
host完整112组、定向race/Linux检查及实际Linux PID1验证通过；同一审查员发现的Status
拷贝阻塞与readiness撤销遗漏已真实RED复现、修复并限定复核关闭，见
[helper原生身份验证](2026-10-17-fuse-helper-kernel-verification.md)。该本地身份能力仍不
等于helper到Pod绑定、真实FUSE挂载或远端flush/collector闭环，不上调生产切换比例。

下文为按阶段保留的历史记录，其中“尚未实现”等描述指各条记录当时；当前范围以本节
及对应最新验收报告为准。

## 阶段实施记录

2026-10-17 最新验收：真实 cleanup 执行器已范围完成，代码至 `3f5a72a`，验收记录
`d8dc76e`。明确 Task 引用通过原 due actor、全 cell 持久 32 槽完成实际关闭、排空、
归档同步、终止、安全释放及已验证 joined 历史恢复；mounted loader 和实际关闭等待
已接通。唯一完整 Task 审查的两项 Important 已修复，原审查员限定复核 Spec/Quality
均 Approved，无新增或未关闭 C/I/M。见 [执行器验收](2026-10-17-etcd-cleanup-executor-acceptance.md)。

当前推进永久 pending 自动发现，接入原共享 actor/预算及严格 mounted 组合；随后仍需
独立 worker 角色和主入口接线。完整 API/Pool/Docker/Upload、FUSE/collector/远端同步、
长期历史维护、大 N 容量与故障、部署恢复及 Redis 移除尚未完成。没有外部阻塞；
自动发现循环已实现并通过局部验证，真实物理配置的完整自动消费仍未验收。32 槽仲裁、32/33 请求边界不等于 32 个
物理 Pod 并发。生产切换仍为 0%；下文功能约70%/整体45%是此前粗估，尚未重新核算，
不能以提交数或重叠测试数量上调。

自动发现首片已完成原子 pending producer 和严格记录/Grant前预检，定向12组47子例
host/race、旧手动形状及 Linux vet 通过。首轮 race 输入采集选项遗漏已保留并纠正重跑，
见 [原子待办创建验证](2026-10-17-etcd-pending-producer-verification.md)。实际自动消费者、
历史回填与移除尚未交付。80条固定视图分页现已实现，3组40子例 host/race与Linux vet
通过，见 [待办分页验证](2026-10-17-etcd-pending-page-verification.md)；下一片为有持久进度的
历史回填，不把索引或分页创建当自动清理完成。

回填检查点现已实现严格编码、原worker读取及初始化/视图更新，截止点保持不可变；通用
Stage不能写入该键家族。定向host/race通过，Linux vet与输入审计详见
[检查点验证](2026-10-17-etcd-pending-backfill-bootstrap-verification.md)。尚未推进历史frontier、
完成回填或接通自动消费者，不因本片上调整体估算，也不宣称已验证compaction续接。

平坦intent的固定检查点视图读取也已实现，两组34子例host/race及Linuxvet通过，见
[历史分页验证](2026-10-17-etcd-pending-legacy-page-verification.md)。兼容读取按通用64KiB
值及额外lookahead预算限2条，尚非历史任务认证、进度提交或完成回填；新pending仍每页80条。

共同出生认证现已实现：实际intent定位、Task/intent/link/初始已提交回执同一出生版本、
execution/pending摘要绑定及初步release观察；三组定向host/race和Linuxvet通过，见
[历史认证验证](2026-10-17-etcd-pending-birth-verification.md)。不依赖已经释放的三个当前键，
但尚无新签名释放正例、进度提交、pending删除或自动消费。

受控签名释放读取现已实现，host五组34叶例、race六组58叶例和Linuxvet通过；Linux
正常构造配置的13叶模型验证坏证据拒绝、会话失效与实际Close/Relist等待，容器已清理。
见[释放读取验证](2026-10-17-etcd-pending-release-verification.md)。故意取消的客户端告警
原样保留；尚未验证本读取器的真实签名正例或自动物理链。下一步推进回填进度及终态
pending事务，整个Task仍未完成，也不因此上调整体粗估或宣称大N容量通过。

回填推进代码现已实现：旧任务pending补建/保留与B进度及完成转换同事务提交，四组27叶
host、十三组100叶race、Linux编译/vet及共享释放配置模型回归通过，见
[回填推进验证](2026-10-17-etcd-pending-backfill-advance-verification.md)。仍没有自动actor
或实际全历史回填验收，真实签名成功分支尚待完成。
并行生产入口子任务的生命周期观察接口已合入`13475ad`，见
[worker生命周期验证](2026-10-17-etcd-worker-lifecycle-verification.md)；v2入口仍在隔离分支
实现，不能算独立自动worker已交付。FUSE有界增量缓存统计已合入`1f0f4a3`，
host/race、Linux编译/vet与实际inotify模型通过，见
[缓存统计验证](2026-10-17-fuse-cache-monitor-verification.md)。常驻观察器/只读HTTP已合入
`02119dd`，启动与Kubernetes探针接线合入`b68d2ae`，见
[常驻组件验证](2026-10-17-fuse-resident-readiness-verification.md)及
[接线验证](2026-10-17-fuse-resident-readiness-wiring-verification.md)。健康空闲样本会后台
刷新，kubelet改读固定布尔HTTP；v7合同避免旧exec探针池复用，共享版本也使普通池缓存
失效，不直接改写运行Pod。独立支线审查未发现可操作问题，host/race、Linux编译及
inotify/TCP/服务组/渲染模型通过；真实FUSE/kubelet/CNI、collector及容量仍未完成。

终态pending独立删除也已实现并通过host两组15叶、race六组41叶和Linuxvet，见
[终态删除验证](2026-10-17-etcd-pending-delete-verification.md)。不改变原物理释放或满64
节点owner-release；真实签名成功删除、ABA/丢ACK/关闭等待仍待自动组合。下一主线是
原actor自动发现与严格v2独立worker接线。功能约70%/整体45%/生产切换0%暂不变。

原actor自动发现循环现已实现原pending/回填/签名删除与registry接线，保持原会话/预算，
并修复单Range份额下回填饿死到期扫描的问题。六组host/race及Linuxvet/build通过，
见[自动循环验证](2026-10-17-etcd-automatic-actor-verification.md)。真实物理配置启动、
自动关闭归档/终止、签名成功删除、ABA/ACK丢失及独立命令仍待新的native组合，
没有将本片算作整个自动worker或生产迁移完成。

## 2026-10-17 此前切片记录（已由上述完整执行器验收接续）

实际常驻到期 worker 已范围完成，源码 `1f7f7b6/a22f5fc/6463b5d/1017b66`，审查修复 `9a06de9`。唯一完整审查发现的4项 Important 已经真实回归复现、修复并由同一审查员限定复核批准；无新增或未关闭C/I/M。最终71组针对性host/race、Linuxvet/build及新三成员native03通过，后者9.98秒；原故障/取消告警、首次测试及采集脚本失败如实保留。详见 [常驻到期worker验收](2026-10-16-etcd-due-worker-verification.md)。`688748f` 明确有限轮询限制，未实现earliest-expiry timer、不承诺到期时延。

接着推进真实清理执行、持久32槽及共享调度权限；完整业务入口、FUSE/collector/远端同步、长期回收、大N容量/部署和Redis移除仍未完成。功能约70%（65%–75%）、整体45%、生产切换0%暂不变，组件测试不能当性能或容量验收。仓库规范 `AGENTS.md` 已按用户要求单独提交 `a40d73e`，明确关键注释、逻辑分段和包职责审查，本次修复已落实。

清理执行计划Step2已新增真实创建入口的local清理快照：snapshot2版本化摘要、旧v1默认重试兼容、深复制/完整缓存计费及每位读取者placement绑定已实现。56个状态层与12个创建入口测试组host/race、两包Linuxvet通过，三成员测试资源已清理，历史失败及服务日志保留。见 [清理快照验证](2026-10-17-etcd-cleanup-snapshot-verification.md)。这只是后续实际消费者的生产数据来源；持久32槽、物理清理执行及完整Task审查尚未完成，百分比不因这个切片重复上调。

Step3首片已实现槽位永久记录、占用/轮转CAS、原worker预算下的Claim流水线复用与新协议wire预检。19组回归host/race和Linuxvet通过，三成员资源已清理，日志和首次编译失败如实保留。见 [持久槽协议验证](2026-10-17-etcd-cleanup-slot-verification.md)。正向物理准入、真实run/join和终态结算仍未完成，未将32条占用元数据当32个物理任务验收，整体百分比保持不变。

## 2026-10-16 最新接续：显式 HTTP、文件下载、分区 worker 控制与到期发现已范围验收

当前工作分支 `codex/etcd-state-management`。Lookup 验收提交 `436bef8`，实际执行 service 的四个实现切片为 `2469576`、`6777519`、`7f50806`、`2616a36`，唯一审查 C0/I0/M2 Approved。以下为当前状态，下方较早接续条目保留历史含义，不代表仍需重做。

| 已完成单元 | 验收提交 | 范围 |
| --- | --- | --- |
| 普通冷创建/恢复 consumer | `956536ea` | 固定 plain profile 的真实创建、发布和原请求恢复 |
| 原冷建配额安全返回 | `36a5dd13` | 原实际 runtime 释放后返还及不确定结果保护 |
| 有界 snapshot 读 | `9b2de478` | 64MiB 加权缓存、原 view 确认和有界 miss；无 N 全量扫描 |
| 业务配置 snapshot | `36ec725e` | 严格配置/已解析默认值、v1/v2 实际创建消费 |
| detached 业务申请/幂等恢复 | `1161ee4e` | 无 workspace 的请求身份、stable IDs、竞争与原配置恢复 |
| mounted startup loader | `2106f825` | 文件/key/TLS/kubeconfig 接入及只读启动检查 |
| 按 ID Lookup 与真实健康 | `436bef8` | 当前 metadata/原 CreatedAt/owned 配置、mTLS 健康、TTL/最终确认/Close；审查发现的两项 TLS EOF 缺陷已复现、修复并复核 |
| 实际执行与 mounted signer | `2616a36` | service→原 gate/签名/目标执行/续期/Wait/Close；32秒真实命令和失权、取消、关闭验证通过，审查已批准 |
| 显式 etcd HTTP/CLI | `cdcc859` | `--serve-etcd` 绕过 Redis 初始化，实际创建/查询/同步与SSE执行；唯一审查三项 Important 经 R1 全部解决、Approved。其余业务路由仍501 |
| 八条文件 API 与独立探针 | `9ba1fbb` | 文件工具/launcher2f0dc3b、HTTP8656106；列表/递归/glob/读取/行读取/两类编辑/下载、独立有限health/ready；23项gateway host/race及实际Linux14bridge通过，审查C0/I0/M1 Approved。内容512KiB，大文件/上传尚未完成 |
| 可选有界大文件下载 | `35d3006` | 六个源码小提交，私有磁盘额度/单原Exec/终态后交付/实际deadline/关闭恢复；原完整gateway33项host/race及修复后spool12、下载消费者10项通过，正式native两轮各9bridge全部Wait/IOjoin。唯一review I1经R1修复复核Approved，无未解决C/I；原日志M1保留。配置上限2GiB，实际native验证1MiB+37，上传/分片和2GiB吞吐未完成 |
| 分区 worker 控制基础 | `691944c` | 原会话/单Lease、256分区、全cell固定4个relist及20MiB/s、100claim/s软份额，有界桶与实际Close/join。13组host/race、Linuxvet及最终三成员native六场景通过；同reviewer R1全部解决并Approved。实际due/dirty/Watch消费和持久32任务硬槽未完成 |
| 原子到期发现与metadata任务准入 | `d65c62b` / `ff649e8` / `6350a18` / `4957c9f` | 新v3原子due、原worker118条/256KiB固定R分页、可信时钟/current control/原份额fence提交永久task并消费候选。最终21组host/race及新三成员native7.82秒通过；唯一review Spec compliant / Approved、无开放问题。实际常驻worker/Watch/frontier、历史覆盖和硬32未完成 |
| 常驻到期worker与可恢复进度 | `1f7f7b6` / `a22f5fc` / `6463b5d` / `1017b66` / `9a06de9` | 真实常驻调度、严格checkpoint/pending恢复、有界原连接Watch/cache、多份额续租/等待及最终关闭登记。最终71组host/race、Linuxvet/build及新三成员9.98秒通过；同reviewer R1批准、无开放C/I/M。有界轮询限制及故障告警已披露，未实现物理执行/硬32/历史覆盖/生产切换或大N容量 |

实际 Exec 单元已验收，原始 native 实际38.83秒PASS，含32秒命令/目标接受续期/exit7/drain、实际失权、取消/Close、统一预算及原IOjoin。显式HTTP原四路由已验收；新增文件单元实际Linux PASS2.06秒无skip，UID/GID1000、控制配置不可读、Linux非法文件名422及八文件路由/二进制/编辑完成，14个健康原bridge Wait/IO与正式PID1/etcd退出均join。23项gateway host/race无skip；helper各9PASS/1明确Darwin文件名skip，Linux相应场景已实际覆盖。唯一审查无C/I，仅日志噪声M1，详见本目录 `2026-10-16-etcd-file-api-verification.md`。默认仍Redis；接续大文件传输与其余生产调用链。

大文件下载追加验收见本目录 `2026-10-16-etcd-large-download-verification.md`：配置缺省兼容原512KiB路径；显式配置后使用有总额度的磁盘暂存，不扩大原stdin/权限协议或把文件bytes写etcd。审查发现的封存后提前EOF未latch缺口已真实截短RED→修复→host/race/vet/新源码native→同reviewer限定复核闭环。默认仍Redis，生产切换0%；这项有界下载交付不等于整个文件传输或大N容量完成。

分区控制追加验收见本目录 `2026-10-16-etcd-partition-worker-control-verification.md`：控制载体不按已有sandbox数量建立常驻会话，不授予runtime/task权限；跨进程接管仍有旧读取尾部，软份额不能充当实际任务硬槽或严格无突发的全局速率保证。该基础交付不计为完整调度或大N容量验收，百分比保持不变。

到期发现追加验收见本目录 `2026-10-16-etcd-due-discovery-verification.md`。实际三成员验证原子发布、原fence失权拒绝、替代会话metadata任务提交、118＋7固定视图及真实compaction。单受控namespace最新键值28,327/32,732字节包含固定配置和历史，不能当生产每沙盒均值或8KiB目标验证。下一项为实际常驻due worker、Watch衔接及持久扫描进度；该发现链仍不等于物理清理或完整生产调度，百分比保持不变。

剩余主交付：API/Pool/Docker/Upload 的完整调用路径；分区 due/dirty/Watch 调度；FUSE/helper/collector 与实际远端同步；长期历史/槽位回收及证书维护；大量已有 sandbox 的容量、故障、部署/恢复验收；最终 Redis 移除和整分支审查。接续优先分区调度与生产任务接线，上传等缺口继续保留。Create/Lookup 的 service 单元与只读启动检查，均不等价上述主程序生产接线完成。

最新粗估：**功能代码约70%（65%–75%）/ 整体交付约45% / 生产切换0%**。新增依据是八条文件业务调用链与独立探针已实现、实际验证并审查通过；不按源码行数/提交数/测试次数计算，也没有把512KiB文件能力记为大文件/上传完成。固定整体权重保持20/30/20/15/15；对应完成度90/70/30/0/0，加权45%。API项30%仍是显式受限模式，完整Pool/Docker/Upload及默认入口替换未完成。小规模native不构成大量已有sandbox的性能或容量验收。详情见本目录HTTP、文件、Lookup与Exec验收记录。

## 2026-10-16 此前百分比明细（保留历史，最新功能估算见上节）

用户要求后续按百分比汇报。当前整体约 **40%**：采用完整生产交付项加权估算，权重保持不变，计算值39%；估算误差约±5个百分点。它不是源码行数/提交数/测试数统计，也不是已经上线40%。本次上调只来自本地普通运行时安全释放完整单元通过审查验收，不因测试/提交数量上调。

| 交付项 | 权重 | 当前完成度 |
| --- | --- | --- |
| etcd 权威元数据、协议和读基础 | 20% | 90% |
| 关闭排空、恢复同步、安全回收链路 | 30% | 70% |
| API / Pool / Docker / Upload 生产接线 | 20% | 0% |
| 分区调度、FUSE / collector 迁移 | 15% | 0% |
| 大量已有沙盒容量验证、部署恢复、Redis 移除 | 15% | 0% |

安全链已有排空/归档、本地完整同步、真实物理终止和注册发布；本地 ordinary Kubernetes/enrolled plain 的 termination composer、精确 provider 清理、原子安全释放/新claim恢复/新owner后冷receipt 已完成真实组合验证及唯一完整审查与R1闭环。cloud/none/Docker/FUSE、observer槽认证回收/长期证书和实际生产driver仍未完成，安全链不能记100%。主程序仍使用 Redis，生产切换完成度为0%。原生组件测试与固定编码/并发界限不计为大N容量或部署完成。

用户进一步要求区分功能代码完成度：当前约 **55%**，估算区间50%–60%。该口径计入已实现功能，剔除容量/部署/最终验收工作，但仍计入尚未编写的生产接线、分区/FUSE迁移；不能只按基础组件或源码行数计算。后续同时报告“功能代码约55% / 整体交付约40% / 生产切换0%”。前期协议/组件与验证投入偏多、完整业务链路推进不足，是进度慢的主要原因。

## 2026-10-15 接续

2026-10-16 受控Kubernetes profile已范围验收：c38a4cc/e7b2e30/575b714/a702c20/d92fb85与审查补测cf10144，交付共享canonical config、严格五volume/pinned image合同、正式serve-provisioned PID1、observer/cleanup完整tuple核对；唯一新Linux实际readonly mounts/observer/signedclock/ready/Prepare通过。唯一完整review的I1明确回归经同reviewerR1 Approved，保留原etcd日志M1和历史输入快照限制。详见2026-10-16-enrolled-kubernetes-profile-verification.md。这不是实际KubeAPI/CRI、创建发布/main接线或大N容量；功能55%/整体40%/生产0%暂不变。后续固定Kubernetes管理bridge方案4d3f4e4已准备，紧接编码，不重启本单元。

2026-10-16 正式launcher接线已范围验收：2976b16/3f6dfde/7e2001f/78f53ab及review修复9336fc0，v2固定节点配置、真实管理TLS只读publication、正式CLI PID1独立observer/signedclock正例和存活关闭gate拒绝。唯一review的nested分配前cap I1经8项直接rawvalidator RED→GREEN、host/race/vet及同reviewer限定复审闭环，Approved；原native启动/关闭日志与Root AppleDouble审计假设失败保留。详见2026-10-16-enrolled-launcher-transport-verification.md。下一受控Kubernetes Pod profile/provisioning方案3302feb已准备，接着编码；尚不是真实kubelet/生产driver/main。功能代码约55%/整体交付约40%/生产切换0%的口径暂不因这个接点重复上调。

2026-10-16 最新验收：本地普通运行时安全释放已完成，十二源码提交至824c1a8与唯一review的R1补测c20e586。真实链路、六enrollment变更、七HTTP实际效果丢ACK、多个native ACK、Close真实join、身份/claim/UIDRV拒绝以及最后observer Terminate响应丢失→freshclaim仅Query恢复均通过。原reviewer确认I1解决、无新增C/I/M；保留未归因前置EOF、compositescriptexit1和原日志/历史输入绑定限制。详见 `2026-10-16-local-runtime-safe-release-verification.md`，不重放旧矩阵。正式launcher enrolled config/TLS publication单元已开始：spec/plan f618f31，新增config/frame有限RED→GREEN进行中，尚未整项验收；main仍Redis。接着继续真实driver/业务API、分区与FUSE、容量及部署切换。下述早期阶段记录保留为历史，以本节最新范围验收与顶部百分比为准。

2026-10-16 新增安全释放实现切片：`6160c36/7a06877/b03dbca` 和消费者身份修复 `cb09c09`，已提交严格记录/有界owner、独立历史权限、本地公开绑定、终止/清理/原子释放编排及只读冷诊断。后续 `2895ffc` 修复新consumer对Task类型/optional字段和原编码规则的读取错误，八类型含合法opaque特殊字符与拒绝场景新增回归、Root独立race/vet通过；没有修改原validator或删除fence。

完整正例与脚本已小提交 `4831539`。真实Linux full7通过：genuine Supervisor登记发布→public archive/空localcomplete→observer实际SIGKILL与cgroup排空证明→provider cleanup-complete→换claim保留同一原entry→原子release→newowner→cold原receipt。实际HTTP为2PATCH+1DELETE，四policy absent只计skip。原worker Cmd.Wait nil、Supervisor原Wait为observer SIGKILL、observer Serve nil、etcd原SIGTERM、attach/container Wait0及精确容器清理已核对。实际runner 9,044/monitor 1,980 listed inputs的pre/post文件名+SHA精确一致，Root独立核对当前repo输入、ELF/源码归档/脚本hash；此前fixture/consumer失败和teardown日志保留。下一步是enrollment变更、真实Txn/API丢ACK、Closejoin和未决禁止重发的有限native故障；本完整安全释放Task尚未验收，不计为容量或生产切换完成。

其独立 Kubernetes 精确清理客户端 `1cb993c/2689e5d/402fbc0` 与审查修复 `e2aa59e` 已范围验收：固定七槽、实际一次请求、UID/RV preflight、原body/dial真实join、超界响应硬拒绝。唯一fresh审查的policy UID缺口已由同pair修复闭环；原host source subset/race条件限制保留，新R1匹配条件的完整输入绑定和Linux编译已核对，未执行Linux或真实kubelet。详见 `2026-10-16-task-runtime-cleanup-client-verification.md`。该依赖完成不等于主程序生产迁移完成；百分比口径暂不因组件提交数上调，仍功能代码约50%/整体交付约35%/生产切换0%。

本地完整同步单元已范围验收：`44673b4/0488e3c/3b1e4e6/30b5fc9` 与复审修复 `ba7a3dc`，接通目录/空目录、类型替换、删除策略与 excluded、临时权限恢复、实际关闭和 immutable complete。初审发现新 claim regular adoption 缺口，已通过真实延迟 GC 的 RED→GREEN 修复；同 reviewer 限定复审 C0/I0/M1 Approved，M1 为保留的 etcd teardown 日志。详见 `2026-10-15-workspace-sync-local-finalization-verification.md`。S64 单 partition 边界测试不是 full-cell 或大 N 容量验收，结果不是 release 权限。

后续运行时关闭生产者已范围验收：十个连贯源码提交至 `2b2e55d`，以及 review 修复 `ec60d6f/2bbcb12/b76ebb8`，实现真实 Supervisor 登记、独立 TLS、固定60节点原生权限、单次 pidfd 终止、实际死亡和原 cgroup 排空、持久 terminal/unknown 的新 OS 进程查询。初审两项 Important 和 R1 引入的共享 TLS 容器竞态均已解决，同 reviewer R2 无新增 C/I/M。实际 Linux 结果、原始失败和物理替换等未强制场景仍明确保留；这不是 provider 清理、安全释放或生产部署验收。见 `2026-10-15-node-observer-physical-termination-verification.md`。

enrolled publication 已范围验收：六源码小提交 `2beadb8/beb7e26/c3aa645/b43d9c2/c4a9743/1bd2f30`，以及审查修复 `8906066/491238d`。真实 Supervisor 登记/ready/64节点发布/冷读/用户执行已有组合结果；独立 backend18叶、两个 journal 安装交错和三个消费者认证拒绝通过。完整审查发现的前置超界检查、缺失消费者负例均已解决，同 reviewer R1 无新增 C/I/M；原四 epoch 和后续 nonce 断言失败如实保留。见 `2026-10-15-enrolled-runtime-publication-verification.md`。这些不是 owner 释放或生产接线完成。接着完成 etcd termination composer、精确 provider 清理和原子安全释放，再接生产业务与调度。当前无外部阻塞，主程序仍连接 Redis。

已合并feat/workspace-fuse-mount最新c14a52c，merge45ebc70，无冲突；六个受影响普通Go包race、正确标签下Helm限定回归、vet及Linuxarm64构建通过，首批漏Helm标签导致的setup exit1如实保留。详见2026-10-15-base-fuse-fixes-merge-verification.md。

暂停前的bea7f2d已完成限定复审，持久归档与原始归属单元接受，无未解决阻断项；容量M1仍保留。详见2026-10-15-workspace-archive-recovery-storage-verification.md。冷bound archive→fresh claim/认证历史query→旧intent修复/新reserve/checkpoint本地regular-file恢复已范围验收，源码509632a/d7dc18b/fbe66c1/a46d203/5a78262及修复f68923a/d8e6eb7。两文件、跨claim继续、65文件、partial-write、terminal/GC丢ACK、物理Close、0000/0444恢复通过；初轮两个Important和Root新增权限缺口均限定复审闭环。见2026-10-15-workspace-sync-local-recovery-verification.md；历史失败Wait缺口如实保留，不称完整sync/安全释放或容量完成。自动进度跟进恢复，每10分钟汇报；整体生产迁移仍未完成。

独立的一致metadata读/只读MOD复核单元已验收970ce46/3b5178f，测试最大namespace修正04ac585；唯一review及限定复审C0/I0/M0，真实三成员限定组与host race通过。见2026-10-15-etcd-coherent-read-view-verification.md。它补齐生产读接口的一部分，尚未接manager/API或替代Redis，不代表性能/容量已验收。

基分支新增Redis持久probe签名key，应纳入etcd生产接线和旧版本排空，不能在移除Redis时直接丢弃fencing历史。

新增probe原生密钥存储源码226b543/dc08b41已范围验收：operator显式seed永久CAS、API只读owned key加载、未知回执只读仲裁及严格wire/故障边界。独立源码审查C0/I0/M0；原始容器退出码未记录的环境M1透明保留，down0与精确清理已核对。见2026-10-15-etcd-probe-control-key-verification.md。main和旧Redis loader尚未切换，启动接线/原seed导入/部署权限仍需完成。

## 2026-10-09 整体交付状态

| 交付项 | 当前状态 | 还需完成 |
| --- | --- | --- |
| 权威元数据与关闭/排空基础 | 组件已验收，业务尚未接入 | 真实driver和管理器调用组合 |
| 安全恢复、同步和释放 | 实现中 | 新claim恢复、实际writer排除、完整同步、runtime终止和owner安全释放 |
| API、Pool、Docker、Upload生产接线 | 缺口已定位，尚未完成 | 原生领域组合、真实driver和主程序切换 |
| 分区调度、FUSE/collector | 尚未完成 | due/dirty/Watch、有界worker，去掉随已有沙盒数增长的短周期扫描和常驻循环 |
| 容量、部署恢复与Redis移除 | 尚未验收 | 大N与长期故障测试、部署/备份恢复/旧版本排空、依赖移除、整分支审查 |

当前归档持久化单元已提交9个小改动（2719d7b、0d213d6、8778ead、e14c92c、77f0abd、95c9999、2773087、5a742a3、bea7f2d），实现有预算的固定槽文件、原始接收/目标归属和只读冷诊断。首轮审查的关闭竞态经R1修复，复审发现的同prepared并发重复收费经R2修复。最新R2源码提交bea7f2d，针对性测试通过，仍待同一审查员复审；本单元未验收。下一完整恢复执行单元的接口准备已保存，不重放已验收组件。
针对性故障、关闭与文件异常测试通过；整组运行发生过OOM，分组通过不替代完整可靠性或容量验收。恢复后的执行权限和安全释放仍属于后续实现。

四天仍未达到生产迁移完成：协议拆分、反复验证和证据整理占用过多时间。后续按完整功能单元推进，每个单元一次独立审查，小步提交保留；不扩大可选测试与统计，不重跑已验收组件。目前无外部阻塞，不给未经依据的整体百分比或完成日期。生产缺口见 `2026-10-09-etcd-production-wiring-preparation.md`。

2026-10-09 18:04（Asia/Shanghai）用户要求提交当前进度后切分支修bug：暂停迁移实现，所有子任务均已停止，自动跟进配置为PAUSED。接续从bea7f2d的R2限定复审开始，不能重放前序单元或把未审查修复标为验收；OPEN OWN保留原始日志和作者/审查记录。无关Sentinel文档保持原有工作区改动，不纳入迁移提交。

最新已验收源码 `7fd87c1`/`a483a7b`/`e398f41`：v1兼容/v2严格持久checkpoint、进度与generation GC原子64节点提交、unknown同点只读和Close等待。成功覆盖39不同native叶、最终host race14叶，65文件属于准确记录的提交前snapshot；较大批次14PASS/23FAIL和SIGKILL原因UNKNOWN仍保留，不称整轮可靠性通过。唯一review Missing0/Extra0/C0/I0/M1 Approved；Root354 artifacts、12源码绑定/9binary/目标包231编译输入、8环境24原始child join及独立清理通过。见 `2026-10-09-workspace-sync-checkpoint-verification.md`。正常恢复研究已形成，下一步补有预算归档持久manifest、重新打开与新权限接线；该研究不是恢复验收。主程序仍Redis，安全释放、生产接线、分区/FUSE/collector、大N容量、部署恢复与Redis移除均未完成。

已按用户要求合入基分支修复 `52b4d4f`、`d543cf3`，合并提交 `7bca73a`。无冲突；三个受影响包 race/vet/Linuxarm64CGO0编译通过。见 `2026-10-08-base-quiesce-fixes-verification.md`。

新增已验收功能提交 `61fe19a`：原始归档消费owner、当前native权限、有界TAR/PAX私有body和所有关闭路径的物理等待。实际native24项与host5项通过，native零跳过/零警告，vet/Linuxarm64编译通过，独立review无问题。见 `2026-10-08-workspace-sync-source-verification.md`。原始45秒会话实际过期后继续消费、真实续约已测；当前重复整归档SHA存在多文件IO成本，尚不是性能/容量或安全释放验收。

已验收组件包括原生 etcd 元数据与永久 owner/事务回执、原始 operation/claim 和命令 fencing、运行时关闭与用户排空，以及实际 workspace 归档生产者。最新两个功能提交：`b5bdca2` 封闭 TLS 归档传输接口；`f1e3086` Supervisor 原始生产者、独立截止时间、历史完成查询和物理关闭等待。对应验收见 `2026-10-08-supervisor-workspace-archive-verification.md`。这些组件验收不等于整条生产链路完成。

新增已验收功能提交 `bd46b2b`：后端私有归档接收器，含原始 spool 的磁盘/索引/并发额度、真实 TLS 数据与签名/TAR/EOF 校验、物理关闭等待和保守退款。限定新测试/race、包 vet、Linux arm64 编译通过；独立 review 无阻断项。验收见 `2026-10-08-backend-workspace-archive-receive-verification.md`。这仍是接收组件，尚未完成当前 KV 所有权约束的发送链路。

新增已验收功能提交 `90d23d0`：原始归档命令签名、不可变 etcd 记录、当前 claim 与历史排空身份绑定、固定有效期和保守响应丢失处理。新 host/native race、vet、Linux arm64 编译通过，独立 review 无阻断项。见 `2026-10-08-backend-workspace-archive-command-verification.md`。

新增已验收功能提交 `4e3c3c3`、修复 `d97d221`：当前 claim/KV 发送屏障、长接收期间真实续约、接收额度背压、接收后新鲜权限检查及物理关闭等待。最终新 native race15项通过，vet/Linux arm64编译通过，独立审查与针对性复审无未解决项。见 `2026-10-08-backend-workspace-archive-delivery-verification.md`。测试使用真实 etcd 和 TLS 协议 fixture，尚未接到实际生产运行时或 provider，不是安全释放与容量验收。

新增已验收功能提交 `4ca3e45`：不可变规范同步计划绑定实际 workspace identity、排除规则和删除策略；新预留入口在错误目标时、查询/写入前拒绝，且不消耗原始申请。新 native race8项通过，vet/Linuxarm64编译通过，独立审查无问题。见 `2026-10-08-workspace-sync-plan-verification.md`。计划仅为结构归属，当前原生 provider 物理身份和消费接线仍未实现。

后续按以下依赖顺序推进：

1. 原始归档消费者与规范计划已验收；继续实际可信provider worker、物理目标校验与有界body接线。接线研究见 `2026-10-08-trusted-workspace-sync-preparation.md`；重复归档完整性扫描必须在容量验收前优化。
2. 每个真实远端请求（包括 SDK 重试/跳转/分片）的持久意图、fencing、确定结算或 sticky unknown；本地 provider 的可信物理 IO 证据；结合实际运行时/基础设施终止，完成安全释放链路。当前归档能力只能保守拒绝重建，正常 manager 故障恢复仍需实现和实测。
3. API、Pool、Docker/Upload 与主程序生产接线；分区 due/dirty 调度，移除全量扫描和按已有沙盒常驻的控制循环。
4. FUSE/collector/可信同步、长期空闲运行时按需证书维护；验证 N 增大时 idle 成本与活跃工作背压。
5. 大量已有沙盒的完整容量/性能验收、etcd 部署/备份恢复/旧版本排空，最后移除 Redis 并做整分支审查。

当前无外部阻塞，剩余工作仍然较多。容量验收须测量实际 MVCC/WAL/复制、调度/API/FUSE 成本和大 N 空闲行为；现有组件测试与编码大小样本不能替代。每个连贯改动继续独立验证、审查和提交，源码修改使用 apply_patch。

新增已验收 `a6d340c`/独立审查修复`9e49f60`：独立 recycling profile、严格free/occupied/terminal slot编码、只读operator配置pin与新旧入口隔离。最终scopedhostrace149叶/真实etcd metadata13叶PASS、vet/cross/format通过；限定复审C0/I0/M1（server环境告警按限制披露），Root167artifact/302source/3336inputs/10workingGitcompiled绑定与两轮exactcleanup核对。详见`2026-10-09-workspace-sync-recycling-profile-verification.md`。这还没有实现slot复用：当前接续generation reserve/local gate，再持久terminal与GC，旧永久收费记录仍保留。主程序仍Redis；安全释放、生产接线、分区/FUSE/collector、大N容量、部署恢复和Redis移除均未完成。

新增已验收源码`7391a5e`与修复`27feaf6`：原始未读body的持久put意图、固定分区slot跨Backend共享背压、未知响应不重发与出生pin诊断。基线native38实际叶子零跳过/警告；独立review发现空body已完成读取的缺口，针对性RED→GREEN修复及复审通过，剩余C0/I0/M0。详见`2026-10-08-workspace-sync-intent-verification.md`。这是native元数据组件，实际可信worker/provider派发和结算未完成，不能把归属记录当OS UID或安全释放证据；S1..64的逻辑预算上界不等于实际MVCC容量测试。

新增已验收源码`cdb5612`/脚本修复`b75fbeb`：实际非特权worker的原始kernel/process身份，非零UID/GID、零cap、NNP、固定boot/start/ns/exe及不可复用incarnation。host34实际模型叶、五个实际native容器和原始子进程身份通过；独立review发现attach超时句柄丢失，独立修复并以三真实child生命周期场景/单nativepositive和限定复审验证，剩余C0/I0/M0。详见`2026-10-08-workspace-sync-worker-verification.md`。它不能代替Deployment/凭证隔离、物理provider target、native派发与终态证明；下一步继续这些实际IO接线，主程序仍Redis。

新增已验收源码`fac3953`/`7bc4ff6`与review修复`6b411a0`：原始非特权worker、只读local root FD与v2永久意图绑定，只有原始明确ACK/postguard可进入一次性native写前门；保留50历史pins，实际59nodes/11517requestbytes/2629replybytes。原native21叶、修复15实际local叶、真实超时保留→释放→原attach Wait→独立reap通过，限定复审C0/I0/M0；失败试验和server日志完整保留，启动错误归类已纠正。见`2026-10-09-workspace-sync-local-gate-verification.md`。当前开始实际本地文件写入/hash/fsync/Close与进程内witness；持久终态/退款、安全释放、生产接线、分区/FUSE/collector、大N容量与Redis移除仍未完成。

新增已验收源码`96c21cc`/`2f59c93`：实际local regular-file有界写入/hash/fsync/Close、原始进程内witness、截断前身份拒绝、mode0000与8路物理关闭等待。24不同实际Linux叶同源分批PASS/零SKIP（23+1），完整轮旧setupdeadline失败如实保留，不能称整轮全绿；host限定race14PASS/24SKIP及vet/cross/format通过。独立reviewC0/I0/M0，Root426artifact/16source/两个Go编译快照/42exactinventory核对通过。见`2026-10-09-workspace-sync-local-put-verification.md`。永久intent/slot仍charged，下一步原生持久terminal与有预算回收/恢复；完整sync、安全释放、生产与大N性能验收仍未完成，main仍Redis。
