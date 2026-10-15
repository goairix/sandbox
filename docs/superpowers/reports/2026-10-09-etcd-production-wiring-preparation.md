# etcd生产接线缺口图

2026-10-15补充：基分支新增Kubernetes LoadWorkspaceControlKey，main将Redis Store传入，以永久SetNX/Get保存scope级probe签名seed。它是fencing历史，生产etcd接线须明确持久authority/协议替代与旧版本排空；下文早期“未发现credential必须依赖Redis”的研究结论不覆盖这项新增。见2026-10-15-base-fuse-fixes-merge-verification.md。

本线程probe密钥etcd方案已交付原生准备接口：226b543/dc08b41及验收2026-10-15-etcd-probe-control-key-verification.md。operator显式32字节seed、永久无Lease/TTL、当前identity/restore屏障下CAS，丢ACK只读仲裁，API只读owned key加载，禁止失败后生成替代。main仍用旧Redis loader，启动内存安装、原seed迁移/公钥校验、operator初始化与API只读身份RBAC/TLS、磁盘备份保护及轮换/旧runtime排空尚待接线。Pod仍只接收公钥，不随已有sandbox数量逐个读私钥；不能删除Redis时丢弃签名历史。

这是fresh `etcd_production_wiring_gap_map`对master spec及当前业务代码的只读研究，未实施、未测试，不是生产验收。Root抽查LoadOwner/LoadControl/LoadSnapshot签名、distributedStateEnabled和全量协调器/FUSE检查接点。当前main仍Redis；不能用通用etcd.Store包装保留旧循环来宣称完成迁移。

## 已有能力与真实接点

- `cmd/sandbox/main.go`仍构造Active、ordinary pool、WorkspaceCoordinator、FUSE、Session/Ephemeral、Multipart等Redis仓库；Backend未实现旧Store/AtomicStore/ActiveSandboxRepository/FUSEPoolRepository。永久owner不能照搬旧TTL删除和Keys扫描。
- CreateSandbox→Manager.GetOrCreate/Create可消费AcquireIntent、CreationClaim、DeclareRuntimeDispatch、BindRuntime/ConsumeRuntimeMount、PublishRuntime及ResolveStage；Manager尚未消费，真实driver的声明/bootstrap/证书/发布证明/可信Destination组合仍缺。
- GetByWorkspace/Get所需原生ReadWorkspaceView/ReadSandboxView/ConfirmSandboxView已完成并验收，提供同revision owner/fence/placement/control与只读MOD复核；manager尚未消费。仍缺健康/期限准入组合、snapshot→Sandbox严格编解码和有界LRU。PrepareDestroy可用view.ControlRevision，不能让manager拼裸key，也不能将metadata Confirm当健康/副作用许可。
- Exec/ExecStream可复用BeginOperation/RenewOperation、PrepareExecEffect/DeliverExecEffect/RenewExecDelivery；缺真实driver Destination与结果/SSE映射。CancelOperationCapability仅撤销metadata，不是物理结束。
- Upload/Download/Read/Glob/List/Edit等文件公共方法尚走旧acquireSandboxOperation，不能因Exec delivery已有便认为覆盖。TTL/network缺capability下领域更新、TTL+due原子重排和真实副作用回执。Mount/Unmount/Sync缺exclusive关闭/排空/修改/epoch/reopen组合入口。
- Destroy应接PrepareDestroy/CommitStage/ResolveStage及持久TaskClaim/Checkpoint，不能为idle sandbox保留controller。现有ObserveTaskOperationsPage要求真实TaskClaim，不是任意计数API。安全完成/owner释放仍缺。
- 两池缺原生PoolAllocator：有限候选索引/CAS、prepare slot、真实runtime身份、与creation publication组合事务；runtime preparation不等于allocation。Docker旧分布式入口硬限定Kubernetes，需同一领域路径和daemon身份恢复。Multipart旧Get→副作用→Set+24hTTL需独立control/chunk摘要/CAS/期限/终态abort，暂存数据不进etcd。

## 必须移出的短周期N成本

`lifecycle_coordinator.go:runActiveLifecycleCoordinator/reconcileActiveLifecycleControllers`完整扫Active并为每workspace续controller；`workspace_lease.go:StartRenewal/startRenewal`每workspace ticker；`manager.go:startFUSEWatcher/checkFUSELifecycle`每FUSE约5秒操作/runtime健康读取；`reapExpiredSandboxes/reapOnce`约10秒遍历全部本地sandbox；autoSync系列遍历session/lifecycle；`guardFUSEPoolRecord`跨Session/Ephemeral/owner扫描；ordinary_pool_shared及fuse_pool reconcile/acquire扫库存；`workspace_lookup.go:lookupHistoricalWorkspace`Active扫描fallback；`workspace_acquire.go:waitWorkspaceRequest`固定50ms争用重试。

接点为领域事务内due/dirty、runtime反向索引点查、helper→collector→partition dirty、shared Watch hub、bounded singleflight、partition lease/relist/worker和有限bucket候选。当前task/link/checkpoint已有，不等于due/dirty/scheduler已实现。长周期分页审计和迁移drain可保留，不能作为每API短周期协调器。

## 四个完整功能单元

1. 原生业务读与请求契约：带revision coherent读/复用确认、snapshot/LRU、principal+Idempotency-Key、intent/request查询及错误/Retry-After映射。验证跨API复用同ID不改变TTL/config、冲突409、unknown可查询、cache不授权及Destroy可获原revision。
2. 创建与Pool/Docker/Upload领域组合：无workspace生命周期、PoolAllocator/slots、daemon pin、UploadRepository，接Acquire/Dispatch/Bind/Publish。验证并发不双分配/超额、响应丢失不重建、daemon错配拒绝、分片重试/abort归属。
3. Operation/mutation生产路径：Exec可信Destination、全部文件/stream gate、TTL/network领域更新、workspace exclusive。验证失租/迟到命令不能越gate、stream持有到物理结束、TTL不能复活过期对象。
4. 有界worker与最终切换：due/dirty/Watch/compaction relist/partition lease，接安全终态和helper/collector，移除扫描/idle续租，再main/部署/Redis移除。验证重启续接、compaction不漏工作、活跃率不变时idle N×10不导致短周期请求×10、两真实runtime和迁移drain。

前3项接口/独立事务及partition work source可在安全释放前准备；不启用新allocator、删除旧保护、自动清理或切main，直到安全释放和对应真实driver链路完成验收。

2026-10-15当前业务读接点核对：`Manager.Get`仍调用旧acquireSandboxOperation，`GetByWorkspace`仍经owner/lease与lookupHistoricalWorkspace回退，不可仅替换底层client后保留这些扫描/旧准入。`marshalActiveSandbox`/`decodeActiveSandboxPhase`持久化完整旧Sandbox JSON；native snapshot目前只是有界、digest校验的任意JSON，尚无严格业务codec与有界payload LRU。后续cache可按单Backend实例+snapshot版本/digest持有不可变payload，不缓存owner/健康/期限授权，不允许失联时用cache放行。现有`runtime.SandboxInfo`只含ID/UID/State等，没有BootID；旧runtime.GetSandbox成功不能替代新协议的boot/certificate/健康观测。业务复用仍需受控时钟、当前可信健康和最后metadata Confirm的组合，不能把metadata view或nil error当准入证明。本次仅源码核对，未新增业务实现或测试。

本次业务用途研究未发现credential/cache/API auth/rate-limit必须依赖Redis：FUSE credentials来自受控文件/内存、API bearer来自配置、ratelimit token bucket在内存；state管理token应变成原生capability。最终删除旧双份Session/Ephemeral、WorkspaceLease、generic pool、Multipart调用和main/config依赖，旧镜像负责drain，新程序明确拒绝旧Redis配置。上述仍需实际实施和单环境验收。
