# Sandbox etcd 状态管理方案独立审查记录

日期：2026 年 10 月 6 日。对象：[完整方案](../specs/2026-10-06-etcd-state-management-design.md)。当前源码事实基线：HEAD b99b823。本次交付是设计与研究文档，未修改应用实现或运行环境，没有执行容量压测。

用户关切是大量已存在的沙盒。审查优先核算存量N的持续成本、历史workspace U、FUSE数量F、申请churn和恢复尾部，并单独评估请求峰值。

## 研究与审查方法

主线读取当前状态Repository、生命周期协调、workspace申请/租约、ordinary/FUSE pool、runtime健康与探针、Redis安全模式、Helm迁移hook和SDK。三条独立审查线分别读取代码与方案，并使用etcd、Redis及Kubernetes官方资料核对语义和限制。每条线经历两轮全面审查和最终针对性复核；review只读，主线负责修订。

| 审查线 | 第一轮 | 第二轮 | 最终复核与整理 |
| --- | --- | --- | --- |
| 一致性、故障交错 | 5项P1、3项P2，要求修订 | 首轮关闭；新增launcher执行边界P1及3项P2澄清 | P1为0；回执key示例1项P2已在整理时修正 |
| 存量容量、稳态成本 | 8项P1、3项P2，要求修订 | 首轮关闭；新增幂等/GC与journal共2项P1、3项P2 | 无剩余或新增P1/P2 |
| 迁移、部署、运维 | 4项P1、5项P2，要求修订 | 首轮除launcher边界外关闭；新增readiness周期exec成本P2 | 无剩余P1/P2 |

以上数量按每条独立报告统计，重复发现不计为不同业务问题。P1指设计实施前必须补齐的阻断要求，P2指协议或文本澄清；问题关闭表示方案已规定解决契约，不能替代实现测试。

## 关键发现与最终处理

| 发现 | 修订后的方案 |
| --- | --- |
| etcd token消失后，迟到exec仍可能到runtime | etcd关闭准入、target持久关闭gate并ACK，再排空两层operation；旧gate epoch永不复活 |
| 到期任务积压会使过期记录继续准入 | control内expires_at、受控UTC误差预算、事务及实际执行前检查；时钟未知暂停准入 |
| 创建总request回执与多阶段事务混用 | 分离总request、logical stage/attempt receipt、target执行receipt；commit/abort以同attempt仲裁 |
| 快照恢复的递增epoch可被复用 | 不可复用restore ID、全部授权比较、target可信安装与旧credentials隔离；prefix阻塞/quarantine |
| 进程退出不证明远端写已结算 | owner释放需发送端隔离加远端settled/fence证据；未知S3写保持pending |
| 每API复制全量snapshot/due隐含O(N)内存 | control与snapshot分族、有界LRU、近期due窗口、有限队列、按需读取与shared Watch |
| 独立operation Lease的成本被低估 | 至少五类正常RPC加读/重试；KeepAlive、Raft proposal、key mutation分别测量 |
| 申请幂等、历史fence、journal可以长期累积 | 默认1h幂等窗、按终态产出率配置GC、历史U/未决工作集预算、新建背压；target journal有安全GC及水位 |
| HPA/rolling把cell预算倍增 | 固定budget-slot份额、全cell持久preparation slots、原session guard；失租停新工作 |
| 健康汇报不能只传变化却刷新全部沙盒 | 每条freshness tuple、boot/sequence/chunk/ACK校验；网络、分布式cache、本地probe和kubelet探针单列 |
| context timeout不意味着挂死probe退出 | 真实底层执行未退出不释放并发slot，unknown/告警或可终止诊断进程 |
| sidecar自身执行会改变语言镜像与资源隔离 | 实际sandbox容器内受保护PID1 launcher；sidecar仅认证协调；Docker同runner；全部用户后代排空 |
| 同UID与继承FD可能暴露管理凭证 | 用户UID0/同管理UID配置拒绝；降权、清groups/capabilities/FD，proc/ptrace与IPC隔离验收 |
| 新镜像已删Redis却被旧配置hook用于drain | 升级前使用installed旧镜像独立Job，冻结HPA/旁路，旧API全部scale0后完成全源drain |
| scope改名或误接空etcd绕开owner唯一性 | 固定authority、存储/runtime/cluster身份校验、operator初始化、chart fingerprint、共享prefix注册 |
| 单cell幂等与未来workspace多cell路由冲突 | 本轮只承诺单cell；跨cell全局路由/幂等契约在阶段六单独定稿后才开放 |

## 仍需实施验证的结论边界

1. 所有容量数字、默认限流及SLO均为模型或起始配置。必须采样真实记录大小、MVCC/churn放大、GC服务率、journal工作集与健康探测成本，再确定N上限。
2. 大N元数据压测与同N真实Pod/FUSE运行压测分开。普通空闲状态不短周期续租，全cell审计仍O(N)，FUSE检测/汇报及runtime资源仍随N增长。
3. launcher/agent/mounter是实际新增交付项。真实Docker与Kubernetes必须验证执行环境、UID/FD/凭证隔离、全部后代排空、迟到能力、重启closed与资源limit归属。
4. 三成员etcd验证迟到Txn、Lease到期、commit/abort、NOSPACE可能已提交、Watch compaction、同时重启与leader切换；纯内存mock不足以证明协议。
5. 单环境旧镜像drain、snapshot后新增writer的恢复阻塞、无法映射prefix的scope冻结、清空Redis运行依赖和SDK兼容都需要演练证据。
6. 无通用对象存储write fence时，未知远端写可能长期pending。多cell全局幂等和root用户身份支持不作为第一版已经解决的能力。

## 最终独立复核原文

以下保留三条审查线的最终报告原文。原文行号对应审查时506行方案快照；随后仅完成回执key/attempt措辞、明确身份与FD限制、验收项及审查记录整理，当前行号可能变化。


### 一致性最终复核

#### etcd 设计最终针对性复核

对象：`docs/superpowers/specs/2026-10-06-etcd-state-management-design.md`，当前506行。只读复核，未修改仓库。范围限第二轮1项P1与3项P2，不扩展新架构需求。

结论：第二轮P1在设计层面已关闭；3项P2对应的协议与范围要求均已补齐。剩余P1=0。仅有1项非阻断P2文本澄清，可在整理文档时顺手修正；一致性设计可以进入实现与验证阶段，不再要求另一轮完整设计review。此结论不代表launcher安全或etcd容量已有实现验证。

##### 逐项关闭

| 项目 | 当前行号 | 结论 |
|---|---|---|
| sidecar误执行用户代码 | 231、233、237、239 | 已明确sandbox容器内PID1 launcher实际执行；sidecar只认证/协调/观察；保留原镜像、namespace、cgroup及安全约束，无nsenter/CRI主机特权；file也按sandbox身份与目录FD边界执行。关闭 |
| 用户后代setsid/doublefork逃过进程组排空 | 235 | 明确PID1/subreaper登记所有后代、关闭准入后冻结/终止全部用户后代、独立PIDnamespace、禁止改namespace/cgroup；证明缺失或state丢失默认closed；container incarnation/bootID防同PodUID重启复活。关闭为可测试的安全要求 |
| task epoch CreateRevision/ModRevision混用 | 199、227 | 固定claimCreateRevision；claim/guard实际附原taskLease，checkpoint/auth/complete Txn compare精确值、LeaseValue、CreateRevision及owner/control。续租不会改变epoch，ID不重建。关闭 |
| logicalstage aborted后无法合法重试 | 203-209 | 207区分logicalstage与txnAttempt，新attempt有新guard/receipt，旧aborted不覆盖，未知外部effect不新dispatch。关闭 |
| future多cell幂等路由不成立 | 151、360 | 当前只承诺单cellprincipal/key唯一；跨cell需要持久请求路由仲裁或公开cellscope/token，阶段六定稿前不开放。明确不假装跨独立etcd原子Txn。关闭为第一版范围约束 |

##### 非阻断P2文本澄清

115行key示例仍是`p/07/stages/requestID/stageID/receipt`，203行也仍说每stage receipt。207行正确规定同一logicalstage的新attempt必须有新receipt。建议示例改为`p/07/stages/requestID/logicalStageID/attemptID/receipt`，203/205明确仲裁的是对应txnAttempt的receipt；logicalstage journal引用当前attempt。这样不会让实现者误以为新attempt复用已aborted的同一个receipt key。

此处已有207行明确的规范性要求，故不构成新的P1，不阻止进入实施，也不需要扩展成新设计阶段。

##### 实施必须验证的既定要求

1. launcher身份边界：用户UID/GID必须与管理身份隔离；不满足时拒绝配置。子进程无管理credentials、FD、supplementarygroups或capabilities；测试恶意IPC、proc读取、管理目录文件API访问、root或同UID配置、资源限额、rawexec旁路。不能仅实现“sidecar调launcher”就宣布已通过隔离。
2. 全部后代排空：setsid、doublefork、后台daemon及并发fork不能漏出证明；关闭gate与fork/命令接收并发必须排序。supervisor崩溃/重启、containerincarnation变化时默认closed，不恢复旧能力；证据不成立保持pending。
3. claim与回执：真实etcd测试guard已过期时迟到Txn、guard创建unknown、End/Revokeunknown、marker到期、旧claimcheckpoint、新attempt重试已abortedstage。commit/abort只能一个获胜，不覆盖旧receipt，不复活旧ID。
4. targetjournal243行：terminal后不延长旧capability，GC前所有旧capability已过期或gate/restore永久fenced；unknown/state丢失不发第二次命令；容量拒绝新危险任务仍允许cleanup查询。
5. 原先外部写/恢复边界继续有效：lease或进程退出不代替provider settled/fence证据，unknownS3/FUSE保持owner；新恢复ID可信target安装与scope/prefix quarantine先于新allocator开放。

上述是文档既定不变量的实施验收，不是新增scope。可以关闭本次设计一致性审查；实现完成后以实际故障注入结果确认。


### 容量最终复核

#### 第三轮最终针对性容量复核

对象：`docs/superpowers/specs/2026-10-06-etcd-state-management-design.md`（本轮读取506行版本），2026-10-06。只读复核；没有运行性能测试，没有修改仓库。

**结论：第二轮新增2项P1、3项P2均已在设计文字层面关闭；本轮没有剩余或新增P1/P2。容量设计可进入实施与验证阶段。此结论不等于已证明10万或百万实际sandbox承载能力。**

##### 针对性关闭证据

| 第二轮问题 | 当前证据 | 结论 |
| --- | --- | --- |
| P1-A 成功结果保留/GC稳态 | 213默认幂等窗口1h，24h需容量预算；215 GC500records/s仅起点，明确服务率>终态产出率及余量，lag/工作集超预算收紧new admission；217显式λ_new×retention×全部request/stage/intent工作集；337纳入backend公式 | 关闭；500/s和1h都不是固定承载保证 |
| P1-B helper本地journal无界 | 243有operation ID/digest/最大cap期限/终态/epoch，terminal不延长，终态且能力过期或永久fenced才GC；unknown保留、状态丢失关闭；256MiB上限、70/85%告警/拒绝、cleanup查询保留；233固定RAM与原cgroup配额实测 | 关闭；journal不保存大stdout，也不按每Pod预分配256MiB |
| P2-A 全cell份额部署 | 263 startup/relist cell预算；265明确用已有etcd固定budget-slot，session guard领取、份额总和有界、HPA/rolling只借现有份额、失租停止新工作、旧inflight按chunk/请求时限计尾部；297 preparation32共享持久slot | 关闭；没有引入第二数据库 |
| P2-B timeout后probe泄漏 | 279明确实际并发8，底层syscall未终止不释放slot，unknown及告警/受控子进程；281 kubelet readiness也迁为驻留只读health，额外F/10成本单列 | 关闭；取消context不被视为资源已退出 |
| P2-C multicell幂等路线 | 360明确本轮仅singlecell，跨cell目录仲裁或公开cell幂等scope留阶段六定稿，不提前建额外DB；151的未来约束应按360这一后续界限阅读 | 关闭；不是本轮隐式新增存储服务要求 |

##### 大量已有sandbox的稳态成本核对

- 253/257/259采用bounded摘要LRU、近期60s due窗口与固定revision List+Watch，没有恢复每API整库snapshot镜像。132/134 snapshot按immutableversion有界LRU、低命中readbytes单独记录。
- 307普通idle没有按sandbox短期续约/重写；312低频audit仍O(N)，而且全cell预算。不能将这理解成N增长后总成本不变。
- 313/314明确活跃operation约5类RPC和读/重试、KeepAlive与Raft分开。空闲operation数不会按N增长，长operation、用户stream与runtime launcher仍有实际成本。
- 271-281移除FUSE每5s Pod GET/exec，保留F/5本地probe、F/Treport freshness tuples、F×Bhealth缓存、F/10 kubelet readiness以及runtime/helper固定RAM和磁盘。明确不是零成本也不是每worker常数内存自动覆盖全部FUSE状态。
- 293-297 warm/runtime/preparation三种quota分离，freeindex按配置槽位数计量，未知创建不早退槽；静态quota分区的利用率损失被明确接受，没有额外hot global counter。
- 334-341计算live、MVCC历史、pending、幂等保留、历史workspace U、guard、configuredslots和freepages；352/364区分metadata capacity与真实K8s Pod/节点/容器限制。12cell仍只是带假设例算。
- 新默认1h算例100new/s×1KiB×3600=351.6MiB payload，24h=8.24GiB，与217一致；backend倍增另计。按产生5个终态records/request举例，100new/s就耗500records/s、没有GC余量，应触发215约束，而不是认为“默认500足够”。此为实施时对规则的应用，无需新增设计范围。

##### 实施验证要求（原范围内，不新增架构）

1. **GC/幂等工作集稳态**：固定存活N，持续Create/Destroy churn，采样真实每request终态record数和bytes。跑跨默认1h窗口及代表性24h soak，观察collectable backlog有界、服务率高于产出率、低优先GC不被饿死，admission在预测达到quota前收紧。配置24h时另算足够预算；不能只测N不变即宣称容量稳定。
2. **journal与迟到命令**：同runtime长期operation压力，验证terminal后拒绝cap续期、不同digest重放拒绝、GC前后旧命令不被重复执行；unknown保留及journal70/85%/满盘行为，cleanup仍可查询/完成。重启丢状态保持closed，同时统计真实disk/RAM，而非靠256MiB名义上限推测。
3. **预算与故障**：worker HPA/rolling/同时restart、sessionLease过期与迟到Range/任务交错，验证没有复制share/burst，吞吐允许保守下降，实际准备不超32；新worker不把本地tokenbucket初始化为无界全额burst。测chunk/请求时限造成的有限inflight尾部。已有slot/freeindex借还、90/99/100%使用率按既定矩阵验证。
4. **FUSE挂死与稠密节点**：让真实probe阻塞，确认timeout只产生unknown，真实执行数量不随时间增长；8slot耗尽的恢复/告警和readonlyreadiness行为受控，不能回退全Pod exec。测F/5 probeCPU、freshness tuple bytes、cache、kubeletF/10 probes和helper固定内存。
5. **大N与恢复**：普通idle 10k→100k、变化速率相同，验证短期事务/runtime请求没有按N十倍增长；audit、live bytes与真实runtime资源单列。同时重启API/worker/collector并leader切换，在4partition/20MiB/s预算下测ready覆盖与恢复尾部。millionmetadata使用真实etcd和模型records，million真实Pod仍以阶段六多cell及实际资源为前提，不能换概念验收。

上述测试落在当前483/485/487/489与阶段三至五的验收范围；无需因此增设另一个数据库、提前交付多cell路由或继续扩展本轮设计。实施发现新结果再按证据调整容量参数和SLO。


### 迁移与运维最终复核

#### 第三轮最终针对性迁移/运维复核

对象：`docs/superpowers/specs/2026-10-06-etcd-state-management-design.md`最新版本。只读，不改仓库。范围仅复核round2遗留与迁移/restore/去Redis无回归。

结论：本审查范围内无未关闭P1或P2；round2最后1项P1和新增1项P2已在设计层关闭。可以据此进入实施计划与协议验证。此结论不表示launcher安全、etcd容量或实际migration已被实现验证。

##### 遗留关闭证据

- helper执行驻留P1关闭（231-239）：实际用户程序由sandbox容器内受保护PID1 launcher执行，sidecar只认证/协调/观察；Docker同launcher+host agent，不加nsenter/CRI主机特权。文本明确保留原镜像语言、workspace/network/cgroup资源归属；父管理身份与子原UID/GID、supplementary groups/capability清理、no_new_privs/seccomp/LSM、受保护IPC/Secret/状态目录；排空覆盖setsid/doublefork全部后代，失证据或launcher状态丢失gate保持关闭；PodUID相同容器重启也验证container incarnation/boot ID。未再把独立sidecar默认当能执行另容器程序。
- readiness持续fork成本P2关闭（281）：周期10s exec改驻留HTTP/gRPC只读探针，仍把F/10 kubelet探针、Pod状态/系统心跳及helper固定RAM列入成本。探针不能有管理能力/凭证，不降低频率掩盖成本。

##### 迁移与恢复回归复核

- 95-99继续保持稳定authority、独立operator meta初始化、schema/prefix/cell/storage/runtime/etcd身份校验与chart guard，应用不能误接空后端就开新allocator；证书endpoint轮换不改变authority。
- 450-452继续要求关入口、冻结HPA/旁路、旧API全部scale0待无Pod、单个installed旧镜像digest+完整旧配置migration Job；Job不服务/warmup/refill。旧image drain不能依赖新image配旧Redis配置hook。451全源审计包括legacy session/multipart staging/active/owner/pool/finalizer/policy-only残留，pending不进入新allocator；457回滚先新版drain并禁止两套分配器共存。
- 403-405继续要求不复用restore ID、撤旧credentials、受信安装target、固定所有授权epoch；restore前后未知runtime/FUSE/sync writer与远端未结算操作保守阻塞。可映射prefix建owner/quarantine，无法映射冻结authority scope，revision bump不冒充对象写fence。
- 429-444正式覆盖旧隐式原子能力和源码/依赖/bootstrap镜像/chart schema/helpers/RBAC/Secret/PVC/init/config/scripts/integration tests/当前手册/外部CI及Mockoon契约；历史文档allowlist，旧Redis配置报错。不存在以删测试代替回归或保留可执行旧后端分支的回退。
- Docker仍是本地daemon单实例（11、419与475）；multipart领域CAS/intent（418）保留；本地HTTP限流与cell创建预算仍明确分开（300-302附近创建背压章节）。

##### 实施必须提供的验证证据

这些是既有设计退出条件的具体验收，不是新增范围：

1. 两runtime实际用户代码仍在原语言/文件/网络环境运行；CPU/memory/pid limit归属与launcher固定成本测量，恶意子进程读父凭证/写IPC/control卷/残留capability尝试失败；setsid/doublefork后台writer能被完整排空。所有exec、stream、tar/file IO路径经过runner/gate，UID相同容器重启与迟到capability不能复活。
2. launcher/agent/mounter重启的durable gate/receipt恢复、unknown保持关闭；container boot身份用运行时精确ref，pool/chart中的是可稳定验证的协议/安全契约，不能把未来实例UID作为可预渲染静态值。
3. FUSE startup/prepared/readiness保持原mount语义，periodic readiness无CLI fork/远端对象写，health接口不泄露凭证；报告helper RAM/CPU、F/10探针、collector/report实际成本。
4. 从真实installed旧chart+旧Redis配置演练maintenance→无旧API/旁路writer→旧digest独立Job drain→全源报告→新etcd-only配置；任意flush/sync/terminate pending阻止新allocator；新流量开始后的rollback先完整drain。
5. 恢复旧snapshot时保留一个snapshot后新建且仍能写的runtime，验证同prefix申请必然被阻塞；无法映射prefix冻结scope。重复恢复同snapshot用不同restore ID，旧证书/能力无效，stage NOSPACE/response loss正确resolver不误删owner。
6. tracked源码/config/chart/scripts/tests/现行手册Redis运行引用检索与allowlist审查、真实三memberetcd回归、SDK原调用兼容与幂等错误/query语义、Docker persistent/multipart恢复；etcd release/PVC独立于API release lifecycle。

迁移/运维设计的阻塞项已闭合；下一步应按现有阶段实施并测试，不需要为本轮继续扩张架构范围。
