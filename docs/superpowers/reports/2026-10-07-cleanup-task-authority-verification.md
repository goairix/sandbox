# 清理任务元数据单元最终验收

日期2026-10-07；分支`codex/etcd-state-management`，原分支来自`feat/workspace-fuse-mount`的b99b823。完整审查范围`23e4337..9f31ae55319cbf8f6b69cdb7b20165a69e859ccc`，13个分步提交，14个新增Go文件。三块Task都已独立spec/quality审查，通过一次完整新单元review：Critical0、Important0、Minor7。无产品修复波次；七项非阻塞fixture/evidence观察逐项保留，Ruling9记录决定及其代价。

交付严格永久task/intent/link/checkpoint；同原control revision的active→destroying准入关闭；self/origin/context sealed的原Lease TaskClaim、续租/清理及保守checkpoint CAS。只给实际领取任务发Lease，不新增每空闲sandbox client/goroutine/timer/ticker/Watch。事务预算destroy43/64、checkpoint59/64，编码与预算在Grant前校验。永久记录不附TTL；只支持pending/needs_reconciliation，不把checkpoint字符串、owner记录、空token或本地cancel当物理完成。

## 实际门禁

- Task1：真实fixture4top/10subPASS，0FAIL/SKIP，21raw均0；严格codec行为RED与后来UTF8输入断言失败/修正历史分别保留。首次named-volume setup失败发生在Go运行前，18raw原样保留。
- Task2：4top/38subPASS（37native namespace+1pure），0FAIL/SKIP，25raw均0；控制准入两种排序、竞争、晚到Stage和原receipt仲裁实际运行。
- Task3：三次fresh同配额batch6top/97subPASS（95native+2pure），0FAIL/SKIP，75raw均0；原TaskClaim40种fence破坏、未知/迟到/取消、连续checkpoint、claim turnover与旧Stage围栏实际运行。
- 实际Darwinarm64Go1.25.6 CGO1 race，对Linux/arm64etcd3.6.15三成员加foreign。所有接受fixture成员运行前后健康、无OOM，精确项目容器/卷/网络清理清单空。独立review核对121accepted raw的完整哈希/输出；Root核对685current/copy/exactreviewHEAD源码、3compiler及4份688Rootfixture/source manifest。Task1公开诊断读取函数体与其accepted source一致。
- 当前候选产品源/测试在a5bd5ce冻结；9f31ae5仅验收文档。历史fullhost/build有2个后来改动的测试，最终fault-host/vet/diff117source hash完全一致；实际native消费最终候选。缺新API编译失败不冒充行为RED，host SKIP不冒充native PASS。

## 未解决的真实失败和告警

首次Task3完整selector失败：三个64MiB主成员真实OOMKilled/Exit137，三个status不可用，3top/85sub仅partialPASS、3top/12subFAIL。15clientWARN（10NotFound+5network/deadline等）及68metadataWARN+1startupSchemaERROR完整保留；根因和修复均未证明。Ruling8三次fresh相同64MiB/0.5CPU/128PID批次只补齐协议场景，不是内存修复、不中断full selector或大N容量证明，没有加资源或重置失败为成功。

Task1的2canceledTxnWARN、18metadataWARN+1启动schemaERROR保留，原成员恢复未认证。Task2的3clientNotFound+9applyWARN、16configuration+1schemaWARN保留。Task3接受批次的11clientNotFound+33applyWARN与48configurationWARN分别分类；metadataERROR0。fresh成员storageVersion3.6.0仅为直接诊断，不追认旧成员恢复或source/binary映射，不能声称warning-clean或生产安全。

现有Stage成功guard比较value/Lease/Create、无Mod成功CAS或本地单调截止，这是Ruling7保留的基础协议。新TaskClaim完整四字段/本地deadline已实现；parent cancel不能远端撤回既有prepared Stage，真实原taskLease revoke/expiry才使它无法commit。若加强Stage原语，必须独立review既有调用点，不能把本次TaskGuard测试说成全部Stage加固。

## 后续强制交付

本单元不调用runtime、不删除owner/index/control，不证明远端End。下一步合法TaskClaim持久关闭exact data gate；关闭前accepted命令仍可能晚spawn，必须纳入后续helper与etcd-prefix双排空。现有runtime transport把query与admission耦合，关闭后的认证terminal/unknown诊断必须保留，同时拒绝新start；不能为查询重开gate。TaskSigner用途与实际activated TLS issuer身份要匹配，不能静默扩大信任。

之后仍需CloseAll/排空、sync/flush、remote settled/fenced、exact runtime/policy/mount清理和条件安全释放；publishing/exclusive/cleanup_pending/due恢复、共享scheduler/dirty、背压/安全GC、pool/upload、Manager/runtime/images/FUSE接线、部署/restore/drain与完整Redis移除。生产仍使用Redis。现存N及历史U/申请量/保留窗的完整master性能/资源/长稳矩阵尚待执行；不声称速度提升或N容量上限。

## 完整保留与来源

完整最终审查：`2026-10-07-cleanup-task-authority-final-review.md`。每项reviewer原始declined理由与Root处理在下文逐项保留，56项不分组合并丢弃；7项Minor均保留。`2026-10-07-cleanup-task-authority-rulings.md`按原顺序保存9条完整裁定、原因和代价。当前及前一单元OWN继续保留，未在向用户逐条公布全部裁定前删除。

完整evidence archive `2026-10-07-cleanup-task-authority-evidence.tar.gz`：19814731字节，SHA256 `e5588d2c382f5455f023eb72956d412afe7e4144c61cffe851f292e71d59d031`；2328个regular成员，包含每一个当前OWN原始文件和完整源码/编译器副本、raw、报告、审查、ledger、审查包及inactive后续笔记，无排除。manifest SHA256 `bf22a04b4036acc432dcf85cea4263306299c3e2b93f5844526fdc2d55f92690`；生成后逐成员检查size/SHA与精确membership，原始failed/setup/host-skip也保留。archive只归档此元数据单元，未改变前一单元独立封存。

- `task-1-report.md` SHA256 `2979fb322c96c26d1820479aedf8d7a417cb3be3cf31d30596f59b4ab043bf38`。
- `task-2-report.md` SHA256 `86bb9b8a59381c758937cd7e0bed7bd1a65bd4cdc507816f2e90dae9e0fcb8c7`。
- `task-3-report.md` SHA256 `92c9e0ddcb41ffcfa026a5879596c6aa36c2971ea69ef0fd228eeff6224e4e81`。
- `task-1-review.md` SHA256 `ca17f9f8390de7557dec44ba01d24fa8f36442ffb170577906de7a1020515196`。
- `task-2-review.md` SHA256 `be6a48bdb1cc31b03951f42a73bcc7dcbae12fd6d3ef655c578020bf9a887bb8`。
- `task-3-review.md` SHA256 `0e52840f3686cad64f005955990dd5210eeed3acd3efd9e14f22099e9b0ac0aa`。
- `final-review.md` SHA256 `120c2668026e67c0b9494edb005ea6808e2426ba43c940410d1f5ef66ac441a7`。
- `root-final-dispositions.md` SHA256 `98743853eeba8b0c38f20f458d0cd071ed8666050d671038000f43952bd49949`。
- `root-final-unit-source-verify.json` SHA256 `4e91e0ac6c701af5091a1f4d07caf7ea9ac8fed17339e5db2b0593e27480db08`。
- `ordered-controller-rulings.md` SHA256 `58a624d62de4ba902977437cc36a5791c2911257776f4ced1d23a3612ee82927`。

## 56项原始未认证边界与7项Minor的逐项处理

# Final review — every retained boundary and Root disposition

Full final review read through EOF. C0/I0/M7; no product correction recommended by reviewer. No declined line discarded.

## D1
原审查：Task-authenticated `CloseData` 的实际目标 gate 持久关闭：当前代码没有 runtime 调用/目标持久证据；这是下一物理协议的强制工作。
Root裁定：强制下一物理单元：原TaskClaim授权、exact同epoch目标持久关闭和签名回执；当前metadata绝不作为closed证据。

## D2
原审查：Task-authenticated `CloseAll` 的实际目标 gate 持久关闭：当前 metadata claim 不能替代完整目标关闭协议，当前测试也不执行它。
Root裁定：强制后续完整目标/管理关闭协议；与CloseData分开定义，不能从claim/checkpoint推导。

## D3
原审查：etcd operation prefix 的最终排空：destroy 原子关闭新准入但保留现有 token；当前没有 drain 完成实现或接受标准。
Root裁定：强制线性分页operations-prefix排空，保留既有token；不以一次empty或context取消猜测完毕。

## D4
原审查：目标端 accepted/execution 的排空：不是 etcd token 为空就能证明，当前没有测目标状态。
Root裁定：强制目标accepted/in-flight命令排空/精确隔离，覆盖close前已accepted却晚spawn者，不能只看etcd token。

## D5
原审查：final sync 的外部完成：checkpoint 只允许保守状态，不能据此判定后端数据同步完成。
Root裁定：强制final sync独立持久证据，保守checkpoint不提供完成权。

## D6
原审查：final flush 的外部完成：当前没有 durable flush evidence，未从 metadata receipt 推导。
Root裁定：强制final flush独立持久证据，区分缓存/远端结算/进程退出。

## D7
原审查：exact runtime termination / runtimeGone 身份：当前保留 runtime 引用，未执行或验证精确终止。
Root裁定：强制exact UID/container-incarnation/boot termination及发送端归因；无引用/本地cancel不是Gone。

## D8
原审查：remote settled 的远端结算：没有 End/remote settlement 实现，普通本地 Close/receipt 不能替代。
Root裁定：强制remote settled证明；未知PUT/DELETE/multipart保留owner，不把本地terminal当End。

## D9
原审查：remote fenced 的隔离证明：当前原 task Lease fencing 只保护元数据写，不能证明远端停止。
Root裁定：强制remote fenced证明或明确无远端effects证据；taskLease只保护元数据，不能隔离已发远端写。

## D10
原审查：最终条件 owner/fence/control/index 安全释放：本单元明确不删除这些点，尚需上述物理证据链。
Root裁定：强制原restore/control/owner/runtime/liveclaim/物理与remote证据条件释放；当前禁止delete。

## D11
原审查：最终 mount-policy / mount 资源清理：本单元没有该副作用或其条件安全证明。
Root裁定：强制policy/mount cleanup及安全条件；当前不授予这项副作用权。

## D12
原审查：publishing 失败恢复任务：active-only destroy 不覆盖未知外部创建效果，不能据 task API 接管。
Root裁定：强制publishing失败与未知创建效果的归因恢复；active-only手动destroy不得用于抢占。

## D13
原审查：exclusive 生命周期恢复：未接入本单元的 active→destroying 条件，属于后续生命周期工作。
Root裁定：强制exclusive恢复/新gate合法重开协议；未知effects未决不重新发布。

## D14
原审查：cleanup_pending 生命周期恢复：未在当前 API 实现，不能将保守 checkpoint 当已实现恢复。
Root裁定：强制cleanup_pending continuation/transition链；当前claim共同birth要求不能静默放宽。

## D15
原审查：TTL due 触发与 due index：当前支持手动 destroy，不含时间调度/发现准入。
Root裁定：强制due index/共享调度及expires_at版本CAS；手动destroy不是TTL驱动完成。

## D16
原审查：dirty index 与共享调度/背压：没有调度器接线；当前只验证实际领取任务的元数据能力。
Root裁定：强制dirty index、partition shared scheduler/背压，不增加每idle N常驻actor。

## D17
原审查：永久 task/intent/link/checkpoint 的安全 GC：当前保留历史是有意安全边界，未建立年龄删除或未决回收证明。
Root裁定：强制pending→terminal→fenced→collectable安全GC及字节预算；未决不按年龄删除。

## D18
原审查：pool/upload 等下游资源最终回收：当前无相关写入/副作用，不从元数据 ownership 推导完成。
Root裁定：强制pool/upload资源回收与原attempt归因，不能从owner记录推导外部完成。

## D19
原审查：生产 Manager/API/backend/images 接线：范围内未修改生产入口选择，当前结果不等于已启用新路径。
Root裁定：强制Manager/API/nativebackend/runtime/images接线；当前生产仍Redis，不假装已经启用。

## D20
原审查：老版本排空与 Redis 完全移除：master migration 仍进行中，本单元不能证明不再有旧读写者。
Root裁定：强制旧writer drain与全Redis/go-redis/bootstrap/chart依赖移除；单元通过不等于整迁移。

## D21
原审查：外集群拒绝的完整端到端路径：当前 identity/restore 场景和严格响应已检查，但仅存在第四 foreign member 不等于执行了该全路径测试。
Root裁定：当前foreign容器不是新端到端证明；保留foundation已验收cluster binding证据，未来生产/对应变动接口需真实foreign门禁。

## D22
原审查：生产 leader failover 恢复：当前 fault 注入与全失效 OOM 不等于系统性的切主验证。
Root裁定：保留foundation实际leader pause/election有限证明；生产failover另验，不用本次OOM充当切主。

## D23
原审查：网络分区后的系统恢复：当前没有隔离、愈合、重入的完整场景证明。
Root裁定：强制部署故障门禁的partition/heal/reentry；当前未知响应wrapper不认证系统恢复。

## D24
原审查：生产 TLS：isolated HTTP fixture 未提供加密连接配置及其实际验收。
Root裁定：强制生产etcd mTLS门禁，HTTP隔离fixture没有提供它。

## D25
原审查：生产 RBAC：当前 fixture 没有证明生产角色配置和访问隔离。
Root裁定：强制生产etcd RBAC/namespace/role隔离门禁，fixture存在不算权限验收。

## D26
原审查：恶意网络环境：响应校验的局部断言不等于完整威胁模型下认证与服务可用性证明。
Root裁定：当前只认证具体响应验证接口；生产认证/威胁/网络可用性门禁仍需独立执行。

## D27
原审查：生产数据目录权限安全：实际0755告警尚在，当前不能据隔离 fixture 证明生产权限正确。
Root裁定：强制生产数据目录权限与卷安全门禁；0755 fixture WARN保留，不认证生产正确。

## D28
原审查：具有原始 etcd 写权限的 Byzantine 管理者：strict records 与原 guard 比较不提供抵抗可信管理面伪造所有历史的保证；当前不引入这种保证。
Root裁定：当前假设有原始写权限管理面可信，不承诺抵御全历史伪造；生产RBAC必须约束writer，若扩大此威胁模型需新的authority设计。

## D29
原审查：exact server source-to-binary 对应：pinned digest/版本是身份线索，未证明完整源码构建 provenance；caller源码路径也不是证明。
Root裁定：固定digest与version观察仅限身份；完整source/binary provenance未认证，按需要的部署/恢复属性另验。

## D30
原审查：退役 Task1 成员的 storage-version 恢复：直接健康/read 成功不足，当前没有该成员后续恢复证据（F-M2）。
Root裁定：原Task1成员已退役无后续直接状态，保留启动ERROR且恢复未认证；新成员status不能代替。

## D31
原审查：首次 Task3 OOM 成员的 storage-version 恢复：后来的 fresh 成员不是原成员，原 schema ERROR 的恢复未被验证。
Root裁定：初始Task3 OOM成员已退役，schema ERROR/恢复未认证；fresh批次不能追认。

## D32
原审查：etcd upgrade 正确性：当前 restore identity 拒绝测试不执行版本升级。
Root裁定：强制所选部署版本升级和数据可读性门禁；restore UUID改变拒绝不等于upgrade验证。

## D33
原审查：etcd downgrade 正确性：当前没有降级和数据可读性证明。
Root裁定：若提供降级能力必须独立验证其数据与schema兼容；当前不承诺downgrade正确性。

## D34
原审查：snapshot 正确性：当前没有快照生成和内容校验。
Root裁定：强制生产snapshot生成/内容/restore测试；当前元数据snapshot reference不是etcd快照验收。

## D35
原审查：backup 正确性：当前没有备份完整性或可恢复性验收。
Root裁定：强制backup完整性/恢复演练；当前不声称备份正确。

## D36
原审查：disaster restore 正确性：restore identity fence 是拒绝陈旧权威的协议，不能替代完整灾难恢复演练。
Root裁定：强制灾难restore新authority身份安装、隔离旧ID/targets和数据恢复演练；fence拒绝只是其中一环。

## D37
原审查：未中断 full selector 成功：该 actual selector 已失败，三次 fresh batch 不认证这一属性（F-M5）。
Root裁定：初始完整selector保持失败；3fresh分批只证明各协议场景，不能称未中断整批成功。

## D38
原审查：长期内存稳定：没有连续运行的内存曲线或稳定性证据，fresh fixture 会重置状态。
Root裁定：强制持续负载与内存增长调查/N长稳门禁；fresh重置状态不提供长期稳定性。

## D39
原审查：OOM 根因与修复：三成员OOM137已确认，但当前既未定位产品/服务端/负载贡献，也未证明修复。
Root裁定：保留实际3OOM137；根因与修复未定位，不把分批作为修复或排除产品增长。

## D40
原审查：N=10k/100k/1M 的实际资源占用：源码未增加 per-idle actor 不等于实测这些规模。
Root裁定：强制master N矩阵/资源占用测试；无idle actor仅源码属性，不能推出10k/100k/1M容量。

## D41
原审查：规模化 QPS：有限 protocol selectors 不是吞吐 benchmark。
Root裁定：强制明确负载的QPS验收，有限协议case不是benchmark。

## D42
原审查：规模化 p99：当前没有明确负载下的延迟分布验收。
Root裁定：强制明确负载的p99分布验收，不声称速度提升。

## D43
原审查：规模化恢复 SLO：未测试大规模失效/恢复时间和积压消退。
Root裁定：强制大规模恢复SLO/队列积压消退验证，当前没有该数据。

## D44
原审查：Linux 应用 race：实际 Go race 应用在 Darwin，Linux etcd 服务端不是 Linux 应用证明。
Root裁定：实际Darwin race仅宿主；需要Linux应用race对应平台门禁，Linuxetcd不是应用race。

## D45
原审查：PID1 行为：当前没有 launcher/PID1新执行，已有平台门禁不在本次重放。
Root裁定：保留tracked authenticated-target-execution实际PID1证据；新的Task关闭/排空须Linux native changed-seam门禁，不重复无变动旧suite。

## D46
原审查：FUSE 行为：本单元无挂载或FUSE执行，不能以metadata结果证明文件系统行为。
Root裁定：强制FUSE新authority/flush/远端结算/失租语义与固定idle成本门禁；metadata不是文件系统证明。

## D47
原审查：继承 Stage 全部 malformed/unknown/cancel 历史：只检查当前使用接口与新增真实场景，没有重审/重放全部基础原语历史；当前没有修改其实现。
Root裁定：继承Stage/receipt/mutation源码exactBASE未改，已有stage-verification实际仲裁证据继续有效；当前changed-seam不扩成穷尽重认证。

## D48
原审查：原 Stage guard ModRevision 成功 CAS：Ruling7明确保留value/Lease/Create的原协议，新task guard四字段测试不认证这项原Stage加固。
Root裁定：R7继承原Stage成功value/Lease/Create无Mod，不把TaskGuard四字段当基础Stage加固；加强须独立review原语/调用点。

## D49
原审查：原 Stage local monotonic deadline：Ruling7明确继承服务器Lease协议，不能把TaskClaim的新deadline说成原Stage的新保证。
Root裁定：R7继承原Stage serverLease无localdeadline；TaskClaim grant/renew deadline不改变这个基础原语边界。

## D50
原审查：继承 codec 的全部旧调用场景：检查了本单元strict schema/atomic assignment接口与新增用例，未重审所有旧caller。
Root裁定：保留严格codec的accepted domain-model/foundation证据，当前只检查新schema调用，不重复旧caller全量。

## D51
原审查：继承 mutation budget 的全部旧操作形状：当前43/59和预检路径已判断，未重审所有旧预算组合。
Root裁定：保留accepted mutation budget基础证据，当前43/59预检/negative测试只认证这些形状；变更基础预算需对应门禁。

## D52
原审查：backend identity provisioning 全流程：现有read fence接口已检查，未在本次认证整个初始化/管理过程。
Root裁定：继承accepted client/domain/acquisition identity基础与未改源码；当前精确read接口已查，生产初始化全流程仍需门禁。

## D53
原审查：restore administration 全流程：当前检测restore fence变化，未验证管理员恢复程序的所有行为。
Root裁定：强制restore管理员流程部署演练，当前仅检测原restore fence，不认证管理全流程。

## D54
原审查：继承 request-budget machinery 全部边界：当前使用其bounded context接口，未重审整个共享预算实现。
Root裁定：继承未改requestContext预算实现，当前active calls使用bounded contexts；不承诺抢占不合作provider，改此原语需对应测试。

## D55
原审查：整个 migration branch 的最终仓库回归：本次只审新unit并消费现有scoped输出，未重跑全仓suite；Root仍需完成计划中的最终集成检查。
Root裁定：整个迁移接线完成时执行最终全仓/集成回归；此metadata单元已消费changed-seam测试和源码冻结，无无变动suite重放。

## D56
原审查：发布、合并与部署后的实际运行：本次没有push/merge/deploy，metadata gate通过不能代替最终交付与生产验收。
Root裁定：未push/merge/deploy；发布前仍需相应批准与实际部署验收，当前只本地feature branch小提交。

## F-M1
Root裁定：Task1 canceled2WARN归因已核对；非阻塞日志分类观察，保留原始bytes，不抑制额外错误。

## F-M2
Root裁定：Task1 startup18WARN+1ERROR/原成员恢复仍未认证；保留未解决恢复属性，部署相关门禁强制。

## F-M3
Root裁定：Task2重复原Lease revoke3client+9apply是已核对cleanup负例；保留分类不改产品清理语义。

## F-M4
Root裁定：Task2 configuration16/schema1WARN与fresh status分别记载；未提供生产安全/旧成员恢复证明。

## F-M5
Root裁定：Task3 full OOM真实失败/原因未解释；R8批次只有协议覆盖，持续容量调查强制，不宣布修复。

## F-M6
Root裁定：Task3 B/C11client+33apply expectedNotFound分类保留；失败full另5网络/超时WARN不能同归预期。

## F-M7
Root裁定：Task3三批48baselineWARN非pristine；fixturehygiene/生产permission/port/auth另验，不扩当前产品范围。

